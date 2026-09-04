// System audio capture via ScreenCaptureKit.
//
// Written as real Objective-C rather than objc_msgSend calls from cgo (the
// style used elsewhere in this project) because SCShareableContent's API is
// block-based, and hand-rolling Objective-C blocks from C is far worse than
// just compiling a .m file -- which cgo does automatically for any .m in
// the package directory.
//
// Audio only: no windows or displays are actually captured, but macOS still
// classifies this as screen capture, so it needs Screen Recording
// permission (see permissions.go).

#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <AVFoundation/AVFoundation.h>
#import <CoreAudio/CoreAudio.h>

// Implemented in Go (tap_darwin.go).
extern void goSystemAudioChunk(float *samples, int count);

// Voxlog feeds the recognizer 16 kHz mono; ScreenCaptureKit hands over the
// system mix at 48 kHz, so every buffer is downsampled on the way through.
static const double kTargetRate = 16000.0;

// A stream the system killed is restarted; these bound how hard we try, so a
// tap that cannot survive at all does not spin for the length of a call.
static const int kMaxRestarts = 30;
static const int64_t kRestartDelayNSec = 1 * NSEC_PER_SEC;

static dispatch_queue_t controlQueue(void);
API_AVAILABLE(macos(13.0))
static int voxlogSystemAudioStartLocked(void);
// Both guarded by controlQueue. gWantRunning is the Go side's intent: a
// restart scheduled before a Stop must not resurrect the stream after it.
static int gRestarts = 0;
static BOOL gWantRunning = NO;

API_AVAILABLE(macos(13.0))
@interface VoxlogAudioTap : NSObject <SCStreamOutput, SCStreamDelegate>
@property(nonatomic, strong) SCStream *stream;
// Set the moment a stop is requested. ScreenCaptureKit can still deliver a
// buffer or two after stopCapture is called, and those must not be pushed
// into Go once the recording is considered finished.
@property(atomic, assign) BOOL stopped;
@end

// The one live tap. Only ever read or written on controlQueue -- see below.
API_AVAILABLE(macos(13.0))
static VoxlogAudioTap *gTap = nil;

@implementation VoxlogAudioTap

- (void)stream:(SCStream *)stream
    didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
                   ofType:(SCStreamOutputType)type {
  if (self.stopped || type != SCStreamOutputTypeAudio ||
      !CMSampleBufferIsValid(sampleBuffer)) {
    return;
  }

  const AudioStreamBasicDescription *asbd =
      CMAudioFormatDescriptionGetStreamBasicDescription(
          CMSampleBufferGetFormatDescription(sampleBuffer));
  if (asbd == NULL) {
    return;
  }

  // Two-step, as the API documents: ask how big the AudioBufferList has to
  // be, then allocate exactly that. Guessing a fixed size -- even a
  // generous one -- kept failing with kCMSampleBufferError_ArrayTooSmall
  // (-12737), because the required size depends on the buffer's own layout,
  // not just on how many channels were requested.
  size_t ablSize = 0;
  OSStatus sizeStatus = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
      sampleBuffer, &ablSize, NULL, 0, NULL, NULL,
      kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment, NULL);
  if (sizeStatus != noErr || ablSize == 0) {
    static BOOL warnedSize = NO;
    if (!warnedSize) {
      warnedSize = YES;
      NSLog(@"voxlog: could not size system audio buffer (status %d)", (int)sizeStatus);
    }
    return;
  }

  AudioBufferList *abl = (AudioBufferList *)malloc(ablSize);
  if (abl == NULL) {
    return;
  }

  CMBlockBufferRef blockBuffer = NULL;
  OSStatus status = CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
      sampleBuffer, NULL, abl, ablSize, NULL, NULL,
      kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment, &blockBuffer);
  if (status != noErr || abl->mNumberBuffers == 0) {
    static BOOL warned = NO;
    if (!warned) {
      warned = YES; // once, not once per buffer
      NSLog(@"voxlog: could not read system audio buffer (status %d)", (int)status);
    }
    free(abl);
    if (blockBuffer) CFRelease(blockBuffer);
    return;
  }

  // ScreenCaptureKit delivers non-interleaved float32; buffer 0 is the left
  // (or only) channel, which is all a speech recognizer needs.
  const float *src = (const float *)abl->mBuffers[0].mData;
  int srcCount = (int)(abl->mBuffers[0].mDataByteSize / sizeof(float));
  if (src == NULL || srcCount <= 0) {
    free(abl);
    if (blockBuffer) CFRelease(blockBuffer);
    return;
  }

  // Nearest-sample decimation. Crude next to a filtered resampler, and it
  // does alias -- but this feeds a 16 kHz speech model whose own front end
  // discards everything above 8 kHz anyway.
  // ponytail: swap in AVAudioConverter if aliasing ever hurts accuracy.
  double ratio = asbd->mSampleRate / kTargetRate;
  if (ratio < 1.0) ratio = 1.0;
  int outCount = (int)(srcCount / ratio);
  if (outCount > 0) {
    float *out = (float *)malloc(sizeof(float) * (size_t)outCount);
    if (out != NULL) {
      for (int i = 0; i < outCount; i++) {
        out[i] = src[(int)(i * ratio)];
      }
      goSystemAudioChunk(out, outCount);
      free(out);
    }
  }

  free(abl);
  if (blockBuffer) CFRelease(blockBuffer);
}

