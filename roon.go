package main

// Roon: browse a Roon Server's library from the Savant app and play to this Savant audio
// server's AirPlay zone in Roon.
//
// Roon extension API: "MOO/1" messages (a request line, headers, a JSON body) in WebSocket
// frames on ws://<core>:9330/api. The extension registers with the core; the first time, the
// user enables it in Roon > Settings > Extensions and the core hands out a token, kept in a
// file for the next connections.
//
// Roon's browse API is stateful (a stack of levels per session) while Savant asks for any list
// by uri: a Savant uri carries the path from the root (index and title of each item opened),
// and the proxy replays it when Savant asks for a list that is not below the current level.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	roonPrefix  = "roon"
	roonAccount = "roon:server_default"
	roonTitle   = "Roon"
	roonSession = "savant"
)

// roonIconURL is the tile image of "Roon" on the music home (empty = none).
var roonIconURL string

var errRoonNotAuthorized = errors.New("Roon: enable the extension in Roon > Settings > Extensions")

type mooMsg struct {
	verb string // REQUEST, COMPLETE, CONTINUE
	name string // service/method for REQUEST, status (Success, ...) otherwise
	id   int
	body map[string]any
}

type roonStep struct {
	I int    `json:"i"` // index in its list
	T string `json:"t"` // title (checked, the list may have changed)
}

type roonClient struct {
	addr      string
	tokenFile string
	zoneName  string

	wmu   sync.Mutex
	conn  net.Conn
	n     atomic.Int64
	regID atomic.Int64

	pmu     sync.Mutex
	pending map[int]chan mooMsg

	registered   atomic.Bool
	onRegistered func()

	rootItems atomic.Value // []map[string]any: Roon's top menu, shown inside the tile

	bmu   sync.Mutex // browse session: Roon keeps one stack of levels
	stack []roonStep
	dirty bool // stack unknown: pop to the root before the next browse
}

func newRoon(addr, tokenFile, zone string) *roonClient {
	r := &roonClient{addr: addr, tokenFile: tokenFile, zoneName: zone, pending: map[int]chan mooMsg{}, dirty: true}
	go r.run()
	return r
}

