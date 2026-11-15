package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------- Savant item helpers

type itemKind int

const (
	kindFolder    itemKind = iota // browse only
	kindContainer                 // browse + play menu (album, playlist, artist)
	kindLeaf                      // play now
)

func svItem(title, uri, parent, subtitle, art string, k itemKind, account string) map[string]any {
	da, acts := 0, 0
	switch k {
	case kindLeaf:
		da, acts = 5, 3
	case kindContainer:
		da, acts = 0, 1
	}
	key := ""
	if art != "" {
		key = uri
	}
	return map[string]any{
		"title": title, "subtitle": subtitle, "uri": uri, "parentUri": parent,
		"artworkUrl": art, "thumbnailUrl": art, "artworkKey": key,
		"defaultAction": da, "actions": acts, "serviceActions": 0, "Uid": "",
		"account": account, "explicit": false, "tempData": "",
		"displayData": map[string]any{"displayType": 0, "data": nil},
	}
}

func serviceInfo(prefix, name, account string) map[string]any {
	return map[string]any{
		"prefix": prefix, "name": name, "landing_url": "", "password_url": "",
		"state": map[string]any{"code": 1, "instance": account, "message": "", "upsell": nil},
		"flow": map[string]any{"title": name, "subtitle": prefix, "loginType": 0, "instanceId": account,
			"fields": nil, "tokens": nil, "version": 0},
		"functions": map[string]any{}, "actions": nil, "settings": nil,
	}
}

// ---------------------------------------------------------------- proxy

type proxy struct {
	upstream string
	rd       *radios   // nil = radios disabled
	ma       *maClient // nil = MA disabled

	mu    sync.Mutex
	conns map[*hostConn]struct{}

	lastPushID atomic.Int64
	// airplay is true while music-go's current item is an AirPlay one. music-go refuses every
	// other play ("no active playback source for uri") until the AirPlay session is closed.
	airplay   atomic.Bool
	lastItems sync.Map // mount -> last music-go now-playing item (map[string]any, flat)

	ownMu  sync.Mutex
	ownIDs map[string]bool // ids of requests the proxy itself sent to music-go
	ownSeq atomic.Int64
}

type hostConn struct {
	id       int64
	p        *proxy
	client   net.Conn
	up       net.Conn
	cmu, umu sync.Mutex

	smu     sync.Mutex
	rootIDs map[any]bool
	servIDs map[any]bool
	infoIDs map[any]bool
	batches map[any][2]any // id -> {total int, positions map[int]bool}
}

var connSeq atomic.Int64

// marshal encodes like json.Marshal but without HTML escaping (&, <, > stay as is).
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func decodeJSON(b []byte) (map[string]any, bool) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return nil, false
	}
	return m, true
}

func idKey(v any) any {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return v
}

func (p *proxy) serve(c net.Conn) {
	cid := connSeq.Add(1)
	cr := bufio.NewReaderSize(c, 64<<10)
	head, err := readHTTPHead(cr)
	if err != nil {
		c.Close()
		return
	}
	logEvent("connect", "c", cid, "peer", c.RemoteAddr().String(), "head", string(head))
	up, err := net.Dial("tcp", p.upstream)
	if err != nil {
		logEvent("upstream_fail", "c", cid, "err", err.Error())
		c.Close()
		return
	}
	ur := bufio.NewReaderSize(up, 64<<10)
	if _, err := up.Write(head); err != nil {
		c.Close()
		up.Close()
		return
	}
	resp, err := readHTTPHead(ur)
	if err != nil {
		c.Close()
		up.Close()
		return
	}
	_, _ = c.Write(resp)
	if !is101(resp) { // plain HTTP: raw relay
		go func() { io.Copy(c, ur); c.Close() }()
		io.Copy(up, cr)
		up.Close()
		return
	}
	hc := &hostConn{id: cid, p: p, client: c, up: up, rootIDs: map[any]bool{}, servIDs: map[any]bool{},
		infoIDs: map[any]bool{}, batches: map[any][2]any{}}
	p.mu.Lock()
	p.conns[hc] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, hc)
		p.mu.Unlock()
	}()
	done := make(chan struct{}, 2)
	go func() { hc.pumpHost(cr); done <- struct{}{} }()
	go func() { hc.pumpMG(ur); done <- struct{}{} }()
	<-done
	c.Close()
	up.Close()
	<-done
}

