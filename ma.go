package main

// Music Assistant library (optional): browse MA from the Savant UI, play on MA's AirPlay player
// for this unit.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	maPrefix  = "ma"
	maAccount = "ma:server_default"
	maTitle   = "Music Assistant"
)

type maClient struct {
	addr, token, player, base string

	mu       sync.Mutex // connection lifecycle
	wmu      sync.Mutex // writes
	conn     net.Conn
	n        int
	pmu      sync.Mutex
	pending  map[string]chan maResult
	partials map[string][]any
	parents  sync.Map // track uri ("ma:obj:...") -> album/playlist uri it was listed in

	tmu       sync.Mutex
	libTracks []string // "ma:obj:" uris of the library tracks list, in the order shown
}

type maResult struct {
	v   any
	err error
}

func newMA(addr, token, player string) *maClient {
	return &maClient{addr: addr, token: token, player: player, base: "http://" + addr,
		pending: map[string]chan maResult{}, partials: map[string][]any{}}
}

func (m *maClient) connect() error {
	c, r, err := wsDial(m.addr, "/ws", 10*time.Second)
	if err != nil {
		return err
	}
	if _, err := readFrame(r); err != nil { // server info
		c.Close()
		return err
	}
	m.conn = c
	go m.readLoop(c, r)
	res, err := m.rawCall("auth", map[string]any{"token": m.token})
	if err != nil {
		c.Close()
		return err
	}
	if mm, _ := res.(map[string]any); mm == nil || mm["authenticated"] != true {
		c.Close()
		return fmt.Errorf("MA auth failed: %v", res)
	}
	logEvent("ma_connected")
	return nil
}

func (m *maClient) readLoop(c net.Conn, r *bufio.Reader) {
	defer func() {
		m.mu.Lock()
		if m.conn == c {
			m.conn = nil
		}
		m.mu.Unlock()
		c.Close()
		m.pmu.Lock()
		for k, ch := range m.pending {
			ch <- maResult{err: errors.New("MA connection lost")}
			delete(m.pending, k)
		}
		m.pmu.Unlock()
	}()
	for {
		f, err := readFrame(r)
		if err != nil {
			logEvent("ma_disconnected", "err", err.Error())
			return
		}
		switch f.op {
		case 9:
			m.wmu.Lock()
			_, _ = c.Write(encodeFrame(10, f.payload, true))
			m.wmu.Unlock()
			continue
		case 8:
			return
		case 1, 2:
		default:
			continue
		}
		var j map[string]any
		if json.Unmarshal(f.payload, &j) != nil {
			continue
		}
		mid, _ := j["message_id"].(string)
		m.pmu.Lock()
		ch, ok := m.pending[mid]
		if !ok {
			m.pmu.Unlock()
			continue
		}
		if code, has := j["error_code"]; has {
			delete(m.pending, mid)
			delete(m.partials, mid)
			m.pmu.Unlock()
			ch <- maResult{err: fmt.Errorf("%v: %v", code, j["details"])}
			continue
		}
		res := j["result"]
		if j["partial"] == true {
			if l, ok := res.([]any); ok {
				m.partials[mid] = append(m.partials[mid], l...)
			}
			m.pmu.Unlock()
			continue
		}
		if acc, ok := m.partials[mid]; ok {
			if l, ok := res.([]any); ok {
				res = append(acc, l...)
			}
			delete(m.partials, mid)
		}
		delete(m.pending, mid)
		m.pmu.Unlock()
		ch <- maResult{v: res}
	}
}

func (m *maClient) rawCall(cmd string, args map[string]any) (any, error) {
	m.pmu.Lock()
	m.n++
	mid := fmt.Sprintf("g%d", m.n)
	ch := make(chan maResult, 1)
	m.pending[mid] = ch
	m.pmu.Unlock()
	msg := map[string]any{"command": cmd, "message_id": mid}
	if len(args) > 0 {
		msg["args"] = args
	}
	b, _ := json.Marshal(msg)
	m.wmu.Lock()
	c := m.conn
	var err error
	if c == nil {
		err = errors.New("MA not connected")
	} else {
		_, err = c.Write(encodeFrame(1, b, true))
	}
	m.wmu.Unlock()
	if err != nil {
		m.pmu.Lock()
		delete(m.pending, mid)
		m.pmu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(25 * time.Second):
		m.pmu.Lock()
		delete(m.pending, mid)
		m.pmu.Unlock()
		return nil, errors.New("MA timeout")
	}
}

