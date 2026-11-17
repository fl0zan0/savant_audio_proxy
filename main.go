// savant-audio-proxy — sits in front of Savant's music-go (PAV-SMS2001) and adds services to the
// Savant music browser without modifying music-go:
//
//   - "My Radios": the stations of an M3U playlist, played by music-go's native
//     tunein:customstream player, with live now-playing metadata from the streams' own ICY
//     metadata or from a metadata server (see --cover-url).
//   - "Music Assistant" (optional): browse an MA library, play through MA → AirPlay 2.
//
// Deploy on the unit and redirect external traffic on 4028 to it:
//
//	iptables -t nat -I PREROUTING -p tcp --dport 4028 -j REDIRECT --to-ports 14028
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// version is set at build time (-ldflags "-X main.version=v1.2.3").
var version = "dev"

func main() {
	listen := flag.String("listen", "0.0.0.0:14028", "address to listen on")
	upstream := flag.String("upstream", "127.0.0.1:4028", "real music-go address")
	logPath := flag.String("log", "", "JSON-lines frame log (default: stdout)")
	logMax := flag.Int64("log-max-mb", 50, "rotate the log above this size (MB)")
	stations := flag.String("stations", "", "M3U playlist (file or http(s) URL) for the radios tile; empty = disabled")
	coverURL := flag.String("cover-url", "", "optional metadata server base: GET <base>/<mount>.json and SSE <base>/<mount>/events; empty = station name and logo only")
	icy := flag.Bool("icy", true, "without --cover-url, read now-playing titles from the stream's ICY metadata")
	title := flag.String("radios-title", "My Radios", "name of the radios tile shown in Savant")
	radiosIcon := flag.String("radios-icon", "", "image URL for the radios tile (default: first station logo)")
	maIcon := flag.String("ma-icon", defaultMAIcon, "image URL for the \"Music Assistant\" tile (\"none\" = no image)")
	roonAddr := flag.String("roon", "", "Roon Server host[:port] (default port 9330); empty = Roon disabled")
	roonToken := flag.String("roon-token-file", "", "file keeping the Roon extension token (written by the proxy)")
	roonZone := flag.String("roon-zone", "", "name of this Savant audio server's AirPlay zone (or output) in Roon")
	roonIcon := flag.String("roon-icon", defaultRoonIcon, "image URL for the \"Roon\" tile (\"none\" = no image)")
	maAddr := flag.String("ma", "", "Music Assistant HOST:PORT; empty = disabled")
	maTokenFile := flag.String("ma-token-file", "", "file holding an MA long-lived token")
	maPlayer := flag.String("ma-player", "", "MA player_id of this unit's AirPlay player")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := L.open(*logPath, *logMax<<20); err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		os.Exit(1)
	}
	p := &proxy{upstream: *upstream, conns: map[*hostConn]struct{}{}}

	if *stations != "" {
		radioTitle = *title
		radioIcon = *radiosIcon
		rd := &radios{source: *stations, coverURL: strings.TrimRight(*coverURL, "/"), icy: *icy}
		if err := rd.load(); err != nil {
			fmt.Fprintln(os.Stderr, "stations:", err)
			os.Exit(1)
		}
		rd.onMeta = p.pushMeta
		p.rd = rd
		if strings.HasPrefix(*stations, "http") {
			go func() {
				for range time.Tick(10 * time.Minute) {
					if err := rd.load(); err != nil {
						logEvent("radios_reload_error", "err", err.Error())
					}
				}
			}()
		}
	}
	maIconURL = iconURL(*maIcon)
	if *maAddr != "" {
		if *maTokenFile == "" || *maPlayer == "" {
			fmt.Fprintln(os.Stderr, "--ma needs --ma-token-file and --ma-player")
			os.Exit(2)
		}
		tok, err := os.ReadFile(*maTokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ma token:", err)
			os.Exit(1)
		}
		addr := *maAddr
		if !strings.Contains(addr, ":") {
			addr += ":8095"
		}
		p.ma = newMA(addr, strings.TrimSpace(string(tok)), *maPlayer)
		go func() { // MA may already be playing on this unit (proxy restarted)
			if p.ma.queueActive() {
				p.ext.Store(extMA)
				logEvent("ma_active", "at", "startup")
			}
		}()
	}

	roonIconURL = iconURL(*roonIcon)
	if *roonAddr != "" {
		if *roonToken == "" || *roonZone == "" {
			fmt.Fprintln(os.Stderr, "--roon needs --roon-token-file and --roon-zone")
			os.Exit(2)
		}
		addr := *roonAddr
		if !strings.Contains(addr, ":") {
			addr += ":9330"
		}
		r := newRoon(addr, *roonToken, *roonZone)
		r.onRegistered = func() { // Roon may already be playing on this unit (proxy restarted)
			if p.ext.Load() == extNone && r.active() {
				p.ext.Store(extRoon)
				logEvent("roon_active", "at", "startup")
			}
		}
		p.roon = r
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	logEvent("listening", "version", version, "listen", *listen, "upstream", *upstream, "pid", os.Getpid(),
		"radios", p.rd != nil, "ma", *maAddr)
	for {
		c, err := ln.Accept()
		if err != nil {
			logEvent("accept_error", "err", err.Error())
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go p.serve(c)
	}
}

// Tile logos (dashboardicons.com, homarr-labs/dashboard-icons), used unless replaced.
const (
	defaultMAIcon   = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/music-assistant.png"
	defaultRoonIcon = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/roon.png"
)

// iconURL maps the "none" setting to no image.
func iconURL(s string) string {
	if s == "none" {
		return ""
	}
	return s
}
