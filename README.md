# savant-audio-proxy

Adds music sources to the Savant app, **without modifying any Savant software**, on a Savant
audio server (Tested with **PAV-SMS2001**):

- **Radios**: your own stations (M3U playlist), played by the Savant audio server's built-in
  player, with Savant favorites and live title, artist and cover art (from the streams' ICY
  metadata).
- **Music Assistant** *(optional)*: browse a [Music Assistant](https://music-assistant.io)
  library from the Savant app, played over AirPlay 2 on the Savant audio server.
- **Roon** *(optional)*: browse a [Roon](https://roon.app) Server's library from the Savant
  app, played over AirPlay on the Savant audio server (needs a licensed Roon Server).

With Music Assistant or Roon, the Savant transport keys (play/pause, next, previous) control
the queue, and a track picked in an album or a playlist plays the rest of it.

## How it works

The Savant host talks to `music-go` (the Savant audio server's music service) on port 4028.
`savant-audio-proxy` sits in between: an iptables rule redirects port 4028 to the proxy (port
14028), which forwards everything to `music-go` and adds its own entries (the radios tile,
"Music Assistant", "Roon") to the responses.

```
Savant app ─► Savant host ─► :4028 ──iptables──► savant-audio-proxy :14028 ─► music-go 127.0.0.1:4028
                                                      │
                                                      ├─ M3U playlist + now playing (ICY or metadata server)
                                                      ├─ Music Assistant (optional)
                                                      └─ Roon (optional)
```

Radios are played by `music-go`'s own player (TuneIn custom stream). Uninstalling removes the
iptables rule and everything is back to stock.

## Install

Logged in over SSH to the Savant audio server with the `RPM` account:

**Radios only:**

```sh
wget -qO- https://raw.githubusercontent.com/fl0zan0/savant_audio_proxy/main/install.sh | sh
```

**Radios + Music Assistant** (the script asks for the address, player and token):

```sh
wget -qO- https://raw.githubusercontent.com/fl0zan0/savant_audio_proxy/main/install.sh | sh -s -- --with-ma
```

The script downloads the latest published version (GitHub Releases), installs everything in
`/data/RPM/savant-audio-proxy`, creates the `savant-audio-proxy` service (started at boot) and
restarts `music-go`. Because `/data` is mounted after systemd loads its units, the service is
also started at boot by an `@reboot` entry in the RPM crontab (`boot.sh`). Music stops for a few seconds.

| Option | Effect |
|---|---|
| `--with-ma` | enables Music Assistant (prompts, or `MA_ADDR`, `MA_PLAYER`, `MA_TOKEN` variables) |
| `--without-ma` | disables Music Assistant |
| `--with-roon` | enables Roon (prompts, or `ROON_ADDR`, `ROON_ZONE` variables) |
| `--without-roon` | disables Roon |
| `--stations SOURCE` | station list: M3U file or http(s) URL |
| `--version vX.Y.Z` | installs a specific version |

**Update** to the latest release (`config.env`, `stations.m3u` and the tokens are kept; nothing
happens if you already run the latest version):

```sh
/data/RPM/savant-audio-proxy/update.sh
```

`update.sh` comes with v1.0.2: to update from v1.0.0 or v1.0.1, run the install command above
once. To re-apply a change made to `config.env` without downloading anything, run
`/data/RPM/savant-audio-proxy/install.sh`.

**No HTTPS on the Savant audio server?** Download `savant-audio-proxy-armv7.tar.gz` from the
[Releases](https://github.com/fl0zan0/savant_audio_proxy/releases) on a computer, then:

```sh
scp savant-audio-proxy-armv7.tar.gz RPM@<ip>:/tmp/
ssh RPM@<ip> 'cd /tmp && tar -xzf savant-audio-proxy-armv7.tar.gz && sh savant-audio-proxy-armv7/install.sh'
```

## Radios

The default list (`stations.m3u`) holds the [Radio Paradise](https://radioparadise.com)
channels as an example. To use your own stations, edit
`/data/RPM/savant-audio-proxy/stations.m3u`:

```
#EXTM3U
#EXTINF:-1 tvg-logo="https://example.com/logo.png",Display name
https://example.com/stream
```

then `sudo systemctl restart savant-audio-proxy`. `STATIONS` in `config.env` can also be an
http(s) URL: the list is then reloaded every 10 minutes. The tile name shown in Savant is
`RADIOS_TITLE` (default "My Radios").

### Logos

The tile images (radios, Music Assistant, Roon) live in [`icons/`](icons) and are served by
jsDelivr; `RADIOS_ICON`, `MA_ICON` and `ROON_ICON` in `config.env` replace them. Station logos
come from the `tvg-logo` URLs of the M3U.

For a site without Internet, `LOCAL_LOGOS="yes"` downloads all logos once into
`/data/RPM/savant-audio-proxy/logos/` and serves them from the Savant audio server
(`http://<its IP>:14029/logos/...`, port `LOGOS_PORT`). These URLs only work on the local
network: the Savant app shows no logo when used remotely. Now-playing cover art stays remote.

### Now playing

`music-go` ignores the titles radios send inside their stream (ICY metadata): on its own it
shows "Custom Stream URL". `savant-audio-proxy` fixes this in one of two ways:

- **ICY (default)**: while a station plays, the proxy opens the stream itself, discards the
  audio and reads the `StreamTitle='Artist - Title'` metadata. Cover art is used when the
  station sends one (`StreamUrl` pointing to an image, as Radio Paradise does), otherwise the
  station logo is shown. Cost: the stream is downloaded a second time while playing (192 kb/s
  for a 192k station). Reading stops by itself 2 minutes after `music-go` stops playing the
  station. Set `ICY_METADATA="no"` in `config.env` to disable.
- **Metadata server** (`COVER_URL`, replaces ICY): the proxy reads `<COVER_URL>/<mount>.json`
  then follows the SSE stream `<COVER_URL>/<mount>/events`. `<mount>` is the last path
  element of the stream URL, and each event is a JSON object with `title`, `artist`, `cover`
  (and optionally `song`, `station`).

A station without ICY metadata is shown with its name and logo.

## Music Assistant

You need:
- the Music Assistant address (`192.168.1.10:8095`);
- a **long-lived token**, created in the Music Assistant UI;
- the **player_id** of this Savant audio server's AirPlay player in Music Assistant. This is
  the id, not the player's name: open **Settings → Players**, click the player, and take the
  end of the page URL (e.g. `…/settings/editplayer/up0123abcd` → `up0123abcd`). The installer
  rejects a value containing spaces.

The Savant app then shows a "Music Assistant" tile with Radios, Playlists, Albums, Artists,
Tracks, Podcasts and a "Browse" entry for the providers. Playback goes over AirPlay 2 to the
Savant audio server.

## Roon

> **Beta.** Tested live: registration, browsing (long lists included), AirPlay playback with
> now playing, next. Still to validate on a real Roon Server: playing a track "from here",
> pause/play/previous, switching from Roon to a radio.

Needs a licensed Roon Server. First, in **Roon → Settings → Audio**, enable this Savant audio
server's AirPlay device (it is a zone of its own, or part of one). Then:

```sh
sh /data/RPM/savant-audio-proxy/install.sh --with-roon
```

The installer asks for the Roon Server address (`192.168.1.20`, port 9330 by default) and the
name of the zone (or of the AirPlay output) in Roon. Then, once, enable **Savant audio proxy**
in **Roon → Settings → Extensions**; its token is kept in `roon_token`.

The Savant app then shows a "Roon" tile with Roon's own menus (Library, Playlists, Qobuz/TIDAL,
radios…). Folders open lists; a track, "Play Album" or "Play Playlist" plays in this unit's
zone (a track plays "from here", so the rest of the album/playlist follows). Roon's search is
not available from Savant.

## Settings

Everything lives in `/data/RPM/savant-audio-proxy/config.env` (template:
[`config.env.example`](config.env.example)). After a change, run the installer again to
regenerate the service.

## Uninstall

```sh
sh /data/RPM/savant-audio-proxy/uninstall.sh            # keeps files and settings
sh /data/RPM/savant-audio-proxy/uninstall.sh --purge    # removes everything
```

## Troubleshooting

- Service status: `systemctl status savant-audio-proxy`
- Host ↔ music-go traffic: `tail -f /data/RPM/savant-audio-proxy/frames.jsonl`
- music-go log: `journalctl -u music-go -f`
- Installed version: `/data/RPM/savant-audio-proxy/savant-audio-proxy -version`

## Development

Go, standard library only.

```sh
go test ./...
LIVE_ICY=1 go test -run TestICYLive -v   # reads live titles from the stations in stations.m3u
scripts/build.sh v1.0.0                  # → dist/savant-audio-proxy-armv7.tar.gz
```

To publish a version: `git tag v1.0.0 && git push origin v1.0.0`. The GitHub Action
`.github/workflows/release.yml` builds and creates the Release with the archive.

Tested on PAV-SMS2001 (ARMv7). Independent project, not affiliated with Savant.

## License

[MIT](LICENSE)
