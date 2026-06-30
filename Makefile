SQLITE_PREFIX := $(shell brew --prefix sqlite)
CGO_CFLAGS    := -I$(SQLITE_PREFIX)/include
CGO_LDFLAGS   := -L$(SQLITE_PREFIX)/lib -lsqlite3
BUILD_TAGS    := libsqlite3

export CGO_CFLAGS
export CGO_LDFLAGS

.PHONY: test build install

test:
	go test -tags $(BUILD_TAGS) ./...

build:
	go build -tags $(BUILD_TAGS) -o bin/engrex ./cmd/engrex

install: build
	sudo cp bin/engrex /usr/local/bin/engrex
