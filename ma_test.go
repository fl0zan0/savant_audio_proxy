package main

import (
	"net/url"
	"testing"
)

func TestMAMediaType(t *testing.T) {
	for uri, want := range map[string]string{
		"ma:obj:" + url.QueryEscape("library://track/192"):  "track",
		"ma:obj:" + url.QueryEscape("spotify://album/abc"):  "album",
		"ma:obj:" + url.QueryEscape("library://playlist/7"): "playlist",
		"ma:lib:tracks": "",
	} {
		if got := maMediaType(uri); got != want {
			t.Errorf("maMediaType(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestTracksFrom(t *testing.T) {
	m := newMA("x", "", "p")
	page := func(ids ...string) []map[string]any {
		var l []map[string]any
		for _, id := range ids {
			l = append(l, map[string]any{"uri": "ma:obj:" + url.QueryEscape("library://track/"+id)})
		}
		return l
	}
	m.rememberTracks(page("1", "2", "3"), 0)
	m.rememberTracks(page("4", "5"), 3)
	got := m.tracksFrom("ma:obj:"+url.QueryEscape("library://track/4"), 100)
	if len(got) != 2 || got[0] != "library://track/4" || got[1] != "library://track/5" {
		t.Fatalf("tracksFrom = %v", got)
	}
	if got := m.tracksFrom("ma:obj:"+url.QueryEscape("library://track/2"), 2); len(got) != 2 || got[1] != "library://track/3" {
		t.Fatalf("tracksFrom max = %v", got)
	}
	if v, _ := m.parents.Load("ma:obj:" + url.QueryEscape("library://track/5")); v != "ma:lib:tracks" {
		t.Fatalf("parent = %v", v)
	}
	m.rememberTracks(page("9"), 10) // gap: forget the list
	if got := m.tracksFrom("ma:obj:"+url.QueryEscape("library://track/1"), 100); got != nil {
		t.Fatalf("after gap = %v", got)
	}
}