func (m *maClient) call(cmd string, args map[string]any) (any, error) {
	m.mu.Lock()
	if m.conn == nil {
		if err := m.connect(); err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	m.mu.Unlock()
	return m.rawCall(cmd, args)
}

// ---------------------------------------------------------------- mapping MA -> Savant

var maLibMenu = [][2]string{
	{"radios", "Radios"}, {"playlists", "Playlists"}, {"albums", "Albums"},
	{"artists", "Artistes"}, {"tracks", "Morceaux"},
}

func (m *maClient) rootMenu() []map[string]any {
	var out []map[string]any
	for _, kv := range maLibMenu {
		out = append(out, svItem(kv[1], "ma:lib:"+kv[0], "ma:root", "", "", kindFolder, maAccount))
	}
	return append(out, svItem("Parcourir", "ma:browse:root", "ma:root", "", "", kindFolder, maAccount))
}

// maIconURL is the tile image of "Music Assistant" on the music home (empty = none).
var maIconURL string

func (m *maClient) rootEntry() map[string]any {
	e := svItem(maTitle, "ma:root", "root", "", maIconURL, kindFolder, maAccount)
	e["displayData"] = map[string]any{"displayType": 3, "data": m.rootMenu()}
	return e
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func (m *maClient) imageURL(it map[string]any) string {
	var imgs []any
	if im, ok := it["image"].(map[string]any); ok {
		imgs = append(imgs, im)
	}
	if md, ok := it["metadata"].(map[string]any); ok {
		if l, ok := md["images"].([]any); ok {
			imgs = append(imgs, l...)
		}
	}
	if alb, ok := it["album"].(map[string]any); ok {
		if im, ok := alb["image"].(map[string]any); ok {
			imgs = append(imgs, im)
		}
	}
	for _, x := range imgs {
		im, _ := x.(map[string]any)
		if im == nil || str(im, "path") == "" {
			continue
		}
		p := str(im, "path")
		if im["remotely_accessible"] == true && strings.HasPrefix(p, "http") {
			return p
		}
		prov := str(im, "provider")
		if prov == "" {
			prov = "builtin"
		}
		return fmt.Sprintf("%s/imageproxy?path=%s&provider=%s&size=300", m.base, url.QueryEscape(p), url.QueryEscape(prov))
	}
	return ""
}

func maSubtitle(it map[string]any) string {
	var names []string
	if l, ok := it["artists"].([]any); ok {
		for _, a := range l {
			if am, ok := a.(map[string]any); ok && str(am, "name") != "" {
				names = append(names, str(am, "name"))
			}
		}
	}
	return strings.Join(names, ", ")
}

func (m *maClient) toSv(it map[string]any, parent string) map[string]any {
	mt := str(it, "media_type")
	if mt == "folder" {
		if str(it, "name") == ".." || str(it, "path") == "" {
			return nil
		}
		return svItem(str(it, "name"), "ma:browse:"+url.QueryEscape(str(it, "path")), parent, "", "", kindFolder, maAccount)
	}
	k := kindLeaf
	if mt == "album" || mt == "playlist" || mt == "artist" {
		k = kindContainer
	}
	return svItem(str(it, "name"), "ma:obj:"+url.QueryEscape(str(it, "uri")), parent, maSubtitle(it), m.imageURL(it), k, maAccount)
}

func (m *maClient) mapList(res any, parent string) []map[string]any {
	l, _ := res.([]any)
	var out []map[string]any
	for _, x := range l {
		if it, ok := x.(map[string]any); ok {
			if sv := m.toSv(it, parent); sv != nil {
				out = append(out, sv)
			}
		}
	}
	return out
}

func page(l []map[string]any, offset, limit int) []map[string]any {
	if offset >= len(l) {
		return []map[string]any{}
	}
	l = l[offset:]
	if limit > 0 && limit < len(l) {
		l = l[:limit]
	}
	return l
}

func (m *maClient) list(uri string, offset, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 500
	}
	switch {
	case uri == "ma:root":
		return page(m.rootMenu(), offset, limit), nil
	case strings.HasPrefix(uri, "ma:lib:"):
		kind := strings.TrimPrefix(uri, "ma:lib:")
		res, err := m.call("music/"+kind+"/library_items", map[string]any{"limit": limit, "offset": offset, "order_by": "sort_name"})
		if err != nil {
			return nil, err
		}
		l := m.mapList(res, uri)
		if kind == "tracks" {
			m.rememberTracks(l, offset)
		}
		return l, nil
	case strings.HasPrefix(uri, "ma:browse:"):
		p, _ := url.QueryUnescape(strings.TrimPrefix(uri, "ma:browse:"))
		args := map[string]any{}
		if p != "root" {
			args["path"] = p
		}
		res, err := m.call("music/browse", args)
		if err != nil {
			return nil, err
		}
		return page(m.mapList(res, uri), offset, limit), nil
	case strings.HasPrefix(uri, "ma:obj:"):
		mu, _ := url.QueryUnescape(strings.TrimPrefix(uri, "ma:obj:"))
		prov, rest, ok := strings.Cut(mu, "://")
		if !ok {
			return nil, nil
		}
		mt, id, _ := strings.Cut(rest, "/")
		cmd := map[string]string{"album": "music/albums/album_tracks", "playlist": "music/playlists/playlist_tracks",
			"artist": "music/artists/artist_albums"}[mt]
		if cmd == "" {
			return nil, nil
		}
		res, err := m.call(cmd, map[string]any{"item_id": id, "provider_instance_id_or_domain": prov})
		if err != nil {
			return nil, err
		}
		l := m.mapList(res, uri)
		if mt == "album" || mt == "playlist" { // Savant does not send parentUri back on play
			for _, it := range l {
				m.parents.Store(str(it, "uri"), uri)
			}
		}
		return page(l, offset, limit), nil
	}
	return nil, nil
}

// rememberTracks records a page of the library tracks list, so that a track played from it
// can queue the tracks shown after it.
func (m *maClient) rememberTracks(l []map[string]any, offset int) {
	m.tmu.Lock()
	defer m.tmu.Unlock()
	if offset == 0 || offset > len(m.libTracks) {
		m.libTracks = m.libTracks[:0]
		if offset != 0 {
			return // a gap: we cannot know the order before this page
		}
	}
	m.libTracks = m.libTracks[:offset]
	for _, it := range l {
		u := str(it, "uri")
		m.libTracks = append(m.libTracks, u)
		m.parents.Store(u, "ma:lib:tracks")
	}
}

// tracksFrom returns up to max MA uris of the library tracks list, from uri on.
func (m *maClient) tracksFrom(uri string, max int) []any {
	m.tmu.Lock()
	defer m.tmu.Unlock()
	for i, u := range m.libTracks {
		if u != uri {
			continue
		}
		var out []any
		for _, v := range m.libTracks[i:] {
			if len(out) == max {
				break
			}
			mu, _ := url.QueryUnescape(strings.TrimPrefix(v, "ma:obj:"))
			out = append(out, mu)
		}
		return out
	}
	return nil
}

// maMediaType returns the media type of an "ma:obj:" uri (track, album, playlist, ...).
func maMediaType(uri string) string {
	mu, _ := url.QueryUnescape(strings.TrimPrefix(uri, "ma:obj:"))
	_, rest, ok := strings.Cut(mu, "://")
	if !ok {
		return ""
	}
	mt, _, _ := strings.Cut(rest, "/")
	return mt
}

// play replaces MA's queue for this unit. So that the skip keys have somewhere to go, a track
// chosen inside an album or a playlist queues the whole album/playlist from that track, and a
// track chosen in the library tracks list queues the tracks shown after it (up to 100).
func (m *maClient) play(uri, parent string) error {
	mu, _ := url.QueryUnescape(strings.TrimPrefix(uri, "ma:obj:"))
	if parent == "" {
		if v, ok := m.parents.Load(uri); ok {
			parent = v.(string)
		}
	}
	if parent == "ma:lib:tracks" {
		if l := m.tracksFrom(uri, 100); len(l) > 1 {
			_, err := m.call("player_queues/play_media", map[string]any{"queue_id": m.player, "media": l, "option": "replace"})
			if err == nil {
				return nil
			}
			logEvent("ma_play_list_error", "n", len(l), "err", err.Error())
		}
	}
	if pt := maMediaType(parent); strings.HasPrefix(parent, "ma:obj:") && maMediaType(uri) == "track" &&
		(pt == "album" || pt == "playlist") {
		pu, _ := url.QueryUnescape(strings.TrimPrefix(parent, "ma:obj:"))
		_, err := m.call("player_queues/play_media", map[string]any{"queue_id": m.player, "media": pu,
			"option": "replace", "start_item": mu})
		if err == nil {
			return nil
		}
		logEvent("ma_play_from_error", "parent", pu, "err", err.Error())
	}
	_, err := m.call("player_queues/play_media", map[string]any{"queue_id": m.player, "media": mu, "option": "replace"})
	return err
}

// queueActive reports whether MA's queue for this unit is playing or paused.
func (m *maClient) queueActive() bool {
	res, err := m.call("player_queues/get", map[string]any{"queue_id": m.player})
	if err != nil {
		logEvent("ma_queue_error", "err", err.Error())
		return false
	}
	q, _ := res.(map[string]any)
	st := str(q, "state")
	return st == "playing" || st == "paused"
}

// queueCmd sends a queue command without arguments (next, previous) to MA.
func (m *maClient) queueCmd(cmd string) error {
	_, err := m.call("player_queues/"+cmd, map[string]any{"queue_id": m.player})
	return err
}

// stopPlayer stops MA's player for this unit, which ends its AirPlay session on music-go.
func (m *maClient) stopPlayer() {
	if _, err := m.call("player_queues/stop", map[string]any{"queue_id": m.player}); err != nil {
		if _, err2 := m.call("players/cmd/stop", map[string]any{"player_id": m.player}); err2 != nil {
			logEvent("ma_stop_error", "err", err.Error(), "err2", err2.Error())
			return
		}
	}
	logEvent("ma_stopped", "player", m.player)
}
