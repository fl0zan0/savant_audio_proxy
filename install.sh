#!/bin/sh
# Installs or updates savant-audio-proxy on a Savant audio server (Savant PAV-SMS2001).
#
# From GitHub (logged in over SSH with the RPM account):
#   wget -qO- https://raw.githubusercontent.com/fl0zan0/savant_audio_proxy/main/install.sh | sh
#   wget -qO- https://raw.githubusercontent.com/fl0zan0/savant_audio_proxy/main/install.sh | sh -s -- --with-ma
#
# From an extracted archive (no Internet on the Savant audio server):
#   sh install.sh [options]
#
# Options:
#   --with-ma          enable Music Assistant (asks for address, player and token)
#   --without-ma       disable Music Assistant
#   --stations SOURCE  station list: M3U file or http(s) URL
#   --version vX.Y.Z   install this version instead of the latest
#   --help             show this help
#
# Environment variables (unattended install):
#   MA_ADDR, MA_PLAYER, MA_TOKEN   Music Assistant settings for --with-ma
#   REPO                           GitHub repository (default fl0zan0/savant_audio_proxy)
#
# An update keeps config.env, stations.m3u and ma_token; only the program is replaced.
# Only root command used: "sudo systemctl" (allowed without password for RPM).
# Music stops for a few seconds at the end (music-go restart).

set -eu

NAME=savant-audio-proxy
REPO=${REPO:-fl0zan0/savant_audio_proxy}
ASSET=$NAME-armv7.tar.gz
DEST_DIR=/data/RPM/$NAME
UNIT=$NAME
OLD_UNITS="savant-radios musicgo-proxy"   # former names, removed on install
OLD_DIR=/data/RPM/savant-radios          # settings imported when there is no config yet
PORT=14028

say()  { printf '\n==> %s\n' "$*"; }
fail() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

# --- options ---------------------------------------------------------------------------
WITH_MA=""        # "", yes, no
OPT_STATIONS=""
VERSION=latest
expect=""
for a in "$@"; do
    if [ -n "$expect" ]; then
        case $expect in
            stations) OPT_STATIONS=$a ;;
            version)  VERSION=$a ;;
        esac
        expect=""
        continue
    fi
    case $a in
        --with-ma)    WITH_MA=yes ;;
        --without-ma) WITH_MA=no ;;
        --stations)   expect=stations ;;
        --version)    expect=version ;;
        --help|-h)    sed -n '2,25p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || echo "see https://github.com/$REPO"; exit 0 ;;
        *) fail "unknown option: $a (see --help)" ;;
    esac
done
[ -z "$expect" ] || fail "missing value after --$expect"

# --- run through "wget | sh": download the archive, then run its install.sh -----------------
SELF_DIR=""
case $0 in
    *install.sh) SELF_DIR=$(cd "$(dirname "$0")" && pwd) ;;
esac
if [ -z "$SELF_DIR" ] || [ ! -f "$SELF_DIR/$NAME" ]; then
    if [ "$VERSION" = latest ]; then
        URL="https://github.com/$REPO/releases/latest/download/$ASSET"
    else
        URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
    fi
    TMP=$(mktemp -d /tmp/$NAME.XXXXXX)
    trap 'rm -rf "$TMP"' EXIT
    say "Downloading $URL"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "$TMP/$ASSET" "$URL" || fail "download failed (curl)."
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$TMP/$ASSET" "$URL" || fail "download failed (wget). If this Savant audio server
has no HTTPS: download $ASSET on a computer, copy it with scp, then
  tar -xzf $ASSET && sh $NAME-armv7/install.sh"
    else
        fail "neither curl nor wget on this Savant audio server."
    fi
    tar -xzf "$TMP/$ASSET" -C "$TMP" || fail "unreadable archive."
    sh "$TMP/$NAME-armv7/install.sh" "$@"
    exit $?
fi
SRC_DIR=$SELF_DIR

# --- checks ----------------------------------------------------------------------------
say "Checks"
[ "$(uname -m)" = "armv7l" ] || fail "not a 32-bit ARM system ($(uname -m))."
[ -x /usr/bin/music-go ] || fail "music-go not found: is this a Savant audio server?"
[ -f "$SRC_DIR/config.env.example" ] || fail "config.env.example missing in $SRC_DIR."
sudo -n /bin/systemctl --version >/dev/null 2>&1 || fail "\"sudo systemctl\" is not allowed for $(id -un)."
echo "   version: $("$SRC_DIR/$NAME" -version 2>/dev/null || echo '?')"

