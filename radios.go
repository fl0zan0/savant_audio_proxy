package main

// Radios: station catalog (M3U), playback through music-go's native
// tunein:customstream path, and optional live now-playing metadata from a metadata server's
// <coverURL>/<mount>/events SSE feed (music-go itself only reports "Custom Stream URL").

import (
	"bufio"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	radioPrefix  = "lr"
	radioAccount = "lr:server_default"
)

var radioTitle = "My Radios"

// radioIcon is the tile image of the radios entry on the music home; empty = first station logo.
var radioIcon string

type station struct {
	Mount  string
	Name   string
	Stream string
	Logo   string
}

type nowMeta struct {
	Song, Artist, Title, Cover, Station string
}

type radios struct {
	source   string        // file path or http(s) URL of the M3U
	coverURL string        // base for /cover/<mount>.json and /cover/<mount>/events
	icy      bool          // without coverURL: read ICY metadata from the stream itself
	icyIdle  time.Duration // 0 = icyIdle constant (tests shorten it)

	mu       sync.RWMutex
	stations []station
	byMount  map[string]station
	byURI    map[string]station // customstream URI -> station
	meta     map[string]nowMeta

	watchMu   sync.Mutex
	watching  string
	stopW     chan struct{}
	lastWatch atomic.Int64 // unix nanos of the last watch() call (ICY idle detection)

	onMeta func(mount string) // called when live metadata for the watched mount changes
}

var (
	extinfName = regexp.MustCompile(`^#EXTINF:[^,]*,(.*)$`)
	extinfLogo = regexp.MustCompile(`tvg-logo="([^"]*)"`)
)

func parseM3U(r io.Reader) []station {
	var out []station
	var name, logo string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF"):
			if m := extinfName.FindStringSubmatch(line); m != nil {
				name = strings.TrimSpace(m[1])
			}
			if m := extinfLogo.FindStringSubmatch(line); m != nil {
				logo = m[1]
			}
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			u, err := url.Parse(line)
			if err != nil || u.Host == "" {
				continue
			}
			mount := strings.Trim(u.Path, "/")
			if i := strings.LastIndex(mount, "/"); i >= 0 {
				mount = mount[i+1:]
			}
			if name == "" {
				name = mount
			}
			out = append(out, station{Mount: mount, Name: name, Stream: line, Logo: logo})
			name, logo = "", ""
		}
	}
	return out
}

func (rd *radios) load() error {
	var body io.ReadCloser
	if strings.HasPrefix(rd.source, "http://") || strings.HasPrefix(rd.source, "https://") {
		resp, err := httpc.Get(rd.source)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("%s: HTTP %d", rd.source, resp.StatusCode)
		}
		body = resp.Body
	} else {
		f, err := os.Open(rd.source)
		if err != nil {
			return err
		}
		body = f
	}
	defer body.Close()
	st := parseM3U(body)
	if len(st) == 0 {
		return fmt.Errorf("no station in %s", rd.source)
	}
	bm, bu := map[string]station{}, map[string]station{}
	for _, s := range st {
		bm[s.Mount] = s
		bu[customURI(s)] = s
	}
	rd.mu.Lock()
	rd.stations, rd.byMount, rd.byURI = st, bm, bu
	if rd.meta == nil {
		rd.meta = map[string]nowMeta{}
	}
	rd.mu.Unlock()
	logEvent("radios_loaded", "count", len(st), "source", rd.source)
	return nil
}

func customURI(s station) string {
	return "tunein:customstream:" + url.QueryEscape(s.Stream)
}

// customAccount is the music-go account put on our customstream items. It must be the active
// built-in TuneIn instance: music-go only lists favorites whose account is a known instance, so a
// per-station UUID (accepted for playback) made "Save As Favorite" rows invisible. Playback and
// favorites both work with tunein:server_default (verified 2026-11-15).
func customAccount(station) string { return "tunein:server_default" }

func (rd *radios) stationForURI(uri string) (station, bool) {
	rd.mu.RLock()
	defer rd.mu.RUnlock()
	s, ok := rd.byURI[uri]
	if !ok && strings.HasPrefix(uri, "tunein:customstream:") {
		// music-go sometimes encodes with %20 vs +; compare decoded URLs.
		if u, err := url.QueryUnescape(strings.TrimPrefix(uri, "tunein:customstream:")); err == nil {
			for _, st := range rd.stations {
				if st.Stream == u {
					return st, true
				}
			}
		}
	}
	return s, ok
}

func (rd *radios) list(offset, limit int) []map[string]any {
	rd.mu.RLock()
	defer rd.mu.RUnlock()
	var out []map[string]any
	for i, s := range rd.stations {
		if i < offset {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, svItem(s.Name, radioPrefix+":station:"+s.Mount, radioPrefix+":root", "", s.Logo, kindLeaf, radioAccount))
	}
	return out
}

func (rd *radios) rootEntry() map[string]any {
	icon := radioIcon
	if icon == "" {
		rd.mu.RLock()
		for _, s := range rd.stations {
			if s.Logo != "" {
				icon = s.Logo
				break
			}
		}
		rd.mu.RUnlock()
	}
	e := tileItem(radioTitle, radioPrefix+":root", icon, radioAccount)
	e["displayData"] = map[string]any{"displayType": 3, "data": rd.list(0, 12)}
	return e
}

