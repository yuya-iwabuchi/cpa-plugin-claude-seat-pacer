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

.PHONY: build test vet fmt check install clean

build:
	@mkdir -p $(dir $(OUT))
# -trimpath keeps the builder's filesystem paths out of the library's file
# table; the shipped artifact names only module paths.
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(OUT) .
	@echo "built $(OUT)"

test:
	go test ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || { echo "unformatted:"; gofmt -l .; exit 1; }

check: fmt vet test

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
