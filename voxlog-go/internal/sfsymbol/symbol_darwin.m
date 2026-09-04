// SF Symbols rendered to PNG, for the menu bar and its menu.
//
// systray takes item and status images as PNG bytes, so the choice was
// between shipping hand-drawn bitmaps for every state (four more files
// through tools/mkicons, and a diagonal slash the rect rasterizer there
// cannot even draw) and asking the system for the glyph it already has. The
// symbols are the ones every other menu bar app uses, at the weight and
// optical size macOS picked for them.

#import <AppKit/AppKit.h>
#include <stdlib.h>
#include <string.h>

// voxlogSymbolPNG renders the named SF Symbol at the given point size and
// returns freshly malloc'd PNG bytes (the caller frees). tinted != 0 fills the
// glyph with the given colour; tinted == 0 leaves it black, which is what a
// template image wants -- macOS then recolours it for the menu bar's own
// appearance, light or dark.
//
// Returns NULL (and *outLen == 0) for a symbol this macOS does not have, so
// the caller can fall back rather than show an empty slot.
void *voxlogSymbolPNG(const char *name, double points, int r, int g, int b,
                      int tinted, int *outLen) {
  *outLen = 0;
  @autoreleasepool {
    NSImage *image =
        [NSImage imageWithSystemSymbolName:[NSString stringWithUTF8String:name]
                  accessibilityDescription:nil];
    if (image == nil) {
      return NULL;
    }
    NSImageSymbolConfiguration *config =
        [NSImageSymbolConfiguration configurationWithPointSize:points
                                                        weight:NSFontWeightRegular];
    image = [image imageWithSymbolConfiguration:config];

    NSSize size = image.size;
    // 2x, so the same PNG is sharp on a Retina menu bar; rep.size stays in
    // points, which is what tells AppKit it is a @2x bitmap.
    NSBitmapImageRep *rep = [[NSBitmapImageRep alloc]
        initWithBitmapDataPlanes:NULL
                      pixelsWide:(NSInteger)ceil(size.width * 2)
                      pixelsHigh:(NSInteger)ceil(size.height * 2)
                   bitsPerSample:8
                 samplesPerPixel:4
                        hasAlpha:YES
                        isPlanar:NO
                  colorSpaceName:NSDeviceRGBColorSpace
                     bytesPerRow:0
                    bitsPerPixel:0];
    rep.size = size;

    [NSGraphicsContext saveGraphicsState];
    [NSGraphicsContext
        setCurrentContext:[NSGraphicsContext graphicsContextWithBitmapImageRep:rep]];
    NSRect rect = NSMakeRect(0, 0, size.width, size.height);
    [image drawInRect:rect];
    if (tinted != 0) {
      // SourceIn keeps the glyph's alpha and replaces its colour, which is
      // the only way to colour a symbol image without losing its antialiasing.
      [[NSColor colorWithSRGBRed:r / 255.0 green:g / 255.0 blue:b / 255.0 alpha:1.0] set];
      NSRectFillUsingOperation(rect, NSCompositingOperationSourceIn);
    }
    [NSGraphicsContext restoreGraphicsState];

    NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG
                                    properties:@{}];
    if (png.length == 0) {
      return NULL;
    }
    void *out = malloc(png.length);
    if (out == NULL) {
      return NULL;
    }
    memcpy(out, png.bytes, png.length);
    *outLen = (int)png.length;
    return out;
  }
}
