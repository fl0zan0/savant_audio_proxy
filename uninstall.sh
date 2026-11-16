#!/bin/sh
# Uninstalls savant-audio-proxy and gives music-go back direct access.
#   sh /data/RPM/savant-audio-proxy/uninstall.sh            # keeps files and settings
#   sh /data/RPM/savant-audio-proxy/uninstall.sh --purge    # also removes the folder
# Or from GitHub:
#   wget -qO- https://raw.githubusercontent.com/fl0zan0/savant_audio_proxy/main/uninstall.sh | sh
#   wget -qO- https://raw.githubusercontent.com/fl0zan0/savant_audio_proxy/main/uninstall.sh | sh -s -- --purge
set -eu

NAME=savant-audio-proxy
DEST_DIR=/data/RPM/$NAME
UNIT=$NAME

echo "==> Stopping the $UNIT service (also removes the port 4028 redirect)"
sudo systemctl stop "$UNIT" 2>/dev/null || true
sudo systemctl disable "$UNIT" 2>/dev/null || true
sudo systemctl daemon-reload
if command -v crontab >/dev/null 2>&1 && crontab -l 2>/dev/null | grep -q "$DEST_DIR/boot.sh"; then
    crontab -l 2>/dev/null | grep -v "$DEST_DIR/boot.sh" | crontab - || true
fi

echo "==> Restarting music-go (the Savant host reconnects to it directly)"
sudo systemctl restart music-go

if [ "${1:-}" = "--purge" ]; then
    echo "==> Removing $DEST_DIR"
    rm -rf "$DEST_DIR" 2>/dev/null || echo "   (some files created by root, such as the log, could not be removed)"
fi
echo "Uninstall complete."
