SQLITE_PREFIX := $(shell brew --prefix sqlite)
CGO_CFLAGS    := -I$(SQLITE_PREFIX)/include
CGO_LDFLAGS   := -L$(SQLITE_PREFIX)/lib -lsqlite3
BUILD_TAGS    := libsqlite3

export CGO_CFLAGS
export CGO_LDFLAGS

PLIST := $(HOME)/Library/LaunchAgents/com.robertkoller.engrex.plist

.PHONY: test build install daemon-stop daemon-start daemon-logs

test:
	go test -tags $(BUILD_TAGS) ./...

build:
	go build -tags $(BUILD_TAGS) -o bin/engrex ./cmd/engrex

# rm before cp gives a fresh inode. Overwriting the binary in place while a
# daemon has it running/mapped corrupts its code signature and causes
# "Killed: 9" on the next launch. This works whether the daemon is running
# in the foreground (engrex daemon) or via launchd — no need to stop it first.
# After installing, restart your foreground daemon to pick up the new binary.
install: build
	sudo rm -f /usr/local/bin/engrex
	sudo cp bin/engrex /usr/local/bin/engrex

# Optional launchd control — only for background auto-start on login.
# Don't run the launchd daemon at the same time as a foreground `engrex daemon`;
# they would both try to bind the same socket.
daemon-start:
	-launchctl load $(PLIST)

daemon-stop:
	-launchctl unload $(PLIST)

daemon-logs:
	tail -f $(HOME)/.engrex/daemon.log