func (h *hostConn) toClient(b []byte) {
	h.cmu.Lock()
	_, _ = h.client.Write(b)
	h.cmu.Unlock()
}

func (h *hostConn) toUp(b []byte) {
	h.umu.Lock()
	_, _ = h.up.Write(b)
	h.umu.Unlock()
}

func (h *hostConn) sendJSON(m map[string]any, dir string) {
	b := marshal(m)
	L.rec(map[string]any{"c": h.id, "dir": dir, "json": m})
	if dir == "proxy->mg" {
		h.toUp(encodeFrame(1, b, true))
	} else {
		h.toClient(encodeFrame(2, b, false))
	}
}

func (h *hostConn) reply(req map[string]any, data any) {
	h.sendJSON(map[string]any{"query": req["query"], "event": 0, "id": req["id"], "data": data, "source": ""}, "proxy->host")
}

func logFrame(cid int64, dir string, f *frame, j map[string]any) {
	r := map[string]any{"c": cid, "dir": dir, "op": f.op}
	if j != nil {
		r["json"] = j
	} else {
		r["len"] = len(f.payload)
	}
	L.rec(r)
}

func (h *hostConn) pumpHost(r *bufio.Reader) {
	defer logEvent("closed", "c", h.id, "dir", "host->mg")
	for {
		f, err := readFrame(r)
		if err != nil {
			return
		}
		var j map[string]any
		if f.op == 1 || f.op == 2 {
			j, _ = decodeJSON(f.payload)
		}
		logFrame(h.id, "host->mg", f, j)
		out := f.raw
		if j != nil && f.fin {
			act, mod := h.onHost(j)
			if act == drop {
				continue
			}
			if act == modified {
				b := marshal(mod)
				L.rec(map[string]any{"c": h.id, "dir": "proxy->mg", "json": mod})
				out = encodeFrame(f.op, b, true)
			}
		}
		h.toUp(out)
		if f.op == 8 {
			return
		}
	}
}

func (h *hostConn) pumpMG(r *bufio.Reader) {
	defer logEvent("closed", "c", h.id, "dir", "mg->host")
	for {
		f, err := readFrame(r)
		if err != nil {
			return
		}
		var j map[string]any
		if f.op == 1 || f.op == 2 {
			j, _ = decodeJSON(f.payload)
		}
		logFrame(h.id, "mg->host", f, j)
		out := f.raw
		if j != nil && f.fin && asInt(j["event"], -1) == 0 && h.p.isOwn(j["id"]) {
			continue
		}
		if j != nil && f.fin {
			if mod := h.onMG(j); mod != nil {
				b := marshal(mod)
				L.rec(map[string]any{"c": h.id, "dir": "proxy->host", "json": mod})
				out = encodeFrame(f.op, b, false)
			}
		}
		h.toClient(out)
		if f.op == 8 {
			return
		}
	}
}

type verdict int

const (
	forward verdict = iota
	modified
	drop
)

func ours(v any) bool {
	s, ok := v.(string)
	return ok && (strings.HasPrefix(s, radioPrefix+":") || strings.HasPrefix(s, maPrefix+":"))
}

func asInt(v any, def int) int {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
	case float64:
		return int(x)
	}
	return def
}

