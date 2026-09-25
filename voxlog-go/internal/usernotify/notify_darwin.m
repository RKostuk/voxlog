// User notifications via UNUserNotificationCenter.
//
// The point of doing this rather than shelling out to `osascript -e "display
// notification"` is the click: an osascript banner belongs to osascript, so
// clicking it activates that helper (in practice: a Finder window) and there
// is no way to route the click back here. A notification posted by the app
// itself calls this delegate, which hands the action string to Go so the main
// window can open on the right pane.

#import <Foundation/Foundation.h>
#import <UserNotifications/UserNotifications.h>

// Implemented in Go (notify_darwin.go).
extern void goNotificationAction(char *action);
extern void goNotifyAuthDecided(int granted);

API_AVAILABLE(macos(10.14))
@interface VoxlogNotifyDelegate : NSObject <UNUserNotificationCenterDelegate>
@end

@implementation VoxlogNotifyDelegate

// Without this, a notification posted while Voxlog is the frontmost app is
// swallowed -- and Voxlog is frontmost exactly when the History window is
// open, which is when a transcript finishing matters most.
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
       willPresentNotification:(UNNotification *)notification
         withCompletionHandler:(void (^)(UNNotificationPresentationOptions))completionHandler {
  completionHandler(UNNotificationPresentationOptionBanner |
                    UNNotificationPresentationOptionList);
}

- (void)userNotificationCenter:(UNUserNotificationCenter *)center
    didReceiveNotificationResponse:(UNNotificationResponse *)response
             withCompletionHandler:(void (^)(void))completionHandler {
  // Only a plain click on the banner counts; dismissing it is not a request
  // to open anything.
  if ([response.actionIdentifier isEqualToString:UNNotificationDefaultActionIdentifier]) {
    NSString *action = response.notification.request.content.userInfo[@"action"];
    if (action.length > 0) {
      goNotificationAction((char *)action.UTF8String);
    }
  }
  completionHandler();
}

@end

static id gDelegate = nil;
// 0 until the authorization request comes back granted. Written on the main
// thread, read from whichever goroutine is posting.
static volatile int gAuthorized = 0;
// 0 until the completion handler has run at all -- granted or not. Go uses
// this (via goNotifyAuthDecided) to hold a notification fired during the
// split-second before the answer is known, instead of routing it into the
// osascript fallback purely because gAuthorized still happened to read 0.
static volatile int gDecided = 0;
// 1 once an authorization request has actually been issued, so an answer --
// granted or refused -- is coming. 0 means nobody will ever call the
// completion handler: no app bundle, or no notification centre to ask. Go
// uses this to tell "the user has not answered the prompt yet" from "there is
// nothing to wait for", which decide whether a queued banner is worth holding.
static volatile int gWillDecide = 0;

// voxlogNotifyInit asks for permission, on the main thread and without
// waiting for the answer.
//
// Main thread because that is where AppKit's own notification plumbing
// lives, and non-blocking because the prompt sits on screen until the user
// answers it -- the caller is the goroutine that sets up the menu bar, and it
// has a menu to finish building.
void voxlogNotifyInit(void) {
  if (@available(macOS 10.14, *)) {
    // A process that is not an app bundle has no notification centre at all,
    // and asking raises rather than returns -- that is every `go test` and
    // every `go run` of this package.
    if ([[NSBundle mainBundle] bundleIdentifier] == nil) {
      NSLog(@"voxlog: not running from an app bundle; no notification centre");
      return;
    }
    dispatch_async(dispatch_get_main_queue(), ^{
      UNUserNotificationCenter *center = nil;
      @try {
        center = [UNUserNotificationCenter currentNotificationCenter];
      } @catch (NSException *e) {
        NSLog(@"voxlog: no notification centre: %@", e);
        return;
      }
      if (center == nil) {
        return;
      }
      gDelegate = [[VoxlogNotifyDelegate alloc] init];
      center.delegate = gDelegate;
      // Set before asking, not after: the answer can arrive on another
      // thread before this line would otherwise run.
      gWillDecide = 1;
      // Sound is requested alongside Alert because voxlogNotifyPost sets
      // content.sound; without the option the system drops the sound and
      // keeps the banner.
      [center requestAuthorizationWithOptions:(UNAuthorizationOptionAlert |
                                               UNAuthorizationOptionSound)
                            completionHandler:^(BOOL granted, NSError *e) {
                              if (!granted) {
                                NSLog(@"voxlog: notifications not permitted "
                                      @"(%@); falling back to osascript banners",
                                      e);
                              } else {
                                NSLog(@"voxlog: notifications enabled");
                                gAuthorized = 1;
                              }
                              gDecided = 1;
                              goNotifyAuthDecided(granted ? 1 : 0);
                            }];
    });
  }
}

// voxlogNotifyWillDecide reports whether an authorization answer is on its
// way. Read from Go while deciding how long to hold a queued banner.
int voxlogNotifyWillDecide(void) { return gWillDecide; }

// voxlogNotifyPost returns 1 if the notification was handed to the system, 0
// if the caller should fall back. 0 covers the window before the user has
// answered the permission prompt as well as an outright refusal.
int voxlogNotifyPost(const char *message, const char *action) {
  if (@available(macOS 10.14, *)) {
    if (!gAuthorized) {
      return 0;
    }
    UNMutableNotificationContent *content =
        [[UNMutableNotificationContent alloc] init];
    content.title = @"Voxlog";
    content.body = [NSString stringWithUTF8String:message];
    // A reminder that arrives silently is a reminder that gets missed, and
    // grouping them under one thread keeps a burst of them from filling
    // Notification Center with unrelated-looking rows.
    content.sound = [UNNotificationSound defaultSound];
    if (action != NULL && action[0] != '\0') {
      NSString *actionStr = [NSString stringWithUTF8String:action];
      content.userInfo = @{@"action" : actionStr};
      content.threadIdentifier = actionStr;
    } else {
      content.threadIdentifier = @"voxlog";
    }
    // nil trigger: deliver now.
    UNNotificationRequest *request =
        [UNNotificationRequest requestWithIdentifier:[[NSUUID UUID] UUIDString]
                                             content:content
                                             trigger:nil];
    [[UNUserNotificationCenter currentNotificationCenter]
        addNotificationRequest:request
         withCompletionHandler:^(NSError *e) {
           if (e != nil) {
             NSLog(@"voxlog: could not post notification: %@", e);
           }
         }];
    return 1;
  }
  return 0;
}
