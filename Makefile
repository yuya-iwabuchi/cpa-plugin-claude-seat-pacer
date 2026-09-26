# The host loads darwin plugins as .dylib and silently ignores .so, so the
# extension is selected per GOOS rather than hardcoded.
GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
NAME   := claude-seat-pacer

ifeq ($(GOOS),darwin)
EXT := dylib
else ifeq ($(GOOS),windows)
EXT := dll
else
EXT := so
endif

VERSION    := $(shell sed -n 's/.*pluginVersion = "\(.*\)".*/\1/p' main.go)
PLUGIN_DIR ?= $(HOME)/.cli-proxy-api/plugins
OUT        := dist/$(GOOS)/$(GOARCH)/$(NAME).$(EXT)
INSTALLED  := $(PLUGIN_DIR)/$(GOOS)/$(GOARCH)/$(NAME)-v$(VERSION).$(EXT)
ARCHIVE    := dist/$(NAME)_$(VERSION)_$(GOOS)_$(GOARCH).zip

.PHONY: build test vet fmt check dist install release-tag clean

# -trimpath keeps the builder's filesystem paths out of the library's file
# table; the shipped artifact names only module paths. A darwin/amd64 library
# built with stock Go shares the host runtime's goroutine slot and crashes the
# host, so that target builds with the patched toolchain the script compiles.
build:
	@mkdir -p $(dir $(OUT))
ifeq ($(GOOS)/$(GOARCH),darwin/amd64)
	.github/scripts/build-darwin-amd64.sh $(OUT)
else
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(OUT) .
endif
	@echo "built $(OUT)"

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || { echo "unformatted:"; gofmt -l .; exit 1; }

check: fmt vet test build

# The store matches a release asset by this exact file name and reads the
# library from the archive root, so neither the archive's name nor the name of
# the entry inside it is free. checksums.txt is assembled from the per-platform
# .sha256 files in the release workflow, because one file covers every zip.
dist: build
	go run ./.github/scripts/package.go -lib $(OUT) -out $(ARCHIVE)

# Write by rename, never by overwrite: a running host keeps the old library
# mmap'd and rewriting those pages in place crashes it. The host loads a
# library only at a file path it has not loaded, and a build of the same
# version lands on the path already loaded, so loading it needs a host restart.
install: build
	@mkdir -p $(PLUGIN_DIR)/$(GOOS)/$(GOARCH)
	cp $(OUT) $(INSTALLED).new
	mv $(INSTALLED).new $(INSTALLED)
	@echo "installed $(NAME) v$(VERSION) to $(INSTALLED)"
	@echo "restart CLIProxyAPI to load it: brew services restart cliproxyapi"

# The release workflow runs on a pushed v<version> tag and refuses one that
# disagrees with pluginVersion or sits off main. This refuses the same mistakes
# before the tag exists, since a published release is immutable and its tag
# cannot be moved or reused.
release-tag:
	@test "$$(git rev-parse --abbrev-ref HEAD)" = main || { echo "check out main first"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "the working tree is not clean"; exit 1; }
	@git fetch -q origin main --tags
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" || { echo "main is not origin/main; pull or push first"; exit 1; }
	@test -z "$$(git tag -l v$(VERSION))" || { echo "v$(VERSION) is already tagged; bump pluginVersion in main.go"; exit 1; }
	git tag -a v$(VERSION) -m "Claude Seat Pacer v$(VERSION)"
	git push origin v$(VERSION)
	@echo "pushed v$(VERSION); the release workflow publishes it"

clean:
	rm -rf dist