# --- copy, keeping existing settings ---------------------------------------------------------
say "Copying to $DEST_DIR"
mkdir -p "$DEST_DIR"
if [ ! -f "$DEST_DIR/config.env" ] && [ -f "$OLD_DIR/config.env" ]; then
    echo "   importing settings from the previous install ($OLD_DIR)"
    for f in config.env stations.m3u ma_token; do
        [ -f "$OLD_DIR/$f" ] && cp -f "$OLD_DIR/$f" "$DEST_DIR/$f"
    done
fi
if [ "$SRC_DIR" != "$DEST_DIR" ]; then
    cp -f "$SRC_DIR/$NAME" "$DEST_DIR/$NAME.new"
    mv -f "$DEST_DIR/$NAME.new" "$DEST_DIR/$NAME"
    for f in install.sh uninstall.sh config.env.example README.md LICENSE; do
        [ -f "$SRC_DIR/$f" ] && cp -f "$SRC_DIR/$f" "$DEST_DIR/$f"
    done
    [ -f "$DEST_DIR/stations.m3u" ] || cp "$SRC_DIR/stations.m3u" "$DEST_DIR/stations.m3u"
fi
[ -f "$DEST_DIR/config.env" ] || cp "$DEST_DIR/config.env.example" "$DEST_DIR/config.env"
chmod 755 "$DEST_DIR/$NAME"
"$DEST_DIR/$NAME" -version >/dev/null 2>&1 || fail "the program does not run on this Savant audio server."

CONF=$DEST_DIR/config.env

# set_conf KEY VALUE: replaces (or appends) KEY="VALUE" in config.env
set_conf() {
    v=$(printf '%s' "$2" | sed 's/[\\&|]/\\&/g')
    if grep -q "^$1=" "$CONF"; then
        sed -i "s|^$1=.*|$1=\"$v\"|" "$CONF"
    else
        printf '%s="%s"\n' "$1" "$2" >> "$CONF"
    fi
}
# ask QUESTION DEFAULT: reads the answer from the terminal (works with "wget | sh" too)
ask() {
    [ -r /dev/tty ] || fail "no terminal to answer \"$1\": use the MA_ADDR, MA_PLAYER, MA_TOKEN variables."
    if [ -n "$2" ]; then printf '%s [%s]: ' "$1" "$2" > /dev/tty; else printf '%s: ' "$1" > /dev/tty; fi
    read -r rep < /dev/tty || rep=""
    [ -n "$rep" ] && echo "$rep" || echo "$2"
}

# --- options applied to config.env -----------------------------------------------------------
[ -n "$OPT_STATIONS" ] && set_conf STATIONS "$OPT_STATIONS"

# shellcheck disable=SC1090
. "$CONF"
if [ "$WITH_MA" = yes ]; then
    say "Music Assistant"
    MA_ADDR=${MA_ADDR:-}; MA_PLAYER=${MA_PLAYER:-}
    [ -n "$MA_ADDR" ] || MA_ADDR=$(ask "Music Assistant address (e.g. 192.168.1.10:8095)" "")
    [ -n "$MA_PLAYER" ] || MA_PLAYER=$(ask "player_id of this Savant audio server's AirPlay player in MA" "")
    [ -n "$MA_ADDR" ] && [ -n "$MA_PLAYER" ] || fail "Music Assistant address and player are required."
    if [ -n "${MA_TOKEN:-}" ]; then
        printf '%s\n' "$MA_TOKEN" > "$DEST_DIR/ma_token"
    elif [ ! -s "$DEST_DIR/ma_token" ]; then
        tok=$(ask "Music Assistant long-lived token" "")
        [ -n "$tok" ] || fail "a Music Assistant token is required."
        printf '%s\n' "$tok" > "$DEST_DIR/ma_token"
    else
        echo "   keeping the existing token ($DEST_DIR/ma_token)"
    fi
    set_conf MA_ADDR "$MA_ADDR"
    set_conf MA_PLAYER "$MA_PLAYER"
elif [ "$WITH_MA" = no ]; then
    set_conf MA_ADDR ""
fi
[ -f "$DEST_DIR/ma_token" ] && chmod 600 "$DEST_DIR/ma_token"

# shellcheck disable=SC1090
. "$CONF"
RADIOS_TITLE=${RADIOS_TITLE:-My Radios}
STATIONS=${STATIONS:-}
COVER_URL=${COVER_URL:-}
ICY_METADATA=${ICY_METADATA:-yes}
MA_ADDR=${MA_ADDR:-}
MA_PLAYER=${MA_PLAYER:-}
LOG_MAX_MB=${LOG_MAX_MB:-50}