// onHost inspects a host->music-go request.
func (h *hostConn) onHost(j map[string]any) (verdict, map[string]any) {
	p := h.p
	q, _ := j["query"].(string)
	rid := idKey(j["id"])
	d := j["data"]
	dm, _ := d.(map[string]any)
	switch q {
	case "browse":
		if dm == nil {
			break
		}
		uri, _ := dm["uri"].(string)
		if uri == "root" {
			h.smu.Lock()
			h.rootIDs[rid] = true
			h.smu.Unlock()
			return forward, nil
		}
		if ours(uri) {
			go h.answerBrowse(j, uri, asInt(dm["offset"], 0), asInt(dm["limit"], 50))
			return drop, nil
		}
	case "getServices":
		h.smu.Lock()
		h.servIDs[rid] = true
		h.smu.Unlock()
	case "getServerInfo":
		h.smu.Lock()
		h.infoIDs[rid] = true
		h.smu.Unlock()
	case "getServiceActionSet":
		if dm != nil && ours(dm["uri"]) {
			go h.reply(j, []any{})
			return drop, nil
		}
	case "getServiceActionSetBatch":
		l, ok := d.([]any)
		if !ok {
			break
		}
		pos := map[int]bool{}
		var rest []any
		for i, e := range l {
			if em, ok := e.(map[string]any); ok && ours(em["uri"]) {
				pos[i] = true
			} else {
				rest = append(rest, e)
			}
		}
		if len(pos) == 0 {
			break
		}
		if len(rest) == 0 {
			empty := make([]any, len(l))
			for i := range empty {
				empty[i] = []any{}
			}
			go h.reply(j, empty)
			return drop, nil
		}
		h.smu.Lock()
		h.batches[rid] = [2]any{len(l), pos}
		h.smu.Unlock()
		j2 := cloneMap(j)
		j2["data"] = rest
		return modified, j2
	case "action":
		if dm == nil {
			break
		}
		it, _ := dm["item"].(map[string]any)
		if it == nil {
			break
		}
		uri, _ := it["uri"].(string)
		switch {
		case p.rd != nil && strings.HasPrefix(uri, radioPrefix+":station:"):
			pi, ok := p.rd.playItem(uri)
			if !ok {
				go h.reply(j, map[string]any{"items": nil, "screen": 0, "metadata": nil, "error": "Unknown station."})
				return drop, nil
			}
			p.rd.watch(strings.TrimPrefix(uri, radioPrefix+":station:"))
			j2 := cloneMap(j)
			d2 := cloneMap(dm)
			d2["item"] = pi
			j2["data"] = d2
			if p.ma != nil && p.airplay.Load() {
				// Close MA's AirPlay session first, then forward with the same request id.
				go func() {
					p.ma.stopPlayer()
					time.Sleep(1500 * time.Millisecond)
					h.sendJSON(j2, "proxy->mg")
				}()
				return drop, nil
			}
			return modified, j2
		case p.ma != nil && strings.HasPrefix(uri, "ma:"):
			go h.answerMAAction(j, uri)
			return drop, nil
		}
	case "getTransports", "getLikedState", "getArtwork":
		if ours(d) {
			var v any
			if q == "getLikedState" {
				v = false
			}
			go h.reply(j, v)
			return drop, nil
		}
	}
	return forward, nil
}

func cloneMap(m map[string]any) map[string]any {
	o := make(map[string]any, len(m))
	for k, v := range m {
		o[k] = v
	}
	return o
}

