package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRoon is a minimal Roon core: registry, browse (stateful, item keys valid only in the
// current level, like the real one), transport.
type fakeRoon struct {
	t  *testing.T
	ln net.Listener

	mu       sync.Mutex
	gen      int
	stack    []*fakeNode
	keys     map[string]*fakeNode // item_key -> node, current level only
	played   []string             // "title@zone"
	controls []string
	queue    []string // titles queued by the last "Play Album"
	jumped   string   // title jumped to with play_from_here
	token    string   // token the extension registered with
	state    string   // zone state
}

type fakeNode struct {
	title, hint string
	kids        []*fakeNode
}

func fakeTree() *fakeNode {
	actions := func() []*fakeNode {
		// like Roon: no "Play From Here" on a track
		return []*fakeNode{{title: "Play Now", hint: "action"}, {title: "Add Next", hint: "action"},
			{title: "Queue", hint: "action"}, {title: "Start Radio", hint: "action"}}
	}
	album := &fakeNode{title: "Album A", hint: "list", kids: []*fakeNode{
		{title: "Play Album", hint: "action_list", kids: []*fakeNode{{title: "Play Now", hint: "action"}}},
		{title: "1. Track 1", hint: "action_list", kids: actions()},
		{title: "2. Track 2", hint: "action_list", kids: actions()},
		{title: "3. Track 3", hint: "action_list", kids: actions()},
	}}
	return &fakeNode{title: "root", hint: "list", kids: []*fakeNode{
		{title: "Library", hint: "list", kids: []*fakeNode{
			{title: "Albums", hint: "list", kids: []*fakeNode{album}},
		}},
		{title: "Search", hint: "action", kids: nil},
		{title: "Settings", hint: "list", kids: []*fakeNode{{title: "Profile", hint: "list"}}},
	}}
}

func newFakeRoon(t *testing.T) *fakeRoon {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRoon{t: t, ln: ln, state: "stopped"}
	f.stack = []*fakeNode{fakeTree()}
	f.relevel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeRoon) relevel() { // new keys for the current level
	f.gen++
	f.keys = map[string]*fakeNode{}
	for i, k := range f.stack[len(f.stack)-1].kids {
		f.keys[fmt.Sprintf("%d:%d", f.gen, i)] = k
	}
}

func (f *fakeRoon) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	head, err := readHTTPHead(br)
	if err != nil {
		return
	}
	key := ""
	for _, l := range strings.Split(string(head), "\r\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(k, "Sec-WebSocket-Key") {
			key = strings.TrimSpace(v)
		}
	}
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(h[:]))
	for {
		fr, err := readFrame(br)
		if err != nil {
			return
		}
		m, err := parseMoo(fr.payload)
		if err != nil {
			f.t.Errorf("fake core: %v", err)
			return
		}
		verb, status, body := f.handle(m)
		if verb == "" {
			continue
		}
		var msg []byte
		if body != nil {
			b, _ := json.Marshal(body)
			msg = []byte(fmt.Sprintf("MOO/1 %s %s\nRequest-Id: %d\nContent-Length: %d\nContent-Type: application/json\n\n%s",
				verb, status, m.id, len(b), b))
		} else {
			msg = []byte(fmt.Sprintf("MOO/1 %s %s\nRequest-Id: %d\n\n", verb, status, m.id))
		}
		// Like the real core, long messages come in several frames.
		if len(msg) > 200 {
			first := encodeFrame(2, msg[:100], false)
			first[0] &^= 0x80 // FIN off
			cont := encodeFrame(0, msg[100:], false)
			if _, err := c.Write(append(first, cont...)); err != nil {
				return
			}
			continue
		}
		if _, err := c.Write(encodeFrame(2, msg, false)); err != nil {
			return
		}
	}
}

func (f *fakeRoon) items(offset, count int) []any {
	kids := f.stack[len(f.stack)-1].kids
	out := []any{}
	for i := offset; i < len(kids) && i < offset+count; i++ {
		k := kids[i]
		it := map[string]any{"title": k.title, "hint": k.hint, "item_key": fmt.Sprintf("%d:%d", f.gen, i), "image_key": "img" + k.title}
		if k.title == "Search" {
			it["input_prompt"] = map[string]any{"prompt": "Search"}
		}
		out = append(out, it)
	}
	return out
}

