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
# Once installed, in /data/RPM/savant-audio-proxy:
#   ./update.sh        update to the latest release (same as ./install.sh --update)
#   ./install.sh       re-apply config.env (regenerates the service, no download)
#
# Options:
#   --with-ma          enable Music Assistant (asks for address, player and token)
#   --without-ma       disable Music Assistant
#   --with-roon        enable Roon (asks for the Roon Server address and this unit's zone)
#   --without-roon     disable Roon
#   --stations SOURCE  station list: M3U file or http(s) URL
#   --version vX.Y.Z   install this version instead of the latest
#   --update           download the latest release even when run from an installed folder;
#                      does nothing if that version is already installed
#   --force            with --update: reinstall even if already up to date
#   --help             show this help
#
# Environment variables (unattended install):
#   MA_ADDR, MA_PLAYER, MA_TOKEN   Music Assistant settings for --with-ma
#   ROON_ADDR, ROON_ZONE           Roon settings for --with-roon
#   REPO                           GitHub repository (default fl0zan0/savant_audio_proxy)
#
# An update keeps config.env, stations.m3u, ma_token and roon_token; only the program is replaced.
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
TAB=$(printf '\t')

say()  { printf '\n==> %s\n' "$*"; }
fail() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

# --- options ---------------------------------------------------------------------------
WITH_MA=""        # "", yes, no
WITH_ROON=""      # "", yes, no
UPDATE=""
FORCE=""
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
        --with-roon)    WITH_ROON=yes ;;
        --without-roon) WITH_ROON=no ;;
        --update)     UPDATE=yes ;;
        --force)      FORCE=yes ;;
        --stations)   expect=stations ;;
        --version)    expect=version ;;
        --help|-h)    sed -n '2,/^$/p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || echo "see https://github.com/$REPO"; exit 0 ;;
        *) fail "unknown option: $a (see --help)" ;;
    esac
done
[ -z "$expect" ] || fail "missing value after --$expect"

# --- run through "wget | sh" or --update: download the archive, then run its install.sh -----
SELF_DIR=""
case $0 in
    *install.sh) SELF_DIR=$(cd "$(dirname "$0")" && pwd) ;;
esac
DOWNLOAD=""
[ -z "$SELF_DIR" ] || [ ! -f "$SELF_DIR/$NAME" ] && DOWNLOAD=yes
[ "$UPDATE" = yes ] && [ -z "${SAP_EXTRACTED:-}" ] && DOWNLOAD=yes
if [ -n "$DOWNLOAD" ]; then
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
    if [ "$UPDATE" = yes ]; then
        cur=$("$DEST_DIR/$NAME" -version 2>/dev/null || echo none)
        new=$("$TMP/$NAME-armv7/$NAME" -version 2>/dev/null || echo "?")
        echo "   installed: $cur   available: $new"
        if [ "$cur" = "$new" ] && [ -z "$FORCE" ]; then
            echo ""
            echo "Already up to date (use --force to reinstall)."
            exit 0
        fi
    fi
    SAP_EXTRACTED=1 sh "$TMP/$NAME-armv7/install.sh" "$@"
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
    for f in install.sh update.sh uninstall.sh config.env.example README.md LICENSE; do
        [ -f "$SRC_DIR/$f" ] && cp -f "$SRC_DIR/$f" "$DEST_DIR/$f"
    done
    [ -f "$DEST_DIR/stations.m3u" ] || cp "$SRC_DIR/stations.m3u" "$DEST_DIR/stations.m3u"
fi
[ -f "$DEST_DIR/config.env" ] || cp "$DEST_DIR/config.env.example" "$DEST_DIR/config.env"
chmod 755 "$DEST_DIR/$NAME"
for f in install.sh update.sh uninstall.sh; do
    [ -f "$DEST_DIR/$f" ] && chmod 755 "$DEST_DIR/$f"
done
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
# no_space NAME VALUE HINT: values written unquoted into the service command line
no_space() {
    case $2 in
        *" "*|*"$TAB"*) fail "$1 must not contain spaces (\"$2\").${3:+
$3}" ;;
    esac
}
PLAYER_HINT="MA_PLAYER is the player_id, not the player name: in Music Assistant open
Settings > Players > this Savant audio server's AirPlay player; the id ends the page URL
(e.g. .../settings/editplayer/up0123abcd). Fix it in $CONF or with MA_PLAYER=... --with-ma."

# --- options applied to config.env -----------------------------------------------------------
[ -n "$OPT_STATIONS" ] && set_conf STATIONS "$OPT_STATIONS"

# shellcheck disable=SC1090
. "$CONF"
if [ "$WITH_MA" = yes ]; then
    say "Music Assistant"
    MA_ADDR=${MA_ADDR:-}; MA_PLAYER=${MA_PLAYER:-}
    [ -n "$MA_ADDR" ] || MA_ADDR=$(ask "Music Assistant address (e.g. 192.168.1.10:8095)" "")
    [ -n "$MA_PLAYER" ] || MA_PLAYER=$(ask "player_id (not the name) of this Savant audio server's AirPlay player in MA, e.g. up0123abcd" "")
    [ -n "$MA_ADDR" ] && [ -n "$MA_PLAYER" ] || fail "Music Assistant address and player are required."
    no_space MA_ADDR "$MA_ADDR"
    no_space MA_PLAYER "$MA_PLAYER" "$PLAYER_HINT"
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