// run keeps a connection to the core (the extension must stay connected to show up in Roon).
func (r *roonClient) run() {
	backoff := 2 * time.Second
	for {
		start := time.Now()
		if err := r.session(); err != nil {
			logEvent("roon_disconnected", "err", err.Error())
		}
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		time.Sleep(backoff)
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (r *roonClient) session() error {
	c, br, err := wsDial(r.addr, "/api", 10*time.Second)
	if err != nil {
		return err
	}
	r.wmu.Lock()
	r.conn = c
	r.wmu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.readLoop(c, br) }()
	defer func() {
		r.registered.Store(false)
		r.wmu.Lock()
		r.conn = nil
		r.wmu.Unlock()
		c.Close()
		r.bmu.Lock()
		r.dirty = true
		r.bmu.Unlock()
	}()

	if _, err := r.rawCall("com.roonlabs.registry:1/info", nil); err != nil {
		return err
	}
	reg := map[string]any{
		"extension_id":      "com.github.fl0zan0.savant_audio_proxy",
		"display_name":      "Savant audio proxy",
		"display_version":   version,
		"publisher":         "savant_audio_proxy",
		"email":             "",
		"website":           "https://github.com/fl0zan0/savant_audio_proxy",
		"required_services": []string{"com.roonlabs.browse:1", "com.roonlabs.transport:2"},
		"optional_services": []string{},
		"provided_services": []string{"com.roonlabs.ping:1"},
	}
	if b, err := os.ReadFile(r.tokenFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		reg["token"] = strings.TrimSpace(string(b))
	}
	// The answer to register comes only once the extension is enabled in Roon.
	id := int(r.n.Add(1))
	r.regID.Store(int64(id))
	if err := r.send("REQUEST", "com.roonlabs.registry:1/register", id, reg); err != nil {
		return err
	}
	logEvent("roon_connected", "addr", r.addr)
	return <-done
}

func (r *roonClient) readLoop(c net.Conn, br *bufio.Reader) error {
	defer func() {
		r.pmu.Lock()
		for k, ch := range r.pending {
			close(ch)
			delete(r.pending, k)
		}
		r.pmu.Unlock()
	}()
	var frag []byte // a long message (big list) comes in several frames
	for {
		f, err := readFrame(br)
		if err != nil {
			return err
		}
		switch f.op {
		case 9:
			r.wmu.Lock()
			_, _ = c.Write(encodeFrame(10, f.payload, true))
			r.wmu.Unlock()
			continue
		case 8:
			return errors.New("closed by the core")
		case 1, 2:
			frag = append(frag[:0], f.payload...)
		case 0: // continuation
			frag = append(frag, f.payload...)
		default:
			continue
		}
		if !f.fin {
			continue
		}
		payload := frag
		frag = nil
		m, err := parseMoo(payload)
		if err != nil {
			logEvent("roon_bad_message", "err", err.Error())
			continue
		}
		if m.verb == "REQUEST" { // the core calls a service we provide
			if m.name == "com.roonlabs.ping:1/ping" {
				_ = r.send("COMPLETE", "Success", m.id, nil)
			} else {
				_ = r.send("COMPLETE", "InvalidRequest", m.id, map[string]any{"error": "unknown request " + m.name})
			}
			continue
		}
		if int64(m.id) == r.regID.Load() {
			r.onRegister(m)
			continue
		}
		r.pmu.Lock()
		ch, ok := r.pending[m.id]
		if ok {
			delete(r.pending, m.id)
		}
		r.pmu.Unlock()
		if ok {
			ch <- m
		}
	}
}

func (r *roonClient) onRegister(m mooMsg) {
	if m.name != "Registered" {
		logEvent("roon_register_error", "status", m.name, "body", fmt.Sprint(m.body))
		return
	}
	if tok := str(m.body, "token"); tok != "" {
		if err := os.WriteFile(r.tokenFile, []byte(tok+"\n"), 0o600); err != nil {
			logEvent("roon_token_error", "err", err.Error())
		}
	}
	r.registered.Store(true)
	logEvent("roon_registered", "core", str(m.body, "display_name"))
	go func() { // fill the tile's menu
		if _, err := r.list(roonPrefix+":root", 0, 12); err != nil {
			logEvent("roon_root_error", "err", err.Error())
		}
	}()
	if r.onRegistered != nil {
		go r.onRegistered()
	}
}

func (r *roonClient) send(verb, name string, id int, body any) error {
	var msg []byte
	if body != nil {
		b, _ := json.Marshal(body)
		msg = []byte(fmt.Sprintf("MOO/1 %s %s\nRequest-Id: %d\nContent-Length: %d\nContent-Type: application/json\n\n",
			verb, name, id, len(b)))
		msg = append(msg, b...)
	} else {
		msg = []byte(fmt.Sprintf("MOO/1 %s %s\nRequest-Id: %d\n\n", verb, name, id))
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if r.conn == nil {
		return errors.New("Roon not connected")
	}
	_, err := r.conn.Write(encodeFrame(2, msg, true))
	return err
}

func parseMoo(b []byte) (mooMsg, error) {
	var m mooMsg
	head, body, _ := strings.Cut(string(b), "\n\n")
	lines := strings.Split(head, "\n")
	f := strings.SplitN(lines[0], " ", 3)
	if len(f) < 3 || f[0] != "MOO/1" {
		return m, fmt.Errorf("bad first line %q", lines[0])
	}
	m.verb, m.name = f[1], f[2]
	ctype := ""
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "request-id":
			m.id, _ = strconv.Atoi(v)
		case "content-type":
			ctype = v
		}
	}
	if strings.HasPrefix(ctype, "application/json") && body != "" {
		if err := json.Unmarshal([]byte(body), &m.body); err != nil {
			return m, err
		}
	}
	return m, nil
}

// rawCall sends a request and waits for its first answer.
func (r *roonClient) rawCall(name string, body any) (map[string]any, error) {
	id := int(r.n.Add(1))
	ch := make(chan mooMsg, 1)
	r.pmu.Lock()
	r.pending[id] = ch
	r.pmu.Unlock()
	if err := r.send("REQUEST", name, id, body); err != nil {
		r.pmu.Lock()
		delete(r.pending, id)
		r.pmu.Unlock()
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, errors.New("Roon connection lost")
		}
		if m.name != "Success" && m.name != "Subscribed" && m.name != "Unsubscribed" {
			return m.body, fmt.Errorf("Roon %s: %s %v", name, m.name, m.body)
		}
		return m.body, nil
	case <-time.After(15 * time.Second):
		r.pmu.Lock()
		delete(r.pending, id)
		r.pmu.Unlock()
		return nil, errors.New("Roon timeout")
	}
}

func (r *roonClient) call(name string, body any) (map[string]any, error) {
	if !r.registered.Load() {
		return nil, errRoonNotAuthorized
	}
	return r.rawCall(name, body)
}

// ---------------------------------------------------------------- zone and transport