func (f *fakeRoon) handle(m mooMsg) (string, string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch m.name {
	case "com.roonlabs.registry:1/info":
		return "COMPLETE", "Success", map[string]any{"core_id": "c1", "display_name": "Fake Core"}
	case "com.roonlabs.registry:1/register":
		f.token = str(m.body, "token")
		return "CONTINUE", "Registered", map[string]any{"core_id": "c1", "display_name": "Fake Core", "token": "tok123"}
	case "com.roonlabs.transport:2/get_zones":
		return "COMPLETE", "Success", map[string]any{"zones": []any{
			map[string]any{"zone_id": "z-other", "display_name": "Kitchen", "state": "playing",
				"outputs": []any{map[string]any{"display_name": "Kitchen"}}},
			map[string]any{"zone_id": "z-savant", "display_name": "Salon", "state": f.state,
				"outputs": []any{map[string]any{"display_name": "Savant Media Server"}}},
		}}
	case "com.roonlabs.transport:2/subscribe_queue":
		var items []any
		for i, t := range f.queue {
			items = append(items, map[string]any{"queue_item_id": 100 + i, "three_line": map[string]any{"line1": t}})
		}
		return "CONTINUE", "Subscribed", map[string]any{"items": items}
	case "com.roonlabs.transport:2/unsubscribe_queue":
		return "COMPLETE", "Unsubscribed", nil
	case "com.roonlabs.transport:2/play_from_here":
		i := asInt(m.body["queue_item_id"], -1) - 100
		if i < 0 || i >= len(f.queue) {
			return "COMPLETE", "InvalidRequest", nil
		}
		f.jumped = f.queue[i]
		return "COMPLETE", "Success", nil
	case "com.roonlabs.transport:2/control":
		f.controls = append(f.controls, str(m.body, "control")+"@"+str(m.body, "zone_or_output_id"))
		return "COMPLETE", "Success", nil
	case "com.roonlabs.browse:1/browse":
		if m.body["pop_all"] == true {
			f.stack = f.stack[:1]
			f.relevel()
			return "COMPLETE", "Success", map[string]any{"action": "list"}
		}
		if n := asInt(m.body["pop_levels"], 0); n > 0 {
			f.stack = f.stack[:len(f.stack)-n]
			f.relevel()
			return "COMPLETE", "Success", map[string]any{"action": "list"}
		}
		node, ok := f.keys[str(m.body, "item_key")]
		if !ok {
			return "COMPLETE", "InvalidItemKey", nil
		}
		if node.hint == "action" {
			from := f.stack[len(f.stack)-1].title
			f.played = append(f.played, from+">"+node.title+"@"+str(m.body, "zone_or_output_id"))
			f.queue = nil
			if from == "Play Album" {
				f.queue = []string{"Track 1", "Track 2", "Track 3"}
			}
			f.state = "playing"
			return "COMPLETE", "Success", map[string]any{"action": "none"}
		}
		f.stack = append(f.stack, node)
		f.relevel()
		return "COMPLETE", "Success", map[string]any{"action": "list", "list": map[string]any{"title": node.title}}
	case "com.roonlabs.browse:1/load":
		return "COMPLETE", "Success", map[string]any{"items": f.items(asInt(m.body["offset"], 0), asInt(m.body["count"], 100))}
	}
	return "COMPLETE", "InvalidRequest", nil
}

