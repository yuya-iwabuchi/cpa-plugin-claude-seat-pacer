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

.PHONY: build test vet fmt check dist install clean

build:
	@mkdir -p $(dir $(OUT))
# -trimpath keeps the builder's filesystem paths out of the library's file
# table; the shipped artifact names only module paths.
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(OUT) .
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
# mmap'd and rewriting those pages in place crashes it. Loading a new build
# still needs a host restart, because the library is dlopened once at startup.
install: build
	@mkdir -p $(PLUGIN_DIR)/$(GOOS)/$(GOARCH)
	cp $(OUT) $(INSTALLED).new
	mv $(INSTALLED).new $(INSTALLED)
	@echo "installed $(NAME) v$(VERSION) to $(INSTALLED)"
	@echo "restart CLIProxyAPI to load it: brew services restart cliproxyapi"

clean:
	rm -rf dist
