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
	maIcon := flag.String("ma-icon", "", "image URL for the \"Music Assistant\" tile")
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
	maIconURL = *maIcon
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
				p.maActive.Store(true)
				logEvent("ma_active", "at", "startup")
			}
		}()
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
