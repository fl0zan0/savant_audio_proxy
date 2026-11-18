package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogoCache(t *testing.T) {
	hits := 0
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNGDATA"))
	}))
	defer src.Close()
	lc, err := newLogoCache(t.TempDir(), "127.0.0.1:0", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	u := src.URL + "/a.png"
	if got := lc.url(u); got != u {
		t.Fatalf("before download: %s", got)
	}
	var local string
	for i := 0; i < 50; i++ {
		if local = lc.url(u); local != u {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(local, "http://127.0.0.1:0/logos/") || !strings.HasSuffix(local, ".png") {
		t.Fatalf("local url: %s", local)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
	// serve
	rec := httptest.NewRecorder()
	lc.serve(rec, httptest.NewRequest("GET", strings.TrimPrefix(local, "http://127.0.0.1:0"), nil))
	b, _ := io.ReadAll(rec.Body)
	if string(b) != "PNGDATA" || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("serve: %q %s", b, rec.Header().Get("Content-Type"))
	}
	// a new cache on the same folder finds the file without waiting
	lc2 := &logoCache{dir: lc.dir, port: "1", host: "h", files: map[string]string{}, pending: map[string]bool{}, fetched: map[string]time.Time{}}
	if got := lc2.url(u); !strings.HasPrefix(got, "http://h:1/logos/") {
		t.Fatalf("restart: %s", got)
	}
	var nilc *logoCache
	if nilc.url(u) != u {
		t.Fatal("nil cache")
	}
}
