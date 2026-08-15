SQLITE_PREFIX := $(shell brew --prefix sqlite)
CGO_CFLAGS    := -I$(SQLITE_PREFIX)/include
CGO_LDFLAGS   := -L$(SQLITE_PREFIX)/lib -lsqlite3
BUILD_TAGS    := libsqlite3

export CGO_CFLAGS
export CGO_LDFLAGS

PLIST := $(HOME)/Library/LaunchAgents/com.robertkoller.engrex.plist

XCODE_PROJECT := ui/EngrexUI/EngrexUI.xcodeproj
XCODE_SCHEME  := EngrexUI
# Build into the repo instead of DerivedData so the .app lands somewhere predictable.
APP_BUILD_DIR := $(CURDIR)/bin/app
APP_BUNDLE    := $(APP_BUILD_DIR)/Build/Products/Release/EngrexUI.app

.PHONY: test compile build install daemon-stop daemon-start daemon-logs eval eval-save \
        app app-debug app-install app-run app-clean launch launch-debug

test:
	go test -tags $(BUILD_TAGS) ./...

# Retrieval quality against the golden set. Needs Ollama running and a populated
# database — it really embeds and really retrieves. See docs/evaluation.md.
eval: compile
	./bin/engrex eval

# Freeze the current numbers as the baseline every later run diffs against. Only run
# this once a change has been judged an improvement.
eval-save: compile
	./bin/engrex eval --save --label "$(LABEL)"

# Compile only, no install. Used by targets that run ./bin/engrex directly and so
# don't need the copy in /usr/local/bin — keeps them from triggering a sudo prompt.
compile:
	go build -tags $(BUILD_TAGS) -o bin/engrex ./cmd/engrex

# `build` compiles AND installs, because the daemon executes /usr/local/bin/engrex.
# Compiling alone leaves the daemon serving whatever was installed last time, which
# looks exactly like "my changes did nothing".
build: install

# rm before cp gives a fresh inode. Overwriting the binary in place while a
# daemon has it running/mapped corrupts its code signature and causes
# "Killed: 9" on the next launch. This works whether the daemon is running
# in the foreground (engrex daemon) or via launchd — no need to stop it first.
#
# Needs sudo: /usr/local/bin is root-owned.
#
# The daemon still has to be restarted afterwards — a running process keeps executing
# the binary it started with, however new the file on disk is.
install: compile
	sudo rm -f /usr/local/bin/engrex
	sudo cp bin/engrex /usr/local/bin/engrex
	@echo ""
	@echo "Installed. Restart the daemon (Ctrl-C, then 'engrex daemon') to pick it up."

# Optional launchd control — only for background auto-start on login.
# Don't run the launchd daemon at the same time as a foreground `engrex daemon`;
# they would both try to bind the same socket.
daemon-start:
	-launchctl load $(PLIST)

daemon-stop:
	-launchctl unload $(PLIST)

daemon-logs:
	tail -f $(HOME)/.engrex/daemon.log

# Swift menu-bar app — built with xcodebuild, no Xcode GUI needed. Xcode still has to
# be installed (xcodebuild ships with it), and `xcode-select -p` must point at it
# rather than at the bare Command Line Tools.
app:
	xcodebuild -project $(XCODE_PROJECT) -scheme $(XCODE_SCHEME) \
		-configuration Release -derivedDataPath $(APP_BUILD_DIR) build
	@echo "Built $(APP_BUNDLE)"

# Faster: skips optimization. Use while iterating.
app-debug:
	xcodebuild -project $(XCODE_PROJECT) -scheme $(XCODE_SCHEME) \
		-configuration Debug -derivedDataPath $(APP_BUILD_DIR) build

# Replaces the copy in /Applications. Quits the running app first — macOS will not
# overwrite a running bundle cleanly, and a half-replaced .app fails to launch.
app-install: app
	-osascript -e 'quit app "EngrexUI"' 2>/dev/null || true
	rm -rf /Applications/EngrexUI.app
	cp -R $(APP_BUNDLE) /Applications/EngrexUI.app
	@echo "Installed to /Applications/EngrexUI.app"

# Build, deploy, and run the app in the FOREGROUND so Ctrl-C kills it.
#
# Executes the binary inside the bundle rather than `open`-ing the .app: `open` hands
# off to LaunchServices and returns immediately, leaving the app detached from this
# terminal with no way to stop it from here.
#
# It runs the copy in /Applications, not the one in bin/, because macOS ties
# accessibility and input-monitoring permissions to the bundle's path. Running the
# build-directory copy would prompt for those permissions again and leave the granted
# ones pointing at an app you are not using.
launch: app-install
	@echo ""
	@echo "EngrexUI running in the foreground — Ctrl-C to quit."
	@echo ""
	@/Applications/EngrexUI.app/Contents/MacOS/EngrexUI

# Same, but skips the Release build for a faster edit-run loop.
launch-debug: app-debug
	-osascript -e 'quit app "EngrexUI"' 2>/dev/null || true
	rm -rf /Applications/EngrexUI.app
	cp -R $(APP_BUILD_DIR)/Build/Products/Debug/EngrexUI.app /Applications/EngrexUI.app
	@echo ""
	@echo "EngrexUI (debug) running in the foreground — Ctrl-C to quit."
	@echo ""
	@/Applications/EngrexUI.app/Contents/MacOS/EngrexUI

# Detached, if you want it to outlive the terminal.
app-run: app-install
	open /Applications/EngrexUI.app

app-clean:
	rm -rf $(APP_BUILD_DIR)
