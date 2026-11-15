#!/bin/sh
# Builds savant-audio-proxy for the Savant audio server (32-bit ARM) and assembles the
# archive published in the GitHub Releases:
#   dist/savant-audio-proxy-armv7.tar.gz
#     savant-audio-proxy-armv7/{savant-audio-proxy, install.sh, uninstall.sh,
#                               config.env.example, stations.m3u, README.md}
#
#   scripts/build.sh [VERSION]      (default: git describe, else "dev")
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
NAME=savant-audio-proxy
VERSION=${1:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}
PKG=$NAME-armv7
OUT=$ROOT/dist

cd "$ROOT"
echo "==> Tests"
go vet ./...
go test ./...

echo "==> Building $VERSION for linux/arm (GOARM=7)"
rm -rf "$OUT"
mkdir -p "$OUT/$PKG"
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/$PKG/$NAME" .

cp install.sh uninstall.sh config.env.example stations.m3u README.md LICENSE "$OUT/$PKG/"
chmod 755 "$OUT/$PKG/$NAME" "$OUT/$PKG/install.sh" "$OUT/$PKG/uninstall.sh"

echo "==> Archive"
TARFLAGS=""
if tar --version 2>/dev/null | grep -q bsdtar; then
    TARFLAGS="--no-xattrs --no-mac-metadata --no-acls"   # no ._* files from macOS
fi
# shellcheck disable=SC2086
(cd "$OUT" && COPYFILE_DISABLE=1 tar $TARFLAGS -czf "$PKG.tar.gz" "$PKG")
ls -la "$OUT"
