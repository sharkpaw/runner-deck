#!/bin/sh
# Builds the Manager under the name Tauri expects for a bundled binary.
set -e
cd "$(dirname "$0")/../.."
TRIPLE="${TAURI_ENV_TARGET_TRIPLE:-$(rustc -vV | sed -n 's/^host: //p')}"
case "$TRIPLE" in
    aarch64-apple-darwin) GOOS=darwin GOARCH=arm64 EXT= ;;
    x86_64-apple-darwin) GOOS=darwin GOARCH=amd64 EXT= ;;
    x86_64-pc-windows-msvc) GOOS=windows GOARCH=amd64 EXT=.exe ;;
    *) echo "Unsupported target: $TRIPLE" >&2; exit 1 ;;
esac
mkdir -p desktop/src-tauri/binaries
CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -ldflags "-s -w" \
    -o "desktop/src-tauri/binaries/runner-manager-$TRIPLE$EXT" ./cmd/runner-manager