// playItem turns a "lr:station:<mount>" item into the music-go customstream item to forward.
func (rd *radios) playItem(uri string) (map[string]any, bool) {
	mount := strings.TrimPrefix(uri, radioPrefix+":station:")
	rd.mu.RLock()
	s, ok := rd.byMount[mount]
	rd.mu.RUnlock()
	if !ok {
		return nil, false
	}
	cu := customURI(s)
	it := svItem(s.Name, cu, "", "", s.Logo, kindLeaf, customAccount(s))
	it["artworkKey"] = cu
	return it, true
}

// ---------------------------------------------------------------- live metadata

func (rd *radios) current(mount string) (nowMeta, bool) {
	rd.mu.RLock()
	defer rd.mu.RUnlock()
	m, ok := rd.meta[mount]
	return m, ok
}

func (rd *radios) setMeta(mount string, m nowMeta) bool {
	rd.mu.Lock()
	old, had := rd.meta[mount]
	rd.meta[mount] = m
	rd.mu.Unlock()
	return !had || old != m
}

func metaFromJSON(raw map[string]any) nowMeta {
	s := func(k string) string { v, _ := raw[k].(string); return strings.TrimSpace(v) }
	return nowMeta{Song: s("song"), Artist: s("artist"), Title: s("title"), Cover: s("cover"), Station: s("station")}
}

// watch makes sure live metadata is followed for this mount (one mount at a time: music-go
// is a single player). Sources: the metadata server (coverURL) if set, else the stream's own
// ICY metadata if enabled, else nothing (Savant shows the station name and logo).
func (rd *radios) watch(mount string) {
	rd.lastWatch.Store(time.Now().UnixNano())
	if rd.coverURL == "" && !rd.icy {
		return
	}
	rd.mu.RLock()
	s, ok := rd.byMount[mount]
	rd.mu.RUnlock()
	if !ok {
		return
	}
	rd.watchMu.Lock()
	defer rd.watchMu.Unlock()
	if rd.watching == mount {
		return
	}
	if rd.stopW != nil {
		close(rd.stopW)
	}
	rd.watching = mount
	stop := make(chan struct{})
	rd.stopW = stop
	if rd.coverURL != "" {
		go rd.follow(mount, stop)
	} else {
		go rd.followICY(s, stop)
	}
}

// unwatch forgets the watched mount if stop is still the current follower, so that the next
// watch() of the same mount starts a new one.
func (rd *radios) unwatch(stop chan struct{}) {
	rd.watchMu.Lock()
	defer rd.watchMu.Unlock()
	if rd.stopW == stop {
		rd.watching, rd.stopW = "", nil
	}
}

// stopWatching stops following the current station's metadata (another source took over).
func (rd *radios) stopWatching() {
	rd.watchMu.Lock()
	defer rd.watchMu.Unlock()
	if rd.stopW != nil {
		close(rd.stopW)
		logEvent("radio_unwatch", "mount", rd.watching)
	}
	rd.watching, rd.stopW = "", nil
}

func (rd *radios) idle() bool {
	d := rd.icyIdle
	if d == 0 {
		d = icyIdle
	}
	return time.Since(time.Unix(0, rd.lastWatch.Load())) > d
}

func (rd *radios) follow(mount string, stop chan struct{}) {
	// Prime with the JSON snapshot, then follow the SSE stream (reconnecting on errors).
	if resp, err := httpc.Get(rd.coverURL + "/" + mount + ".json"); err == nil {
		var raw map[string]any
		if resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&raw) == nil {
			if rd.setMeta(mount, metaFromJSON(raw)) && rd.onMeta != nil {
				rd.onMeta(mount)
			}
		}
		resp.Body.Close()
	}
	backoff := time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		err := rd.sse(mount, stop)
		logEvent("radio_sse_end", "mount", mount, "err", fmt.Sprint(err))
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (rd *radios) sse(mount string, stop chan struct{}) error {
	req, _ := http.NewRequest("GET", rd.coverURL+"/"+mount+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	go func() { <-stop; resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if data.Len() > 0 {
				var ev map[string]any
				if json.Unmarshal([]byte(data.String()), &ev) == nil {
					raw, _ := ev["meta"].(map[string]any)
					if raw == nil {
						raw = ev
					}
					if rd.setMeta(mount, metaFromJSON(raw)) && rd.onMeta != nil {
						rd.onMeta(mount)
					}
				}
				data.Reset()
			}
		}
	}
	return sc.Err()
}

// display returns what Savant should show for this station right now.
func (rd *radios) display(s station) (title, subtitle, art string) {
	m, _ := rd.current(s.Mount)
	title = m.Title
	if title == "" {
		title = m.Song
	}
	if title == "" {
		title = s.Name
	}
	subtitle = s.Name
	if m.Artist != "" && m.Artist != title {
		subtitle = m.Artist + " — " + s.Name
	}
	art = m.Cover
	if art == "" {
		art = s.Logo
	}
	return
}

// rewriteItem patches a music-go now-playing item in place if it is one of our stations.
// Returns the station mount when it did.
func (rd *radios) rewriteItem(it map[string]any) (string, bool) {
	uri, _ := it["uri"].(string)
	if !strings.HasPrefix(uri, "tunein:customstream:") {
		return "", false
	}
	s, ok := rd.stationForURI(uri)
	if !ok {
		return "", false
	}
	t, st, art := rd.display(s)
	it["title"], it["subtitle"] = t, st
	if art != "" {
		it["artworkUrl"], it["thumbnailUrl"] = art, art
		it["artworkKey"] = "lr:" + s.Mount + ":" + shortHash(art)
	}
	return s.Mount, true
}

func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return fmt.Sprintf("%x", h[:6])
}

var httpc = &http.Client{Timeout: 10 * time.Second}
var sseClient = &http.Client{} // no timeout: long-lived event stream
