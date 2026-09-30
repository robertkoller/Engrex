SQLITE_PREFIX := $(shell brew --prefix sqlite)
CGO_CFLAGS    := -I$(SQLITE_PREFIX)/include
CGO_LDFLAGS   := -L$(SQLITE_PREFIX)/lib -lsqlite3
BUILD_TAGS    := libsqlite3

export CGO_CFLAGS
export CGO_LDFLAGS

PLIST       := $(HOME)/Library/LaunchAgents/com.robertkoller.engrex.plist
CACHE_PLIST := $(HOME)/Library/LaunchAgents/com.robertkoller.engrex-cache.plist

# Flags the cache agent runs with. --tolerant lets a reworded question reuse an answer
# when retrieval came back with mostly the same passages, which is the case that actually
# turns up in use: "tell me what is cifar" and "explain cifar to me" retrieve different
# passages, so without it the second one regenerates from scratch. It is the only tier
# that can serve a wrong answer, so it is stated here rather than being a silent default —
# drop it from this line to run strict.
CACHE_FLAGS := --tolerant

XCODE_PROJECT := ui/EngrexUI/EngrexUI.xcodeproj
XCODE_SCHEME  := EngrexUI
# Build into the repo instead of DerivedData so the .app lands somewhere predictable.
APP_BUILD_DIR := $(CURDIR)/bin/app
APP_BUNDLE    := $(APP_BUILD_DIR)/Build/Products/Release/EngrexUI.app

.PHONY: test compile build install daemon-stop daemon-start daemon-logs eval eval-save \
        app app-build app-debug app-install app-run app-open app-clean launch-debug \
        cache cache-install cache-serve cache-bench cache-calibrate \
        cache-agent cache-start cache-stop cache-logs stack-start stack-stop stack-status

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
install: compile cache
	sudo rm -f /usr/local/bin/engrex
	sudo cp bin/engrex /usr/local/bin/engrex
	sudo rm -f /usr/local/bin/engrex-cache
	sudo cp bin/engrex-cache /usr/local/bin/engrex-cache
	@echo ""
	@echo "Installed engrex and engrex-cache."
	@echo "Restart the daemon (make daemon-stop && make daemon-start) to pick it up."

# The semantic cache (cache/). No build tags and no CGO_CFLAGS, unlike every target
# above: cache/ imports only internal/hnsw and internal/embedder, neither of which
# reaches sqlite, so it builds with a plain toolchain. See docs/caching.md.
cache:
	go build -o bin/engrex-cache ./cache/cmd/engrex-cache

cache-install: install

# Write the launchd agent for the cache and load it, so the proxy is up before the daemon
# needs it. Generated rather than checked in, matching how the daemon's own agent is
# handled — see docs/development.md. launchd does not expand ~, hence the absolute paths.
cache-agent:
	@mkdir -p $(HOME)/Library/LaunchAgents
	@printf '%s\n' \
	  '<?xml version="1.0" encoding="UTF-8"?>' \
	  '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
	  '<plist version="1.0">' \
	  '<dict>' \
	  '  <key>Label</key><string>com.robertkoller.engrex-cache</string>' \
	  '  <key>ProgramArguments</key>' \
	  '  <array>' \
	  '    <string>/usr/local/bin/engrex-cache</string>' \
	  '    <string>serve</string>' \
	  $(foreach flag,$(CACHE_FLAGS),'    <string>$(flag)</string>' \) \
	  '  </array>' \
	  '  <key>RunAtLoad</key><true/>' \
	  '  <key>KeepAlive</key><true/>' \
	  '  <key>StandardOutPath</key><string>$(HOME)/.engrex/cache.log</string>' \
	  '  <key>StandardErrorPath</key><string>$(HOME)/.engrex/cache.log</string>' \
	  '  <key>EnvironmentVariables</key>' \
	  '  <dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string></dict>' \
	  '</dict>' \
	  '</plist>' > $(CACHE_PLIST)
	@echo "Wrote $(CACHE_PLIST)"
	@echo "Flags: $(CACHE_FLAGS)"

