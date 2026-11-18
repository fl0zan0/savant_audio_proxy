package main

// Local logo cache: tile images and station logos are downloaded once into a folder on the
// Savant audio server and served from there over HTTP (http://<unit IP>:<port>/logos/<file>),
// so the Savant app shows them without depending on the remote sites. Until a logo is
// downloaded (or if the download fails), the original URL is used.

import (
	"crypto/sha1"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// logos is nil when the local cache is disabled.
var logos *logoCache

type logoCache struct {
	dir  string
	port string // HTTP port the logos are served on
	host string // fixed host for the URLs; empty = this unit's LAN IP

	mu      sync.Mutex
	files   map[string]string    // original URL -> file name in dir
	pending map[string]bool      // downloads in progress
	fetched map[string]time.Time // last successful download

	ipMu sync.Mutex
	ip   string
	ipAt time.Time
}

const logoRefresh = 7 * 24 * time.Hour

func newLogoCache(dir, listen, host string) (*logoCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	lc := &logoCache{dir: dir, port: port, host: host,
		files: map[string]string{}, pending: map[string]bool{}, fetched: map[string]time.Time{}}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/logos/", lc.serve)
	go http.Serve(ln, mux)
	return lc, nil
}

// fileName is the cache file of an URL: a hash of the URL, so a new URL gives a new file.
func fileName(u string) string {
	h := sha1.Sum([]byte(u))
	return fmt.Sprintf("%x", h[:10])
}

// url returns the local URL of a logo when it is cached, else the original URL (and starts
// downloading it). Safe on a nil cache.
func (lc *logoCache) url(u string) string {
	if lc == nil || !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
		return u
	}
	lc.mu.Lock()
	name, ok := lc.files[u]
	if !ok {
		name = lc.findFile(u)
		if name != "" {
			lc.files[u] = name
			ok = true
		}
	}
	stale := time.Since(lc.fetched[u]) > logoRefresh
	lc.mu.Unlock()
	if !ok || stale {
		lc.fetch(u)
	}
	if !ok {
		return u
	}
	host := lc.host
	if host == "" {
		host = lc.localIP()
	}
	if host == "" {
		return u
	}
	return "http://" + net.JoinHostPort(host, lc.port) + "/logos/" + name
}

// findFile looks for an already downloaded file of this URL (any extension). Called with mu held.
func (lc *logoCache) findFile(u string) string {
	m, _ := filepath.Glob(filepath.Join(lc.dir, fileName(u)+".*"))
	for _, p := range m {
		if !strings.HasSuffix(p, ".tmp") {
			return filepath.Base(p)
		}
	}
	return ""
}

// prefetch downloads these logos in the background (startup, station list reload).
func (lc *logoCache) prefetch(urls ...string) {
	for _, u := range urls {
		lc.url(u)
	}
}

func (lc *logoCache) fetch(u string) {
	lc.mu.Lock()
	if lc.pending[u] {
		lc.mu.Unlock()
		return
	}
	lc.pending[u] = true
	lc.mu.Unlock()
	go func() {
		logoSlots <- struct{}{}
		name, err := lc.download(u)
		<-logoSlots
		lc.mu.Lock()
		delete(lc.pending, u)
		if err == nil {
			lc.files[u] = name
			lc.fetched[u] = time.Now()
		} else {
			// keep any old file; retry in an hour at the earliest
			lc.fetched[u] = time.Now().Add(-logoRefresh + time.Hour)
		}
		lc.mu.Unlock()
		if err != nil {
			logEvent("logo_error", "url", u, "err", err.Error())
		} else {
			logEvent("logo_cached", "url", u, "file", name)
		}
	}()
}

var logoSlots = make(chan struct{}, 4) // parallel downloads

var logoClient = &http.Client{Timeout: 30 * time.Second}

func (lc *logoCache) download(u string) (string, error) {
	resp, err := logoClient.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif",
		"image/webp": ".webp", "image/svg+xml": ".svg", "image/avif": ".avif"}[ct]
	if ext == "" {
		if !strings.HasPrefix(ct, "image/") && ct != "application/octet-stream" && ct != "" {
			return "", fmt.Errorf("not an image: %s", ct)
		}
		ext = ".img"
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", fmt.Errorf("empty")
	}
	base := fileName(u)
	name := base + ext
	tmp := filepath.Join(lc.dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(lc.dir, name)); err != nil {
		return "", err
	}
	// remove the same logo saved with another extension
	m, _ := filepath.Glob(filepath.Join(lc.dir, base+".*"))
	for _, p := range m {
		if filepath.Base(p) != name {
			os.Remove(p)
		}
	}
	return name, nil
}

func (lc *logoCache) serve(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/logos/")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(name) {
	case ".img":
		w.Header().Set("Content-Type", "application/octet-stream")
	case ".avif":
		w.Header().Set("Content-Type", "image/avif")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, filepath.Join(lc.dir, name))
}

// localIP is this unit's LAN address (the one used to reach other hosts), re-checked every minute.
func (lc *logoCache) localIP() string {
	lc.ipMu.Lock()
	defer lc.ipMu.Unlock()
	if lc.ip != "" && time.Since(lc.ipAt) < time.Minute {
		return lc.ip
	}
	ip := ""
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil { // no packet is sent
		ip = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	}
	if ip == "" {
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
				ip = n.IP.String()
				break
			}
		}
	}
	if ip != "" {
		lc.ip, lc.ipAt = ip, time.Now()
	}
	return ip
}