func waitRegistered(t *testing.T, r *roonClient) {
	t.Helper()
	for i := 0; i < 100 && !r.registered.Load(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !r.registered.Load() {
		t.Fatal("not registered")
	}
}

func titles(l []map[string]any) []string {
	var out []string
	for _, it := range l {
		out = append(out, str(it, "title"))
	}
	return out
}

func find(t *testing.T, l []map[string]any, title string) map[string]any {
	t.Helper()
	for _, it := range l {
		if str(it, "title") == title {
			return it
		}
	}
	t.Fatalf("%q not in %v", title, titles(l))
	return nil
}

func TestRoonBrowseAndPlay(t *testing.T) {
	f := newFakeRoon(t)
	defer f.ln.Close()
	tokFile := filepath.Join(t.TempDir(), "roon_token")
	r := newRoon(f.ln.Addr().String(), tokFile, "savant media server")
	waitRegistered(t, r)
	if b, _ := os.ReadFile(tokFile); strings.TrimSpace(string(b)) != "tok123" {
		t.Fatalf("token file = %q", b)
	}

	root, err := r.list("roon:root", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titles(root), ","); got != "Library,Settings" { // search box skipped
		t.Fatalf("root = %s", got)
	}
	lib := find(t, root, "Library")
	if lib["defaultAction"] != 0 {
		t.Fatalf("Library should be a folder: %v", lib)
	}
	libL, err := r.list(str(lib, "uri"), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	albums, err := r.list(str(find(t, libL, "Albums"), "uri"), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	albumURI := str(find(t, albums, "Album A"), "uri")
	tracks, err := r.list(albumURI, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titles(tracks), ","); got != "Play Album,1. Track 1,2. Track 2,3. Track 3" {
		t.Fatalf("album = %s", got)
	}
	if str(find(t, tracks, "2. Track 2"), "artworkUrl") == "" {
		t.Fatal("no artwork url")
	}

	// Going back up then down again (Savant asks lists in any order): keys are replayed.
	if _, err := r.list("roon:root", 0, 50); err != nil {
		t.Fatal(err)
	}
	if again, err := r.list(albumURI, 0, 50); err != nil || len(again) != 4 {
		t.Fatalf("album again: %v %v", again, err)
	}

	// Play a track: the album plays in the Savant zone (matched by its output name), then the
	// queue jumps to the track.
	if err := r.play(str(find(t, tracks, "2. Track 2"), "uri")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	jumped := f.jumped
	f.mu.Unlock()
	if jumped != "Track 2" {
		t.Fatalf("jumped to %q", jumped)
	}
	// Play an album (action list with a single action) after the session moved.
	if err := r.play(str(find(t, tracks, "Play Album"), "uri")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	played := strings.Join(f.played, " | ")
	f.mu.Unlock()
	if played != "Play Album>Play Now@z-savant | Play Album>Play Now@z-savant" {
		t.Fatalf("played = %s", played)
	}

	if !r.active() {
		t.Fatal("zone should be active")
	}
	if err := r.control("playPause"); err != nil {
		t.Fatal(err)
	}
	r.stop()
	f.mu.Lock()
	controls := strings.Join(f.controls, ",")
	f.mu.Unlock()
	if controls != "playpause@z-savant,stop@z-savant" {
		t.Fatalf("controls = %s", controls)
	}
}

func TestRoonReconnectUsesToken(t *testing.T) {
	f := newFakeRoon(t)
	defer f.ln.Close()
	tokFile := filepath.Join(t.TempDir(), "roon_token")
	if err := os.WriteFile(tokFile, []byte("saved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRoon(f.ln.Addr().String(), tokFile, "Salon")
	waitRegistered(t, r)
	f.mu.Lock()
	tok := f.token
	f.mu.Unlock()
	if tok != "saved" {
		t.Fatalf("registered with token %q", tok)
	}
	if _, err := r.zoneID(); err != nil { // matched by zone name
		t.Fatal(err)
	}
}

func TestRoonPathRoundTrip(t *testing.T) {
	p := []roonStep{{0, "Library"}, {12, "Albums / Été"}}
	got, ok := decodeRoonPath(encodeRoonPath(p))
	if !ok || len(got) != 2 || got[1] != p[1] {
		t.Fatalf("round trip = %v %v", got, ok)
	}
	if p, ok := decodeRoonPath("roon:root"); !ok || p != nil {
		t.Fatal("root")
	}
	if _, ok := decodeRoonPath("roon:p:!!"); ok {
		t.Fatal("bad path accepted")
	}
}