cache-start: 
	-launchctl load $(CACHE_PLIST)

cache-stop:
	-launchctl unload $(CACHE_PLIST)

cache-logs:
	tail -f $(HOME)/.engrex/cache.log

# Bring the whole thing up in dependency order. The cache has to be listening before the
# daemon starts, because the daemon resolves and pings its Ollama URL once at construction
# and exits if nothing answers. Both agents are KeepAlive, so a wrong order self-heals
# within a few seconds — this just avoids the log noise.
stack-start: install cache-agent
	@echo "Ollama must already be running (ollama serve)."
	$(MAKE) cache-start
	@sleep 1
	./bin/engrex-cache enable
	@# Restart rather than start: the daemon resolves its Ollama endpoint once, at
	@# construction, so an already-running one would keep talking to whatever it was
	@# pointed at when it launched and the enable above would look like it did nothing.
	$(MAKE) daemon-stop
	@sleep 1
	$(MAKE) daemon-start
	@echo ""
	@echo "Proxy    http://127.0.0.1:11435"
	@echo "Metrics  http://127.0.0.1:11436"

stack-stop:
	$(MAKE) daemon-stop
	-./bin/engrex-cache disable
	$(MAKE) cache-stop

stack-status:
	@printf 'ollama    '; curl -s -m 2 http://localhost:11434/api/tags >/dev/null && echo up || echo down
	@printf 'cache     '; curl -s -m 2 http://127.0.0.1:11436/api/stats >/dev/null && echo up || echo down
	@printf 'daemon    '; pgrep -f 'engrex daemon' >/dev/null && echo up || echo down
	@printf 'engrex -> '; python3 -c "import json,os;p=os.path.expanduser('~/.engrex/config.json');print((json.load(open(p)).get('ollama_url') if os.path.exists(p) else '') or 'http://localhost:11434 (cache not in the path)')"

# Run it in the foreground. Ctrl-C stops it.
cache-serve: cache
	./bin/engrex-cache serve

# The load test from docs/caching.md. Uses a synthetic provider, because two thousand
# real generations would take hours — it measures the cache, not the model, and the
# report says so. Needs Ollama running for the embedding model.
cache-bench: cache
	@echo "Starting the proxy with a synthetic provider..."
	@./bin/engrex-cache serve --synthetic-upstream 800ms --data /tmp/engrex-cache-bench > /tmp/engrex-cache-bench.log 2>&1 & \
		sleep 2; \
		./bin/engrex-cache loadtest --requests 2000 --synthetic; \
		pkill -f "engrex-cache serve" || true
	@rm -rf /tmp/engrex-cache-bench

# Measure how well similarity separates a reworded question from a different one, and
# recommend a threshold. Needs Ollama for the embedding model; no generation.
cache-calibrate: cache
	./bin/engrex-cache calibrate

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
app-build:
	xcodebuild -project $(XCODE_PROJECT) -scheme $(XCODE_SCHEME) \
		-configuration Release -derivedDataPath $(APP_BUILD_DIR) build
	@echo "Built $(APP_BUNDLE)"

# Faster: skips optimization. Use while iterating.
app-debug:
	xcodebuild -project $(XCODE_PROJECT) -scheme $(XCODE_SCHEME) \
		-configuration Debug -derivedDataPath $(APP_BUILD_DIR) build

# Replaces the copy in /Applications. Quits the running app first — macOS will not
# overwrite a running bundle cleanly, and a half-replaced .app fails to launch.
app-install: app-build
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
app-run: app-install
	@echo ""
	@echo "EngrexUI running in the foreground — Ctrl-C to quit."
	@echo ""
	@/Applications/EngrexUI.app/Contents/MacOS/EngrexUI

# Alias for convenience: make app builds, installs, and runs the app
app: app-run

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
app-open: app-install
	open /Applications/EngrexUI.app

app-clean:
	rm -rf $(APP_BUILD_DIR)
