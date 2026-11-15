package main

// ICY (Shoutcast/Icecast in-stream) metadata: music-go ignores the StreamTitle of custom streams,
// so when no metadata server is configured the proxy opens the station's stream itself with
// "Icy-MetaData: 1", discards the audio and reads the metadata blocks. This costs a second copy
// of the stream while a station is playing; it stops on its own once music-go stops reporting
// the station (see icyIdle).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// icyIdle: stop reading the stream when music-go has not reported the station for this long
// (it pushes the now-playing item every ~10 s while a custom stream plays).
const icyIdle = 2 * time.Minute

var errIcyIdle = errors.New("idle")

// parseICY extracts "StreamTitle='Artist - Title';StreamUrl='…';" into a nowMeta.
func parseICY(block []byte) (nowMeta, bool) {
	s := strings.TrimRight(string(block), "\x00")
	if !utf8.ValidString(s) { // many old servers send Latin-1
		s = latin1ToUTF8([]byte(s))
	}
	title, ok := icyField(s, "StreamTitle")
	if !ok {
		return nowMeta{}, false
	}
	title = strings.TrimSpace(title)
	m := nowMeta{Song: title}
	if a, t, found := strings.Cut(title, " - "); found {
		m.Artist, m.Title = strings.TrimSpace(a), strings.TrimSpace(t)
	} else {
		m.Title = title
	}
	if u, ok := icyField(s, "StreamUrl"); ok && looksLikeImage(u) {
		m.Cover = u
	}
	return m, true
}

// icyField returns the value of key='value'; — values may contain quotes, so the end is the
// first "';" (or the last quote of the block).
func icyField(s, key string) (string, bool) {
	i := strings.Index(s, key+"='")
	if i < 0 {
		return "", false
	}
	rest := s[i+len(key)+2:]
	if j := strings.Index(rest, "';"); j >= 0 {
		return rest[:j], true
	}
	return strings.TrimSuffix(rest, "'"), true
}

func looksLikeImage(u string) bool {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return false
	}
	p := strings.ToLower(u)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

func latin1ToUTF8(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// followICY reads the stream's metadata until stopped or idle, reconnecting on errors.
func (rd *radios) followICY(s station, stop chan struct{}) {
	backoff := time.Second
	for {
		err := rd.readICY(s, stop)
		select {
		case <-stop:
			return
		default:
		}
		if errors.Is(err, errIcyIdle) {
			logEvent("radio_icy_idle", "mount", s.Mount)
			rd.unwatch(stop)
			return
		}
		logEvent("radio_icy_end", "mount", s.Mount, "err", fmt.Sprint(err))
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if rd.idle() {
			rd.unwatch(stop)
			return
		}
	}
}

func (rd *radios) readICY(s station, stop chan struct{}) error {
	req, _ := http.NewRequest("GET", s.Stream, nil)
	req.Header.Set("Icy-MetaData", "1")
	req.Header.Set("User-Agent", "savant-audio-proxy/"+version)
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	metaint, _ := strconv.Atoi(resp.Header.Get("icy-metaint"))
	if metaint <= 0 {
		return fmt.Errorf("no icy-metaint: the stream sends no ICY metadata")
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			resp.Body.Close()
		case <-done:
		}
	}()
	r := bufio.NewReaderSize(resp.Body, 32<<10)
	block := make([]byte, 255*16)
	for {
		if _, err := io.CopyN(io.Discard, r, int64(metaint)); err != nil {
			return err
		}
		n, err := r.ReadByte()
		if err != nil {
			return err
		}
		if n > 0 {
			b := block[:int(n)*16]
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			if m, ok := parseICY(b); ok { // no cover in the block: display() falls back to the logo
				if rd.setMeta(s.Mount, m) && rd.onMeta != nil {
					rd.onMeta(s.Mount)
				}
			}
		}
		if rd.idle() {
			return errIcyIdle
		}
	}
}
