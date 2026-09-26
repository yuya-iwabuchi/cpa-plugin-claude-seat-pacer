#!/usr/bin/env bash
# Loads a darwin/amd64 library into the official CLIProxyAPI darwin/amd64
# release and fails unless the host loads it, serves its status and page
# routes, and stays alive through a soak with GOGC=10, where a runtime reading
# the other's goroutine corrupts the heap within seconds.
#
# The host runs with an empty auth-dir, so the plugin holds no seat and reads
# no usage, and its proxy variables name a closed loopback port, so the
# host's own outbound requests fail. The only network access is the release
# download from GitHub and the host on loopback. On an arm64 Mac the host runs
# under Rosetta.
#
# Usage: load-test-darwin-amd64.sh <library>
set -euo pipefail

CPA_VERSION=7.3.15
CPA_SHA256=1dd2f2f5d57c2c9172eb51837d07f1f014d02ab1093215401a00c61d942bb972
SOAK_SECONDS=${SOAK_SECONDS:-120}
PLUGIN=claude-seat-pacer

library=${1:?usage: load-test-darwin-amd64.sh <library>}
work=$(mktemp -d)
pid=
log=$work/host.log
fail() {
  echo "FAIL: $*"
  echo "--- host log (last 200 lines) ---"
  tail -n 200 "$log"
  exit 1
}
# A host that ignores SIGTERM for 15 s is killed, so a hang while it unloads
# the plugin cannot hold the job to its timeout.
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    kill -9 "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

asset=CLIProxyAPI_${CPA_VERSION}_darwin_amd64.tar.gz
release=https://github.com/router-for-me/CLIProxyAPI/releases/download/v$CPA_VERSION

curl -fsSL -o "$work/$asset" "$release/$asset"
echo "$CPA_SHA256  $work/$asset" | shasum -a 256 -c -
mkdir -p "$work/host" "$work/auth" "$work/home" "$work/plugins/darwin/amd64"
tar -xzf "$work/$asset" -C "$work/host"
cp "$library" "$work/plugins/darwin/amd64/$PLUGIN.dylib"

key=$(openssl rand -hex 24)
port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
cat > "$work/config.yaml" <<EOF
host: "127.0.0.1"
port: $port
auth-dir: "$work/auth"
remote-management:
  allow-remote: false
  secret-key: "$key"
  disable-control-panel: true
  disable-auto-update-panel: true
routing:
  session-affinity: false
logging-to-file: false
plugins:
  enabled: true
  dir: "$work/plugins"
  configs:
    $PLUGIN:
      enabled: true
EOF

launch=()
if [ "$(uname -m)" = arm64 ]; then launch=(arch -x86_64); fi
dead=http://127.0.0.1:9
env HOME="$work/home" GOGC=10 HTTP_PROXY=$dead HTTPS_PROXY=$dead ALL_PROXY=$dead NO_PROXY=127.0.0.1,localhost \
  ${launch[@]+"${launch[@]}"} "$work/host/cli-proxy-api" -config "$work/config.yaml" -local-model > "$log" 2>&1 &
pid=$!

status_url=http://127.0.0.1:$port/v0/management/plugins/$PLUGIN/status
page_url=http://127.0.0.1:$port/v0/resource/plugins/$PLUGIN/index.html
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$@" || true; }
check_log() {
  if grep -E 'failed to load plugin|fatal|panic|SIGSEGV|SIGBUS' "$log"; then fail "the host log reports a failure"; fi
}

for _ in $(seq 60); do
  kill -0 "$pid" 2>/dev/null || fail "the host exited during startup"
  [ "$(code -H "Authorization: Bearer $key" "$status_url")" = 200 ] && break
  sleep 1
done
check_log
got=$(code -H "Authorization: Bearer $key" "$status_url")
[ "$got" = 200 ] || fail "GET status answered $got"
got=$(code "$page_url")
[ "$got" = 200 ] || fail "GET index.html answered $got"
echo "loaded: status 200, index.html 200"

polls=0
end=$((SECONDS + SOAK_SECONDS))
while [ "$SECONDS" -lt "$end" ]; do
  kill -0 "$pid" 2>/dev/null || fail "the host exited after $polls polls"
  got=$(code -H "Authorization: Bearer $key" "$status_url")
  [ "$got" = 200 ] || fail "GET status answered $got after $polls polls"
  got=$(code "$page_url")
  [ "$got" = 200 ] || fail "GET index.html answered $got after $polls polls"
  polls=$((polls + 1))
done
kill -0 "$pid" 2>/dev/null || fail "the host exited at the end of the soak"
check_log
echo "soak: host alive after $polls polls of both routes over ${SOAK_SECONDS}s with GOGC=10"
