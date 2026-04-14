#!/usr/bin/env bash
# Rebuild every rmouse binary into build/<os>-<arch>/. CLI bins cross-compile
# from any host; GUI bins can only be built for the host OS (Wails limitation).
# Run from the repo root.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

WIN_DIR="build/windows-amd64"
LINUX_DIR="build/linux-amd64"

# Fresh slate.
rm -rf build
mkdir -p "$WIN_DIR" "$LINUX_DIR"

echo "==> Windows amd64 CLI (client, server, relay)"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$WIN_DIR/mouse-remote-client.exe" ./cmd/mouse-remote-client
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$WIN_DIR/mouse-remote-server.exe" ./cmd/mouse-remote-server
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$WIN_DIR/mouse-remote-relay.exe"  ./cmd/mouse-remote-relay

echo "==> Linux amd64 CLI (client, server, relay)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$LINUX_DIR/mouse-remote-client" ./cmd/mouse-remote-client
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$LINUX_DIR/mouse-remote-server" ./cmd/mouse-remote-server
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$LINUX_DIR/mouse-remote-relay"  ./cmd/mouse-remote-relay

# Wails GUI builds: host-only. On a Windows host we produce the Windows
# GUI binaries; on a Linux host the loop produces the Linux GUI binaries.
HOST_OS="$(go env GOOS)"
case "$HOST_OS" in
  windows) HOST_OUT="$WIN_DIR"; EXT=".exe" ;;
  linux)   HOST_OUT="$LINUX_DIR"; EXT="" ;;
  darwin)  HOST_OUT="build/darwin-$(go env GOARCH)"; mkdir -p "$HOST_OUT"; EXT="" ;;
  *)       echo "!! skipping GUI builds — unsupported host $HOST_OS"; exit 0 ;;
esac

for gui in mouse-remote-client-gui mouse-remote-server-gui; do
  echo "==> $HOST_OS GUI $gui"
  (cd "cmd/$gui" && wails build -clean >/dev/null)
  cp "cmd/$gui/build/bin/$gui$EXT" "$HOST_OUT/$gui$EXT"
done

echo
echo "Done. Layout:"
find build -type f | sort