// The system stops the stream on its own -- error -3821, "Stream was stopped
// by the system" -- when the audio hardware changes underneath it, which a
// Bluetooth headset flipping between its music and headset profiles does
// several times in a normal call. Left alone, that silently ends the capture:
// a 50-minute meeting kept the microphone but lost the other side after 71
// seconds, with nothing but a log line to say so. So: start a new stream.
- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
  NSLog(@"voxlog: system audio stream stopped: %@", error);
  if (self.stopped) {
    return; // a stop we asked for
  }
  dispatch_after(dispatch_time(DISPATCH_TIME_NOW, kRestartDelayNSec),
                 controlQueue(), ^{
                   if (gTap != self || !gWantRunning) {
                     return; // a Stop, or a newer stream, got there first
                   }
                   if (gRestarts >= kMaxRestarts) {
                     NSLog(@"voxlog: giving up on system audio after %d restarts",
                           gRestarts);
                     return;
                   }
                   gRestarts++;
                   gTap = nil;
                   self.stopped = YES;
                   if (@available(macOS 13.0, *)) {
                     int rc = voxlogSystemAudioStartLocked();
                     NSLog(@"voxlog: system audio restart #%d -> %d", gRestarts, rc);
                   }
                 });
}

@end

// Every read and write of gTap happens on this serial queue. Start and Stop
// are driven from Go goroutines while ScreenCaptureKit tears a stream down
// asynchronously in the background, and letting those overlap on a bare
// global is what turned a stop-then-start into a use-after-free crash.
static dispatch_queue_t controlQueue(void) {
  static dispatch_queue_t q;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    q = dispatch_queue_create("com.voxlog.sysaudio.control", DISPATCH_QUEUE_SERIAL);
  });
  return q;
}

// voxlogSystemAudioStart begins capturing the system audio mix. Returns 0 on
// success, non-zero on failure (unsupported OS, permission denied, no
// display available). Blocking: SCShareableContent is async, so this waits
// on a semaphore to give the Go side an ordinary synchronous call.
API_AVAILABLE(macos(13.0))
static int voxlogSystemAudioStartLocked(void);

// stopLocked tears the current stream down without touching the restart
// budget. Runs on controlQueue.
API_AVAILABLE(macos(13.0))
static void voxlogSystemAudioStopLocked(void) {
  VoxlogAudioTap *tap = gTap;
  if (tap == nil) {
    return;
  }
  gTap = nil;
  tap.stopped = YES; // no more buffers routed to Go from this point

  // The completion block captures `tap` strongly on purpose: it keeps the
  // delegate alive until ScreenCaptureKit has finished tearing the stream
  // down. Releasing it any earlier leaves SCK holding a dead delegate, which
  // is what crashed the next start.
  [tap.stream stopCaptureWithCompletionHandler:^(NSError *e) {
    (void)e;
    (void)tap;
  }];
}

static const AudioObjectPropertyAddress kDefaultOutputAddress = {
    kAudioHardwarePropertyDefaultOutputDevice, kAudioObjectPropertyScopeGlobal,
    kAudioObjectPropertyElementMain};

// defaultOutputChanged restarts the tap when the machine's output device
// changes under it.
//
// This is the other half of the didStopWithError restart, and the half that
// loses audio silently: an SCStream bound to a device that goes away does not
// always fail, it just delivers digital silence -- 57 seconds of zeros in the
// recording that prompted this, before the system finally killed the stream.
// A Bluetooth headset switching profile counts as a device change, so this
// fires in the middle of exactly the calls a meeting recording is for.
static OSStatus defaultOutputChanged(AudioObjectID object, UInt32 count,
                                     const AudioObjectPropertyAddress *addresses,
                                     void *context) {
  (void)object;
  (void)count;
  (void)addresses;
  (void)context;
  if (@available(macOS 13.0, *)) {
    dispatch_async(controlQueue(), ^{
      if (gTap == nil || !gWantRunning || gRestarts >= kMaxRestarts) {
        return; // nothing running, or the budget is spent
      }
      gRestarts++;
      voxlogSystemAudioStopLocked();
      NSLog(@"voxlog: output device changed; restarting system audio (#%d)",
            gRestarts);
      // Let CoreAudio and SCK settle before asking for a new stream: a start
      // issued into the middle of a device switch fails outright.
      dispatch_after(dispatch_time(DISPATCH_TIME_NOW, kRestartDelayNSec),
                     controlQueue(), ^{
                       if (gTap != nil || !gWantRunning) {
                         return; // stopped meanwhile, or already restarted
                       }
                       int rc = voxlogSystemAudioStartLocked();
                       if (rc != 0) {
                         NSLog(@"voxlog: system audio restart failed (%d)", rc);
                       }
                     });
    });
  }
  return noErr;
}