func (h *hostConn) answerBrowse(j map[string]any, uri string, offset, limit int) {
	p := h.p
	var items []map[string]any
	var err error
	switch {
	case p.rd != nil && uri == radioPrefix+":root":
		items = p.rd.list(offset, limit)
	case p.ma != nil && strings.HasPrefix(uri, "ma:"):
		items, err = p.ma.list(uri, offset, limit)
	}
	if err != nil {
		logEvent("browse_error", "c", h.id, "uri", uri, "err", err.Error())
		h.reply(j, map[string]any{"items": []any{}, "screen": 0, "metadata": nil, "error": "Service unavailable."})
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	screen := 0
	for _, it := range items {
		if asInt(it["defaultAction"], 0) == 0 {
			screen = 8
			break
		}
	}
	h.reply(j, map[string]any{"items": items, "screen": screen, "metadata": nil, "error": ""})
}

func (h *hostConn) answerMAAction(j map[string]any, uri string) {
	if !strings.HasPrefix(uri, "ma:obj:") {
		h.reply(j, map[string]any{"items": nil, "screen": 0, "metadata": nil, "error": "This folder cannot be read."})
		return
	}
	// MA's play_media returns only once playback started (4-6 s): acknowledge first.
	h.reply(j, map[string]any{"items": nil, "screen": 0, "metadata": nil, "error": ""})
	if err := h.p.ma.play(uri); err != nil {
		logEvent("ma_error", "c", h.id, "uri", uri, "err", err.Error())
	}
}

// onMG inspects a music-go -> host message; returns a replacement or nil.
func (h *hostConn) onMG(j map[string]any) map[string]any {
	p := h.p
	ev := asInt(j["event"], 0)
	rid := idKey(j["id"])
	if ev != 0 {
		p.lastPushID.Store(int64(asInt(j["id"], 0)))
		p.trackSource(j)
		return p.rewritePush(j)
	}
	q, _ := j["query"].(string)
	h.smu.Lock()
	defer h.smu.Unlock()
	switch q {
	case "browse":
		if !h.rootIDs[rid] {
			return nil
		}
		delete(h.rootIDs, rid)
		dm, _ := j["data"].(map[string]any)
		if dm == nil {
			return nil
		}
		items, _ := dm["items"].([]any)
		var kept []any
		for _, it := range items {
			if im, ok := it.(map[string]any); ok && ours(im["uri"]) {
				continue
			}
			kept = append(kept, it)
		}
		var inject []any
		if p.rd != nil {
			inject = append(inject, p.rd.rootEntry())
		}
		if p.ma != nil {
			inject = append(inject, p.ma.rootEntry())
		}
		at := 1
		if len(kept) < at {
			at = len(kept)
		}
		out := append([]any{}, kept[:at]...)
		out = append(out, inject...)
		out = append(out, kept[at:]...)
		d2 := cloneMap(dm)
		d2["items"] = out
		j2 := cloneMap(j)
		j2["data"] = d2
		return j2
	case "getServices":
		if !h.servIDs[rid] {
			return nil
		}
		delete(h.servIDs, rid)
		l, ok := j["data"].([]any)
		if !ok {
			return nil
		}
		var out []any
		for _, s := range l {
			if s != radioPrefix && s != maPrefix {
				out = append(out, s)
			}
		}
		if p.rd != nil {
			out = append(out, radioPrefix)
		}
		if p.ma != nil {
			out = append(out, maPrefix)
		}
		j2 := cloneMap(j)
		j2["data"] = out
		return j2
	case "getServerInfo":
		if !h.infoIDs[rid] {
			return nil
		}
		delete(h.infoIDs, rid)
		dm, _ := j["data"].(map[string]any)
		svcs, ok := dm["services"].([]any)
		if !ok {
			return nil
		}
		var out []any
		for _, s := range svcs {
			if sm, ok := s.(map[string]any); ok && (sm["prefix"] == radioPrefix || sm["prefix"] == maPrefix) {
				continue
			}
			out = append(out, s)
		}
		if p.rd != nil {
			out = append(out, serviceInfo(radioPrefix, radioTitle, radioAccount))
		}
		if p.ma != nil {
			out = append(out, serviceInfo(maPrefix, maTitle, maAccount))
		}
		if p.rd != nil {
			for _, sv := range svcs {
				if sm, ok := sv.(map[string]any); ok && sm["prefix"] == "tunein" {
					if st, _ := sm["state"].(map[string]any); st != nil && asInt(st["code"], 0) != 1 {
						go h.activateTuneIn()
					}
				}
			}
		}
		d2 := cloneMap(dm)
		d2["services"] = out
		j2 := cloneMap(j)
		j2["data"] = d2
		return j2
	case "getServiceActionSetBatch":
		b, ok := h.batches[rid]
		if !ok {
			return nil
		}
		delete(h.batches, rid)
		total, pos := b[0].(int), b[1].(map[int]bool)
		l, _ := j["data"].([]any)
		out := make([]any, total)
		k := 0
		for i := 0; i < total; i++ {
			if pos[i] || k >= len(l) {
				out[i] = []any{}
				continue
			}
			out[i] = l[k]
			k++
		}
		j2 := cloneMap(j)
		j2["data"] = out
		return j2
	}
	return nil
}

func (p *proxy) isOwn(id any) bool {
	k := fmt.Sprint(idKey(id))
	p.ownMu.Lock()
	defer p.ownMu.Unlock()
	if p.ownIDs[k] {
		delete(p.ownIDs, k)
		return true
	}
	return false
}

// activateTuneIn re-enables music-go's built-in TuneIn service, which customstream playback
// needs. music-go normally does this itself at boot, but after some restarts it doesn't, and
// every play then fails with "no active playback source for uri".
func (h *hostConn) activateTuneIn() {
	id := 900000000 + h.p.ownSeq.Add(1)
	h.p.ownMu.Lock()
	if h.p.ownIDs == nil {
		h.p.ownIDs = map[string]bool{}
	}
	h.p.ownIDs[fmt.Sprint(id)] = true
	h.p.ownMu.Unlock()
	logEvent("tunein_activate", "c", h.id)
	h.sendJSON(map[string]any{"query": "activateService", "event": 0, "id": id, "source": "",
		"data": map[string]any{
			"flow": map[string]any{"title": "TuneIn", "subtitle": "No Account", "loginType": 1,
				"instanceId": "tunein:server_default", "fields": nil, "tokens": nil, "version": 1},
			"save_success": false, "show_failed": true,
		}}, "proxy->mg")
}

// trackSource follows which player music-go is using, from its push events.
func (p *proxy) trackSource(j map[string]any) {
	switch src, _ := j["source"].(string); src {
	case "airplay2", "airplay":
		p.airplay.Store(true)
	case "gstreamer", "internal_stream":
		p.airplay.Store(false)
	}
}

// rewritePush patches now-playing pushes about one of our stations with live metadata.
func (p *proxy) rewritePush(j map[string]any) map[string]any {
	if p.rd == nil {
		return nil
	}
	dm, _ := j["data"].(map[string]any)
	if dm == nil {
		return nil
	}
	var it map[string]any
	nested := false
	if inner, ok := dm["item"].(map[string]any); ok {
		it, nested = cloneMap(inner), true
	} else if _, ok := dm["uri"]; ok {
		it = cloneMap(dm)
	} else {
		return nil
	}
	mount, ok := p.rd.rewriteItem(it)
	if !ok {
		return nil
	}
	p.rd.watch(mount)
	p.lastItems.Store(mount, cloneMap(it))
	j2 := cloneMap(j)
	if nested {
		d2 := cloneMap(dm)
		d2["item"] = it
		j2["data"] = d2
	} else {
		j2["data"] = it
	}
	return j2
}

// pushMeta sends an immediate metadata update (event 8, same shape music-go uses for stream
// metadata) to every connected host when the radio server reports a new song.
func (p *proxy) pushMeta(mount string) {
	v, ok := p.lastItems.Load(mount)
	if !ok {
		return
	}
	it := cloneMap(v.(map[string]any))
	if _, ok := p.rd.rewriteItem(it); !ok {
		return
	}
	p.lastItems.Store(mount, cloneMap(it))
	msg := map[string]any{"query": "", "event": 8, "id": p.lastPushID.Load(), "data": it, "source": "internal_stream"}
	p.mu.Lock()
	conns := make([]*hostConn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.sendJSON(msg, "proxy->host")
	}
}