if [ "$WITH_ROON" = yes ]; then
    say "Roon"
    ROON_ADDR=${ROON_ADDR:-}; ROON_ZONE=${ROON_ZONE:-}
    [ -n "$ROON_ADDR" ] || ROON_ADDR=$(ask "Roon Server address (e.g. 192.168.1.20, port 9330 by default)" "")
    [ -n "$ROON_ZONE" ] || ROON_ZONE=$(ask "name of this Savant audio server's AirPlay zone in Roon (Settings > Audio)" "")
    [ -n "$ROON_ADDR" ] && [ -n "$ROON_ZONE" ] || fail "Roon Server address and zone are required."
    no_space ROON_ADDR "$ROON_ADDR"
    set_conf ROON_ADDR "$ROON_ADDR"
    set_conf ROON_ZONE "$ROON_ZONE"
elif [ "$WITH_ROON" = no ]; then
    set_conf ROON_ADDR ""
fi

# shellcheck disable=SC1090
. "$CONF"
RADIOS_TITLE=${RADIOS_TITLE:-My Radios}
STATIONS=${STATIONS:-}
COVER_URL=${COVER_URL:-}
ICY_METADATA=${ICY_METADATA:-yes}
MA_ADDR=${MA_ADDR:-}
MA_PLAYER=${MA_PLAYER:-}
ROON_ADDR=${ROON_ADDR:-}
ROON_ZONE=${ROON_ZONE:-}
LOG_MAX_MB=${LOG_MAX_MB:-50}

# --- command line ----------------------------------------------------------------------
no_space STATIONS "$STATIONS"
no_space COVER_URL "$COVER_URL"
no_space RADIOS_ICON "${RADIOS_ICON:-}"
no_space MA_ADDR "$MA_ADDR"
no_space MA_PLAYER "$MA_PLAYER" "$PLAYER_HINT"
no_space MA_ICON "${MA_ICON:-}"
no_space ROON_ADDR "$ROON_ADDR"
no_space ROON_ICON "${ROON_ICON:-}"
case $ROON_ZONE in *\"*) fail "ROON_ZONE must not contain double quotes." ;; esac
case $RADIOS_TITLE in *\"*) fail "RADIOS_TITLE must not contain double quotes." ;; esac
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
if [ -n "$ROON_ADDR" ]; then
    [ -n "$ROON_ZONE" ] || fail "ROON_ADDR is set but ROON_ZONE is empty ($CONF)."
    # The zone name may contain spaces: quoted for systemd.
    ARGS="$ARGS --roon $ROON_ADDR --roon-token-file $DEST_DIR/roon_token --roon-zone \"$ROON_ZONE\""
    [ -n "${ROON_ICON:-}" ] && ARGS="$ARGS --roon-icon $ROON_ICON"
    echo "   Roon: $ROON_ADDR (zone \"$ROON_ZONE\")"
    [ -s "$DEST_DIR/roon_token" ] || ROON_FIRST=yes
else
    echo "   Roon disabled."
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
        # "%" starts a systemd specifier: doubled (URLs such as wsrv.nl/%3Furl...).
        echo "ExecStart=$DEST_DIR/$NAME $ARGS $ARGS_TITLE \"$RADIOS_TITLE\"" | sed 's/%/%%/g'
    else
        echo "ExecStart=$DEST_DIR/$NAME $ARGS" | sed 's/%/%%/g'
    fi
    echo "ExecStopPost=-/usr/sbin/iptables -t nat -D PREROUTING $RULE"
    echo "Restart=on-failure"
    echo "RestartSec=2"
    echo ""
    echo "[Install]"
    # Same targets as music-go. Not multi-user.target: music-go is ordered after pulseaudio,
    # itself after multi-user.target, so After=music-go there is an ordering cycle at boot.
    echo "WantedBy=savant-audio.target savant-host-audio.target"
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

# --- start at boot ---------------------------------------------------------------------
# The unit is a link into /data, which is mounted after systemd loads its units: at boot
# the service is "not found" and does not start. RPM's crontab runs boot.sh at reboot to
# reload systemd once /data is there, start the service and make the host reconnect.
cat > "$DEST_DIR/boot.sh" <<EOF
#!/bin/sh
# Run at boot by the RPM crontab (@reboot), installed by install.sh.
sleep 15
systemctl is-active --quiet $UNIT && exit 0
sudo -n /bin/systemctl daemon-reload
sudo -n /bin/systemctl start $UNIT
sleep 2
systemctl is-active --quiet $UNIT && sudo -n /bin/systemctl restart music-go
EOF
chmod +x "$DEST_DIR/boot.sh"
if command -v crontab >/dev/null 2>&1; then
    { crontab -l 2>/dev/null | grep -v "$DEST_DIR/boot.sh" || true
      echo "@reboot sh $DEST_DIR/boot.sh >/dev/null 2>&1"; } | crontab - \
        || echo "   warning: could not add the @reboot crontab entry; after a reboot run: sh $DEST_DIR/boot.sh"
else
    echo "   warning: no crontab here; after a reboot run: sh $DEST_DIR/boot.sh"
fi

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
echo "   update: $DEST_DIR/update.sh"
echo "   uninstall: $DEST_DIR/uninstall.sh"
if [ "${ROON_FIRST:-}" = yes ]; then
    echo ""
    echo "   Roon: enable \"Savant audio proxy\" in Roon > Settings > Extensions (once)."
fi
echo ""
echo "Installation complete."
