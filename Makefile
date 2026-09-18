VERSION   ?= 0.1.0
BUILD     ?= $(shell git rev-list --count HEAD 2>/dev/null || echo 1)
BUNDLE_ID ?= dev.aiu.menubar
LDFLAGS    = -s -w -X github.com/getparable/aiu/internal/core.Version=$(VERSION)
BIN        = bin/aiu
APP        = build/AIU.app
APP_DEST  ?= $(HOME)/Applications
CLI_DEST  ?= $(HOME)/.local/bin
# -disable-sandbox: swiftc runs macro plugins (@Observable) inside a sandbox of its
# own, and that nested sandbox cannot be applied when the build is already sandboxed —
# Homebrew's builder fails with "sandbox_apply: Operation not permitted" and every
# macro then silently fails to expand. The plugins here are Apple's own.
SWIFT      = swiftc -O -parse-as-library -swift-version 5 -disable-sandbox

# Release signing. SIGN_ID defaults to the first "Developer ID Application" identity in
# the keychain; NOTARY_PROFILE names credentials saved with `xcrun notarytool store-credentials`.
SIGN_ID        ?= $(shell security find-identity -v -p codesigning | sed -n 's/.*"\(Developer ID Application:[^"]*\)".*/\1/p' | head -1)
NOTARY_PROFILE ?= aiu
DIST            = dist
ZIP             = $(DIST)/AIU-$(VERSION).zip

.PHONY: build test icon app install uninstall release clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/aiu

test:
	go test -race ./...

# Regenerates macos/AppIcon.icns from macos/icon/render.swift (the .icns is committed).
icon:
	rm -rf build/AppIcon.iconset && mkdir -p build/AppIcon.iconset
	swift macos/icon/render.swift build/icon-1024.png
	for s in 16 32 128 256 512; do \
	  sips -z $$s $$s build/icon-1024.png --out build/AppIcon.iconset/icon_$${s}x$${s}.png >/dev/null; \
	  sips -z $$((s*2)) $$((s*2)) build/icon-1024.png --out build/AppIcon.iconset/icon_$${s}x$${s}@2x.png >/dev/null; \
	done
	iconutil -c icns build/AppIcon.iconset -o macos/AppIcon.icns
	@echo "✔ macos/AppIcon.icns"

# AIUBar (SwiftUI, the panel) and aiu (Go, everything else) ship side by side.
# BIN_ARCH=universal builds both for arm64 and x86_64 (used by release).
app:
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources bin
ifeq ($(BIN_ARCH),universal)
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/aiu-arm64 ./cmd/aiu
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/aiu-amd64 ./cmd/aiu
	lipo -create -output $(APP)/Contents/MacOS/aiu bin/aiu-arm64 bin/aiu-amd64
	$(SWIFT) -target arm64-apple-macosx26.0 macos/AIUBar.swift -o bin/AIUBar-arm64
	$(SWIFT) -target x86_64-apple-macosx26.0 macos/AIUBar.swift -o bin/AIUBar-x86_64
	lipo -create -output $(APP)/Contents/MacOS/AIUBar bin/AIUBar-arm64 bin/AIUBar-x86_64
else
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/aiu
	cp $(BIN) $(APP)/Contents/MacOS/aiu
	$(SWIFT) -target $(shell uname -m)-apple-macosx26.0 macos/AIUBar.swift -o $(APP)/Contents/MacOS/AIUBar
endif
	cp internal/core/assets/*.svg macos/AppIcon.icns $(APP)/Contents/Resources/
	sed -e 's/__VERSION__/$(VERSION)/g' -e 's/__BUILD__/$(BUILD)/g' -e 's/__BUNDLE_ID__/$(BUNDLE_ID)/g' macos/Info.plist.in > $(APP)/Contents/Info.plist
	codesign --force --sign - $(APP)/Contents/MacOS/aiu >/dev/null 2>&1 || true
	codesign --force --sign - $(APP) >/dev/null 2>&1 || echo "  (ad-hoc signing unavailable — the app still runs locally)"
	@echo "✔ $(APP)"

# The CLI is a symlink into the installed app, so the two never drift apart.
install: app
	mkdir -p $(APP_DEST) $(CLI_DEST)
	-pkill -f "$(APP_DEST)/AIU.app/Contents/MacOS/" 2>/dev/null; sleep 0.5
	rm -rf $(APP_DEST)/AIU.app
	cp -R $(APP) $(APP_DEST)/AIU.app
	ln -sf $(APP_DEST)/AIU.app/Contents/MacOS/aiu $(CLI_DEST)/aiu
	open $(APP_DEST)/AIU.app
	@echo "✔ installed $(APP_DEST)/AIU.app and $(CLI_DEST)/aiu"

uninstall:
	-pkill -f "$(APP_DEST)/AIU.app/Contents/MacOS/" 2>/dev/null
	rm -rf $(APP_DEST)/AIU.app $(CLI_DEST)/aiu

# Universal build, Developer ID signature with hardened runtime and secure timestamp
# (inner binary first, then the bundle), notarization, stapling, and a zip to share.
release: test
	@test -n "$(SIGN_ID)" || { echo "error: no \"Developer ID Application\" certificate in the keychain — see README → Releasing"; exit 1; }
	@xcrun notarytool history --keychain-profile "$(NOTARY_PROFILE)" >/dev/null 2>&1 || { echo "error: no notarytool credentials named \"$(NOTARY_PROFILE)\" — see README → Releasing"; exit 1; }
	$(MAKE) app BIN_ARCH=universal
	codesign --force --timestamp --options runtime --entitlements macos/entitlements.plist --sign "$(SIGN_ID)" $(APP)/Contents/MacOS/aiu
	codesign --force --timestamp --options runtime --entitlements macos/entitlements.plist --sign "$(SIGN_ID)" $(APP)
	codesign --verify --strict --deep --verbose=2 $(APP)
	mkdir -p $(DIST) && rm -f $(ZIP)
	ditto -c -k --keepParent $(APP) $(ZIP)
	xcrun notarytool submit $(ZIP) --keychain-profile "$(NOTARY_PROFILE)" --wait
	xcrun stapler staple $(APP)
	rm -f $(ZIP) && ditto -c -k --keepParent $(APP) $(ZIP)
	spctl --assess --type execute --verbose=2 $(APP)
	@echo "✔ $(ZIP) — signed, notarized and stapled"

clean:
	rm -rf bin build dist