// Registered once for the life of the process; the callback no-ops when no
// capture is running, which is cheaper than adding and removing a listener
// around every recording.
static void watchDefaultOutput(void) {
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    OSStatus err = AudioObjectAddPropertyListener(
        kAudioObjectSystemObject, &kDefaultOutputAddress, defaultOutputChanged,
        NULL);
    if (err != noErr) {
      NSLog(@"voxlog: cannot watch the default output device (%d)", (int)err);
    }
  });
}

int voxlogSystemAudioStart(void) {
  if (@available(macOS 13.0, *)) {
    __block int result = 0;
    watchDefaultOutput();
    dispatch_sync(controlQueue(), ^{
      gRestarts = 0; // a fresh recording gets the full restart budget
      gWantRunning = YES;
      result = voxlogSystemAudioStartLocked();
      if (result != 0) {
        gWantRunning = NO;
      }
    });
    return result;
  }
  return 6; // needs macOS 13+
}

// Runs on controlQueue; see voxlogSystemAudioStart.
API_AVAILABLE(macos(13.0))
static int voxlogSystemAudioStartLocked(void) {
  {
    if (gTap != nil) {
      return 0; // already running
    }

    __block SCShareableContent *content = nil;
    __block NSError *contentError = nil;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [SCShareableContent
        getShareableContentExcludingDesktopWindows:NO
                               onScreenWindowsOnly:NO
                                 completionHandler:^(SCShareableContent *c, NSError *e) {
                                   content = c;
                                   contentError = e;
                                   dispatch_semaphore_signal(sem);
                                 }];
    // A denied Screen Recording permission surfaces here as an error rather
    // than a prompt-and-wait, so a bounded wait is enough.
    if (dispatch_semaphore_wait(
            sem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0) {
      return 1;
    }
    if (contentError != nil || content == nil || content.displays.count == 0) {
      return 2;
    }

    // A content filter is required even for audio-only capture, so it names
    // a display whose video output is then made as cheap as possible below.
    SCDisplay *display = content.displays.firstObject;
    SCContentFilter *filter =
        [[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]];

    SCStreamConfiguration *config = [[SCStreamConfiguration alloc] init];
    config.capturesAudio = YES;
    config.excludesCurrentProcessAudio = YES; // don't capture our own output
    config.sampleRate = 48000;
    // Mono: the recognizer is mono anyway, and asking for one channel means
    // one AudioBuffer to unpack instead of one per channel.
    config.channelCount = 1;
    // Video can't be disabled outright; make it as close to free as
    // possible -- a 2x2 frame is essentially nothing to encode.
    config.width = 2;
    config.height = 2;
    // 1 fps rather than one frame per 10 minutes: some SCStream builds
    // stall the whole stream (audio included) on an extreme interval.
    config.minimumFrameInterval = CMTimeMake(1, 1);

    gTap = [[VoxlogAudioTap alloc] init];
    gTap.stream = [[SCStream alloc] initWithFilter:filter
                                     configuration:config
                                          delegate:gTap];

    NSError *addError = nil;
    if (![gTap.stream addStreamOutput:gTap
                                 type:SCStreamOutputTypeAudio
                   sampleHandlerQueue:dispatch_queue_create("com.voxlog.sysaudio", NULL)
                                error:&addError]) {
      gTap = nil;
      return 3;
    }

    __block NSError *startError = nil;
    dispatch_semaphore_t startSem = dispatch_semaphore_create(0);
    [gTap.stream startCaptureWithCompletionHandler:^(NSError *e) {
      startError = e;
      dispatch_semaphore_signal(startSem);
    }];
    if (dispatch_semaphore_wait(
            startSem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0) {
      gTap = nil;
      return 4;
    }
    if (startError != nil) {
      gTap = nil;
      return 5;
    }
    return 0;
  }
}

void voxlogSystemAudioStop(void) {
  if (@available(macOS 13.0, *)) {
    dispatch_sync(controlQueue(), ^{
      gWantRunning = NO; // a restart already in flight must not resurrect it
      voxlogSystemAudioStopLocked();
    });
  }
}