# --- command line ----------------------------------------------------------------------
ARGS="--listen 0.0.0.0:$PORT --upstream 127.0.0.1:4028 --log $DEST_DIR/frames.jsonl --log-max-mb $LOG_MAX_MB"
ARGS_TITLE=""
if [ -n "$STATIONS" ]; then
    case $STATIONS in
        http://*|https://*|/*) ST=$STATIONS ;;
        *) ST=$DEST_DIR/$STATIONS ;;
    esac
    case $ST in http*) ;; *) [ -f "$ST" ] || fail "station list not found: $ST" ;; esac
    ARGS="$ARGS --stations $ST"
    if [ -n "$COVER_URL" ]; then
        ARGS="$ARGS --cover-url $COVER_URL"
        echo "   now playing: metadata server $COVER_URL"
    elif [ "$ICY_METADATA" = no ]; then
        ARGS="$ARGS --icy=false"
        echo "   now playing: disabled (station name and logo)"
    else
        echo "   now playing: ICY metadata from the streams"
    fi
    [ -n "${RADIOS_ICON:-}" ] && ARGS="$ARGS --radios-icon $RADIOS_ICON"
    ARGS_TITLE="--radios-title"
    echo "   \"$RADIOS_TITLE\": $ST"
else
    echo "   radios disabled (STATIONS is empty)."
fi
if [ -n "$MA_ADDR" ]; then
    [ -n "$MA_PLAYER" ] || fail "MA_ADDR is set but MA_PLAYER is empty ($CONF)."
    [ -s "$DEST_DIR/ma_token" ] || fail "MA_ADDR is set but the ma_token file is missing."
    ARGS="$ARGS --ma $MA_ADDR --ma-token-file $DEST_DIR/ma_token --ma-player $MA_PLAYER"
    [ -n "${MA_ICON:-}" ] && ARGS="$ARGS --ma-icon $MA_ICON"
    echo "   Music Assistant: $MA_ADDR (player $MA_PLAYER)"
else
    echo "   Music Assistant disabled."
fi

# --- systemd service -------------------------------------------------------------------
say "Creating the $UNIT service"
RULE="-p tcp --dport 4028 -j REDIRECT --to-ports $PORT"
{
    echo "[Unit]"
    echo "Description=savant-audio-proxy: radios + Music Assistant in front of music-go ($PORT -> 127.0.0.1:4028)"
    echo "After=network-online.target music-go.service"
    echo "Wants=network-online.target"
    echo ""
    echo "[Service]"
    echo "ExecStartPre=-/usr/sbin/iptables -t nat -D PREROUTING $RULE"
    echo "ExecStartPre=/usr/sbin/iptables -t nat -I PREROUTING $RULE"
    if [ -n "$ARGS_TITLE" ]; then
        echo "ExecStart=$DEST_DIR/$NAME $ARGS $ARGS_TITLE \"$RADIOS_TITLE\""
    else
        echo "ExecStart=$DEST_DIR/$NAME $ARGS"
    fi
    echo "ExecStopPost=-/usr/sbin/iptables -t nat -D PREROUTING $RULE"
    echo "Restart=on-failure"
    echo "RestartSec=2"
    echo ""
    echo "[Install]"
    echo "WantedBy=multi-user.target"
} > "$DEST_DIR/$UNIT.service"

for old in $OLD_UNITS; do
    if systemctl cat "$old" >/dev/null 2>&1; then
        say "Removing the former $old service"
        sudo systemctl stop "$old" || true
        sudo systemctl disable "$old" || true
    fi
done

sudo systemctl stop "$UNIT" 2>/dev/null || true
sudo systemctl disable "$UNIT" 2>/dev/null || true
sudo systemctl link "$DEST_DIR/$UNIT.service"
sudo systemctl daemon-reload
sudo systemctl enable "$UNIT"
sudo systemctl start "$UNIT"
sleep 2
systemctl is-active --quiet "$UNIT" || {
    systemctl --no-pager status "$UNIT" | tail -15
    fail "the $UNIT service does not start (see above)."
}

# --- host reconnection -----------------------------------------------------------------
say "Restarting music-go (music stops for a few seconds)"
sudo systemctl restart music-go
i=0
while [ $i -lt 30 ]; do
    if ss -tn 2>/dev/null | grep -q ":$PORT "; then break; fi
    sleep 1; i=$((i + 1))
done

say "Result"
echo "   service $UNIT: $(systemctl is-active "$UNIT") / $(systemctl is-enabled "$UNIT")"
if ss -tn 2>/dev/null | grep -q ":$PORT "; then
    echo "   Savant host connected through $NAME: yes"
else
    echo "   Savant host connected through $NAME: not yet (usually within a minute;"
    echo "   check with: ss -tn | grep $PORT)"
fi
echo "   settings: $CONF"
echo "   log: $DEST_DIR/frames.jsonl"
echo "   uninstall: sh $DEST_DIR/uninstall.sh"
echo ""
echo "Installation complete."