// zone returns this Savant audio server's zone in Roon: the one named zoneName, or holding an
// output named zoneName (case ignored).
func (r *roonClient) zone() (map[string]any, error) {
	res, err := r.call("com.roonlabs.transport:2/get_zones", nil)
	if err != nil {
		return nil, err
	}
	zones, _ := res["zones"].([]any)
	var names []string
	for _, z := range zones {
		zm, _ := z.(map[string]any)
		if strings.EqualFold(str(zm, "display_name"), r.zoneName) {
			return zm, nil
		}
		outs, _ := zm["outputs"].([]any)
		for _, o := range outs {
			if om, _ := o.(map[string]any); strings.EqualFold(str(om, "display_name"), r.zoneName) {
				return zm, nil
			}
		}
		var on []string
		for _, o := range outs {
			om, _ := o.(map[string]any)
			on = append(on, str(om, "display_name"))
		}
		names = append(names, fmt.Sprintf("%s (outputs: %s)", str(zm, "display_name"), strings.Join(on, ", ")))
	}
	return nil, fmt.Errorf("Roon zone %q not found (enable the AirPlay device in Roon > Settings > Audio); zones: %s",
		r.zoneName, strings.Join(names, ", "))
}

func (r *roonClient) zoneID() (string, error) {
	z, err := r.zone()
	if err != nil {
		return "", err
	}
	return str(z, "zone_id"), nil
}

// active reports whether this unit's Roon zone is playing, paused or loading.
func (r *roonClient) active() bool {
	z, err := r.zone()
	if err != nil {
		if err != errRoonNotAuthorized {
			logEvent("roon_zone_error", "err", err.Error())
		}
		return false
	}
	st := str(z, "state")
	return st == "playing" || st == "paused" || st == "loading"
}

var roonControl = map[string]string{"next": "next", "previous": "previous", "pause": "pause",
	"play": "play", "playPause": "playpause", "stop": "stop"}

// control sends a Savant transport key (next, pause, ...) to this unit's Roon zone.
func (r *roonClient) control(key string) error {
	zid, err := r.zoneID()
	if err != nil {
		return err
	}
	_, err = r.call("com.roonlabs.transport:2/control", map[string]any{"zone_or_output_id": zid, "control": roonControl[key]})
	return err
}

func (r *roonClient) stop() {
	if err := r.control("stop"); err != nil {
		logEvent("roon_stop_error", "err", err.Error())
		return
	}
	logEvent("roon_stopped", "zone", r.zoneName)
}

// ---------------------------------------------------------------- browse

func encodeRoonPath(p []roonStep) string {
	if len(p) == 0 {
		return roonPrefix + ":root"
	}
	b, _ := json.Marshal(p)
	return roonPrefix + ":p:" + base64.RawURLEncoding.EncodeToString(b)
}

