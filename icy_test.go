package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseICY(t *testing.T) {
	cases := []struct {
		in   string
		want nowMeta
	}{
		{"StreamTitle='Jude Cole - Speed of Life';StreamUrl='http://img.radioparadise.com/covers/l/8225.jpg';\x00\x00",
			nowMeta{Song: "Jude Cole - Speed of Life", Artist: "Jude Cole", Title: "Speed of Life", Cover: "http://img.radioparadise.com/covers/l/8225.jpg"}},
		{"StreamTitle='Flash info';StreamUrl='https://radio.example.com';",
			nowMeta{Song: "Flash info", Title: "Flash info"}},
		{"StreamTitle='Guns N' Roses - Don't Cry';",
			nowMeta{Song: "Guns N' Roses - Don't Cry", Artist: "Guns N' Roses", Title: "Don't Cry"}},
		{"StreamTitle='Zaz - Je veux';", nowMeta{Song: "Zaz - Je veux", Artist: "Zaz", Title: "Je veux"}},
		{"StreamTitle='Caf\xe9 - Cr\xe8me';", nowMeta{Song: "Café - Crème", Artist: "Café", Title: "Crème"}}, // Latin-1
	}
	for _, c := range cases {
		got, ok := parseICY([]byte(c.in))
		if !ok || got != c.want {
			t.Errorf("parseICY(%q) = %+v, %v; want %+v", c.in, got, ok, c.want)
		}
	}
	if _, ok := parseICY([]byte("StreamUrl='x';")); ok {
		t.Error("block without StreamTitle accepted")
	}
}

// icyServer streams fake audio with an ICY block every metaint bytes.
func icyServer(t *testing.T, titles ...string) *httptest.Server {
	const metaint = 64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Icy-MetaData") != "1" {
			t.Errorf("missing Icy-MetaData header")
		}
		w.Header().Set("icy-metaint", "64")
		audio := make([]byte, metaint)
		for i := 0; ; i++ {
			if _, err := w.Write(audio); err != nil {
				return
			}
			meta := []byte("StreamTitle='" + titles[i%len(titles)] + "';")
			n := (len(meta) + 15) / 16
			block := append([]byte{byte(n)}, meta...)
			block = append(block, make([]byte, n*16-len(meta))...)
			if _, err := w.Write(block); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
}

// stopFollower ends the current follower (before the test server closes).
func stopFollower(rd *radios) {
	rd.watchMu.Lock()
	defer rd.watchMu.Unlock()
	if rd.stopW != nil {
		close(rd.stopW)
		rd.watching, rd.stopW = "", nil
	}
}

func TestICYFollow(t *testing.T) {
	srv := icyServer(t, "Artist A - Song A", "Artist B - Song B")
	defer srv.Close()
	s := station{Mount: "live", Name: "Live", Stream: srv.URL + "/live", Logo: "L"}
	rd := &radios{icy: true, meta: map[string]nowMeta{}, byMount: map[string]station{"live": s}}
	defer stopFollower(rd)
	got := make(chan string, 100)
	rd.onMeta = func(mount string) {
		m, _ := rd.current(mount)
		got <- m.Artist
	}
	rd.watch("live")
	seen := map[string]bool{}
	timeout := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case a := <-got:
			seen[a] = true
		case <-timeout:
			t.Fatalf("metadata seen: %v", seen)
		}
	}
	if ti, st, art := rd.display(s); !strings.HasPrefix(ti, "Song ") || !strings.HasSuffix(st, " — Live") || art != "L" {
		t.Errorf("display = %q %q %q", ti, st, art)
	}
}

func TestICYStopsWhenIdle(t *testing.T) {
	srv := icyServer(t, "A - B")
	defer srv.Close()
	s := station{Mount: "live", Stream: srv.URL + "/live"}
	rd := &radios{icy: true, icyIdle: 200 * time.Millisecond, meta: map[string]nowMeta{}, byMount: map[string]station{"live": s}}
	defer stopFollower(rd)
	rd.watch("live")
	deadline := time.Now().Add(3 * time.Second)
	for {
		rd.watchMu.Lock()
		w := rd.watching
		rd.watchMu.Unlock()
		if w == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ICY follower still running after idle timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rd.watch("live") // a new report from music-go restarts it
	rd.watchMu.Lock()
	w := rd.watching
	rd.watchMu.Unlock()
	if w != "live" {
		t.Fatalf("not restarted: %q", w)
	}
}

// LIVE_ICY=1 go test -run TestICYLive -v   reads the first title of the example stations.
func TestICYLive(t *testing.T) {
	if os.Getenv("LIVE_ICY") == "" {
		t.Skip("set LIVE_ICY=1 to read stations.m3u over the network")
	}
	f, err := os.Open("stations.m3u")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, s := range parseM3U(f) {
		rd := &radios{icy: true, meta: map[string]nowMeta{}, byMount: map[string]station{s.Mount: s}}
		got := make(chan nowMeta, 1)
		rd.onMeta = func(m string) {
			if v, ok := rd.current(m); ok {
				select {
				case got <- v:
				default:
				}
			}
		}
		rd.watch(s.Mount)
		select {
		case m := <-got:
			ti, st, art := rd.display(s)
			t.Logf("%s: title=%q subtitle=%q art=%q (artist=%q)", s.Name, ti, st, art, m.Artist)
		case <-time.After(15 * time.Second):
			t.Errorf("%s: no ICY metadata", s.Name)
		}
		stopFollower(rd)
	}
}
