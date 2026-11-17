#!/bin/sh
# Updates savant-audio-proxy to the latest GitHub release, keeping config.env, stations.m3u
# ma_token and roon_token. Does nothing if the latest version is already installed.
#   /data/RPM/savant-audio-proxy/update.sh               # latest release
#   /data/RPM/savant-audio-proxy/update.sh --force       # reinstall even if up to date
#   /data/RPM/savant-audio-proxy/update.sh --version v1.0.1
exec sh "$(cd "$(dirname "$0")" && pwd)/install.sh" --update "$@"