func decodeRoonPath(uri string) ([]roonStep, bool) {
	if uri == roonPrefix+":root" {
		return nil, true
	}
	s, ok := strings.CutPrefix(uri, roonPrefix+":p:")
	if !ok {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var p []roonStep
	if json.Unmarshal(b, &p) != nil || len(p) == 0 {
		return nil, false
	}
	return p, true
}

func (r *roonClient) browse(opts map[string]any) (map[string]any, error) {
	opts["hierarchy"] = "browse"
	opts["multi_session_key"] = roonSession
	return r.call("com.roonlabs.browse:1/browse", opts)
}

func (r *roonClient) load(offset, count int) ([]any, error) {
	res, err := r.call("com.roonlabs.browse:1/load", map[string]any{"hierarchy": "browse",
		"multi_session_key": roonSession, "offset": offset, "count": count})
	if err != nil {
		return nil, err
	}
	items, _ := res["items"].([]any)
	return items, nil
}

// findKey returns the item_key of step st in the current level: the item at st.I if its title
// matches, else the first item with that title (the list may have changed).
func (r *roonClient) findKey(st roonStep) (string, error) {
	if l, err := r.load(st.I, 1); err == nil && len(l) == 1 {
		if it, _ := l[0].(map[string]any); str(it, "title") == st.T {
			return str(it, "item_key"), nil
		}
	} else if err != nil {
		return "", err
	}
	for off := 0; off < 5000; off += 100 {
		l, err := r.load(off, 100)
		if err != nil {
			return "", err
		}
		for _, x := range l {
			if it, _ := x.(map[string]any); str(it, "title") == st.T {
				return str(it, "item_key"), nil
			}
		}
		if len(l) < 100 {
			break
		}
	}
	return "", fmt.Errorf("Roon item %q not found", st.T)
}

// navigate makes path the current level of the browse session (r.bmu held).
func (r *roonClient) navigate(path []roonStep) error {
	c := 0
	if !r.dirty {
		for c < len(r.stack) && c < len(path) && r.stack[c] == path[c] {
			c++
		}
	}
	if r.dirty || c < len(r.stack) {
		var err error
		if r.dirty || c == 0 {
			_, err = r.browse(map[string]any{"pop_all": true})
			c = 0
		} else {
			_, err = r.browse(map[string]any{"pop_levels": len(r.stack) - c})
		}
		if err != nil {
			r.dirty = true
			return err
		}
		r.stack, r.dirty = r.stack[:c], false
	}
	for _, st := range path[c:] {
		key, err := r.findKey(st)
		if err != nil {
			r.dirty = true
			return err
		}
		res, err := r.browse(map[string]any{"item_key": key})
		if err != nil {
			r.dirty = true
			return err
		}
		if str(res, "action") != "list" {
			r.dirty = true
			return fmt.Errorf("Roon item %q is not a list (%s)", st.T, str(res, "action"))
		}
		r.stack = append(r.stack, st)
	}
	return nil
}

func (r *roonClient) imageURL(key string) string {
	if key == "" {
		return ""
	}
	return "http://" + r.addr + "/api/image/" + key + "?scale=fit&width=500&height=500"
}

func (r *roonClient) rootEntry() map[string]any {
	e := tileItem(roonTitle, roonPrefix+":root", roonIconURL, roonAccount)
	// Like the radios and MA tiles: the music home shows a tile with its menu inside
	// (displayType 3); without it the Savant app leaves an empty space.
	items, _ := r.rootItems.Load().([]map[string]any)
	if items == nil {
		items = []map[string]any{}
	}
	e["displayData"] = map[string]any{"displayType": 3, "data": items}
	return e
}

func (r *roonClient) list(uri string, offset, limit int) ([]map[string]any, error) {
	path, ok := decodeRoonPath(uri)
	if !ok {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	r.bmu.Lock()
	defer r.bmu.Unlock()
	if err := r.navigate(path); err != nil {
		return nil, err
	}
	l, err := r.load(offset, limit)
	if err != nil {
		r.dirty = true
		return nil, err
	}
	out := []map[string]any{}
	for k, x := range l {
		it, _ := x.(map[string]any)
		hint := str(it, "hint")
		if hint == "header" || it["input_prompt"] != nil { // search boxes: not supported
			continue
		}
		child := append(append([]roonStep{}, path...), roonStep{I: offset + k, T: str(it, "title")})
		kind := kindLeaf
		if hint == "list" {
			kind = kindFolder
		}
		out = append(out, svItem(str(it, "title"), encodeRoonPath(child), uri, str(it, "subtitle"),
			r.imageURL(str(it, "image_key")), kind, roonAccount))
	}
	if len(path) == 0 && offset == 0 {
		r.rootItems.Store(out)
	}
	return out, nil
}

// roonPlayActions are the action titles tried in order on an item's action list: "play from
// here" first, so that the skip keys have the rest of the album/playlist to go through.
var roonPlayActions = []string{"Play From Here", "Play Now", "Lire à partir d'ici", "Lire maintenant"}

var errNoContainer = errors.New("no Play Album/Playlist item in this list")

// play plays the item at uri in this unit's zone. A track inside an album or a playlist plays
// the whole album/playlist from that track (Roon offers no "play from here" on a track), so
// that the skip keys go through the rest of it; anything else runs its "play now" action.
func (r *roonClient) play(uri string) error {
	path, ok := decodeRoonPath(uri)
	if !ok || len(path) == 0 {
		return errors.New("not a Roon item")
	}
	zid, err := r.zoneID()
	if err != nil {
		return err
	}
	r.bmu.Lock()
	defer r.bmu.Unlock()
	defer func() { r.dirty = true }() // running an action can change the levels
	if len(path) >= 2 {
		err := r.playFromHere(path, zid)
		if err == nil {
			return nil
		}
		if err != errNoContainer {
			logEvent("roon_play_from_here_error", "item", path[len(path)-1].T, "err", err.Error())
		}
	}
	return r.playLocked(path, zid)
}

var trackNumber = regexp.MustCompile(`^\d+[.)]\s*`)

// sameTrack compares a browse title ("2. Title") with a queue title ("Title").
func sameTrack(browse, queue string) bool {
	return strings.EqualFold(trackNumber.ReplaceAllString(browse, ""), strings.TrimSpace(queue))
}

// playFromHere plays the album/playlist holding the track at path (its "Play Album" or "Play
// Playlist" item, at the top of the list), then jumps to that track in the zone's queue.
func (r *roonClient) playFromHere(path []roonStep, zid string) error {
	parent, last := path[:len(path)-1], path[len(path)-1]
	if err := r.navigate(parent); err != nil {
		return err
	}
	l, err := r.load(0, 300)
	if err != nil {
		return err
	}
	container, pos, seen := -1, -1, 0 // pos: the track's place among the playable items after the container
	for i, x := range l {
		it, _ := x.(map[string]any)
		t, hint := str(it, "title"), str(it, "hint")
		if container < 0 {
			if i != last.I && hint == "action_list" && (strings.HasPrefix(t, "Play ") || strings.HasPrefix(t, "Lire ")) {
				container = i
			}
			continue
		}
		if hint != "action_list" && hint != "action" {
			continue
		}
		if i == last.I {
			pos = seen
		}
		seen++
	}
	if container < 0 || last.I <= container {
		return errNoContainer
	}
	ct, _ := l[container].(map[string]any)
	if err := r.playLocked(append(append([]roonStep{}, parent...), roonStep{I: container, T: str(ct, "title")}), zid); err != nil {
		return err
	}
	r.dirty = true
	for try := 0; try < 10; try++ { // the queue fills a moment after the play
		time.Sleep(300 * time.Millisecond)
		q, err := r.queue(zid)
		if err != nil {
			return err
		}
		id := ""
		for _, x := range q {
			qi, _ := x.(map[string]any)
			tl, _ := qi["three_line"].(map[string]any)
			if sameTrack(last.T, str(tl, "line1")) {
				id = fmt.Sprint(qi["queue_item_id"])
				break
			}
		}
		if id == "" && pos >= 0 && pos < len(q) && len(q) == seen {
			qi, _ := q[pos].(map[string]any)
			id = fmt.Sprint(qi["queue_item_id"])
		}
		if id == "" {
			continue
		}
		_, err = r.call("com.roonlabs.transport:2/play_from_here", map[string]any{"zone_or_output_id": zid, "queue_item_id": jsonID(id)})
		if err == nil {
			logEvent("roon_play_from_here", "item", last.T, "container", str(ct, "title"))
		}
		return err
	}
	return fmt.Errorf("track %q not found in the queue", last.T)
}

// jsonID sends a numeric queue_item_id as a number.
func jsonID(s string) any {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}

// queue returns the items of the zone's queue (one subscription, closed right away).
func (r *roonClient) queue(zid string) ([]any, error) {
	key := fmt.Sprintf("q%d", r.n.Add(1))
	res, err := r.call("com.roonlabs.transport:2/subscribe_queue", map[string]any{"zone_or_output_id": zid,
		"max_item_count": 300, "subscription_key": key})
	_, _ = r.call("com.roonlabs.transport:2/unsubscribe_queue", map[string]any{"subscription_key": key})
	if err != nil {
		return nil, err
	}
	items, _ := res["items"].([]any)
	return items, nil
}

// playLocked runs the play action of the item at path (r.bmu held): an action list (track,
// "Play Album", ...) runs its "play from here" or "play now" action; a plain action runs as is.
func (r *roonClient) playLocked(path []roonStep, zid string) error {
	if err := r.navigate(path[:len(path)-1]); err != nil {
		return err
	}
	last := path[len(path)-1]
	key, err := r.findKey(last)
	if err != nil {
		return err
	}
	res, err := r.browse(map[string]any{"item_key": key, "zone_or_output_id": zid})
	if err != nil {
		return err
	}
	switch str(res, "action") {
	case "list": // action list: pick the play action
		l, err := r.load(0, 20)
		if err != nil {
			return err
		}
		var titles []string
		pick := ""
		for _, want := range roonPlayActions {
			for _, x := range l {
				if it, _ := x.(map[string]any); str(it, "title") == want && pick == "" {
					pick = str(it, "item_key")
				}
			}
		}
		for _, x := range l {
			it, _ := x.(map[string]any)
			titles = append(titles, str(it, "title"))
			if pick == "" && str(it, "hint") == "action" {
				pick = str(it, "item_key")
			}
		}
		if pick == "" {
			return fmt.Errorf("no play action for %q (%s)", last.T, strings.Join(titles, ", "))
		}
		res, err = r.browse(map[string]any{"item_key": pick, "zone_or_output_id": zid})
		if err != nil {
			return err
		}
		logEvent("roon_play", "item", last.T, "actions", strings.Join(titles, ", "))
	default:
		logEvent("roon_play", "item", last.T)
	}
	if str(res, "action") == "message" && res["is_error"] == true {
		return fmt.Errorf("Roon: %s", str(res, "message"))
	}
	return nil
}
