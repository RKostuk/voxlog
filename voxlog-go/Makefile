# Built one directory up, at a single fixed location. Two copies of the
# bundle at different paths made macOS track TCC permissions against
# whichever one it saw first, so a grant on one silently did nothing for
# the other.
APP := ../Voxlog.app
ZIP := ../Voxlog.zip
BIN := voxlog-go

# A STABLE signing identity, not ad-hoc ("-"). macOS keys TCC permissions
# (Accessibility, Screen Recording) to the app's code signature, and an
# ad-hoc signature changes on every build -- so every rebuild looked like a
# brand-new app and silently dropped every permission the user had granted.
# Falls back to ad-hoc if the certificate isn't present on this machine.
#
# The fallback has to test the RESULT, not the exit status: the pipeline ends
# in `tr`, which succeeds even when there is no identity to print, so an
# `|| echo -` here never fired and the build died on `codesign --sign ""`.
SIGN_ID ?= $(shell id=$$(security find-identity -v -p codesigning 2>/dev/null | \
             grep -o '"[^"]*"' | head -1 | tr -d '"'); \
             if [ -n "$$id" ]; then echo "$$id"; else echo -; fi)

# sherpa-onnx ships prebuilt dylibs inside its Go module, and the linker
# records an rpath pointing straight at the module cache on THIS machine.
# That path exists nowhere else, so a bundle handed to anyone else fails to
# launch (dyld: Library not loaded). bundle copies the two dylibs the binary
# actually links into Contents/Frameworks and repoints the rpath there.
SHERPA_LIB := $(shell go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-macos)/lib/aarch64-apple-darwin
DYLIBS := libsherpa-onnx-c-api.dylib libonnxruntime.dylib

# Task Hub's local LLM classifier runs on Apple's MLX runtime via mlx_lm's
# HTTP server. It is NOT bundled into the app -- a relocatable Python +
# mlx-lm install is ~150-230MB, and most users never turn Task Hub on.
# Instead internal/llm.EnsureRuntime builds it lazily on the user's own
# machine (via `uv`) the first time they enable the feature. See that file
# for the equivalent of what used to live in this Makefile's vendor-mlx
# target.

# Icon generation is a separate, manual step, not a build dependency: the
# generated .icns and .png are committed, so a normal build never runs it.
# tools/mkicons rasterizes them directly rather than going through an SVG,
# because macOS's only bundled SVG renderer (qlmanage) flattens transparency
# onto white -- which turns the menu bar template image into a solid black
# rectangle and puts white corners around the app icon.
ICONSET := build/AppIcon.iconset

.PHONY: build bundle dist icons clean

icons:
	rm -rf $(ICONSET)
	go run ./tools/mkicons \
		-iconset $(ICONSET) \
		-menubar internal/ui/assets/menubar.png \
		-menubar-states internal/ui/assets
	iconutil -c icns $(ICONSET) -o packaging/AppIcon.icns
	rm -rf $(ICONSET)

build:
	CGO_ENABLED=1 go build -o $(BIN) .

bundle: build
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources $(APP)/Contents/Frameworks
	cp $(BIN) $(APP)/Contents/MacOS/$(BIN)
	cp packaging/Info.plist $(APP)/Contents/Info.plist
	cp packaging/AppIcon.icns $(APP)/Contents/Resources/AppIcon.icns
	for lib in $(DYLIBS); do cp $(SHERPA_LIB)/$$lib $(APP)/Contents/Frameworks/$$lib; done
	chmod u+w $(APP)/Contents/Frameworks/*.dylib
	# Drop the module-cache rpath before adding ours: left in place, a machine
	# that happens to have a DIFFERENT sherpa version at that path would load
	# it in preference to the one shipped here.
	install_name_tool -delete_rpath $(SHERPA_LIB) $(APP)/Contents/MacOS/$(BIN)
	install_name_tool -add_rpath @executable_path/../Frameworks $(APP)/Contents/MacOS/$(BIN)
	# Inner code first, then the bundle: --deep is deprecated and signs
	# nested code with the wrong requirements, and any install_name_tool edit
	# invalidates a signature made before it.
	for lib in $(DYLIBS); do \
		codesign --force --sign "$(SIGN_ID)" $(APP)/Contents/Frameworks/$$lib; done
	# Identifier, and why it keeps having to be a NEW one: macOS's
	# notification service answers the very first requestAuthorization for an
	# identifier and remembers the answer forever. If that first prompt is
	# never actually answered -- the app is rebuilt and relaunched while it is
	# on screen -- the identifier is left refused ("Notifications are not
	# allowed for this application", instantly, no prompt), and nothing
	# reachable from userspace clears it: not ncprefs, not lsregister, not
	# killing usernoted, which SIP will not let you restart anyway.
	# com.voxlog.app, com.voxlog.desktop and com.voxlog.mac were burned that
	# way. So: after changing this, launch the app and ANSWER the prompt
	# before rebuilding, or burn another one.
	codesign --force --sign "$(SIGN_ID)" --identifier com.rkostiuk.voxlog2 $(APP)
	codesign --verify --strict $(APP)

# dist produces the file to actually hand someone. ditto (not zip) because
# it is the only archiver that preserves the bundle's symlinks and extended
# attributes, which a signature check needs intact on the other end.
dist: bundle
	rm -f $(ZIP)
	ditto -c -k --keepParent $(APP) $(ZIP)
	@echo "Built $(ZIP) -- signed as: $(SIGN_ID)"

clean:
	rm -rf $(BIN) $(APP) $(ZIP)
