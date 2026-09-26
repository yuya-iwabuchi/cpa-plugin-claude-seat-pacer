#!/usr/bin/env bash
# Builds the darwin/amd64 library with a Go toolchain whose runtime keeps the
# current goroutine in TSD slot 11 (%gs:0x58), and fails unless every
# goroutine access in the library uses that slot.
#
# Stock Go on darwin/amd64 keeps it in slot 6 (%gs:0x30) in every Go runtime.
# CLIProxyAPI is a Go program, so a stock-built plugin's runtime reads the
# host's goroutine on host threads and corrupts the host's heap. Linux, Windows
# and darwin/arm64 give each runtime its own slot, and build with stock Go.
#
# The toolchain is Go GO_VERSION from checksum-verified upstream source plus
# go-tls-slot.patch, bootstrapped by the `go` on PATH. It lands in
# $GO_TLS_ROOT/go and is reused while $GO_TLS_ROOT/go/.stamp matches the
# version and the patch.
#
# Usage: build-darwin-amd64.sh <library>
set -euo pipefail

# GO_VERSION equals go.mod's toolchain directive. Raising that pin means
# setting both of these from https://go.dev/dl/ and checking the patch still
# applies.
GO_VERSION=go1.25.14
GO_SRC_SHA256=9e83f44f5fc297378861b4e16cc6aa114be8add7993fb3ceb2c512380aa4d582

out=${1:?usage: build-darwin-amd64.sh <library>}
mkdir -p "$(dirname "$out")"
library=$(cd "$(dirname "$out")" && pwd)/$(basename "$out")
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
patch_file=$here/go-tls-slot.patch
root=${GO_TLS_ROOT:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/go-tls}

pinned=$(sed -n 's/^toolchain //p' "$repo/go.mod")
if [ "$pinned" != "$GO_VERSION" ]; then
  echo "go.mod pins $pinned; this script builds $GO_VERSION. Set GO_VERSION and GO_SRC_SHA256 to $pinned." >&2
  exit 1
fi

stamp="$GO_VERSION $(shasum -a 256 "$patch_file" | cut -d' ' -f1)"
goroot=$root/go
if [ "$(cat "$goroot/.stamp" 2>/dev/null)" != "$stamp" ]; then
  rm -rf "$root"
  mkdir -p "$root"
  curl -fsSL -o "$root/src.tar.gz" "https://go.dev/dl/$GO_VERSION.src.tar.gz"
  echo "$GO_SRC_SHA256  $root/src.tar.gz" | shasum -a 256 -c -
  tar -xzf "$root/src.tar.gz" -C "$root"
  rm "$root/src.tar.gz"
  patch -d "$goroot" -p1 --forward < "$patch_file"
  bootstrap=$(GOTOOLCHAIN=local go env GOROOT)
  (cd "$goroot/src" && env -u GOOS -u GOARCH -u GOAMD64 -u GOEXPERIMENT -u GOFLAGS -u CGO_ENABLED -u CC GOROOT_BOOTSTRAP="$bootstrap" ./make.bash)
  echo "$stamp" > "$goroot/.stamp"
fi

# A build cache of its own: a release toolchain's tool ids are its version
# string, which the stock and patched linkers share.
(cd "$repo" && GOTOOLCHAIN=local GOROOT="$goroot" GOCACHE="$root/cache" \
  GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 CC="clang -arch x86_64" \
  "$goroot/bin/go" build -trimpath -buildmode=c-shared -o "$library" .)

otool -tv "$library" | awk '/%gs:0x30/ {stock++} /%gs:0x58/ {patched++}
  END {
    printf "goroutine accesses: %%gs:0x58 x%d, %%gs:0x30 x%d\n", patched, stock
    exit !(patched > 0 && stock == 0)
  }'
