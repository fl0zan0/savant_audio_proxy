package main

import (
	"strings"
	"testing"
)

func TestParseM3U(t *testing.T) {
	in := "#EXTM3U\n\n#EXTINF:-1 tvg-logo=\"https://x/cover/logo/rac-105.png\",RAC 105\nhttps://radio.example.com/rac-105\n#EXTINF:-1,No logo, with comma\nhttps://radio.example.com/a/b/fip\n"
	st := parseM3U(strings.NewReader(in))
	if len(st) != 2 {
		t.Fatalf("got %d stations", len(st))
	}
	if st[0].Mount != "rac-105" || st[0].Name != "RAC 105" || st[0].Logo != "https://x/cover/logo/rac-105.png" {
		t.Errorf("station 0: %+v", st[0])
	}
	if st[1].Mount != "fip" || st[1].Name != "No logo, with comma" || st[1].Logo != "" {
		t.Errorf("station 1: %+v", st[1])
	}
}

func TestDisplay(t *testing.T) {
	rd := &radios{meta: map[string]nowMeta{}}
	s := station{Mount: "fip-electro", Name: "FIP Électro", Logo: "L"}
	if ti, st, art := rd.display(s); ti != "FIP Électro" || st != "FIP Électro" || art != "L" {
		t.Errorf("no meta: %q %q %q", ti, st, art)
	}
	rd.meta["fip-electro"] = nowMeta{Artist: "Aleem", Title: "Filtri organi", Cover: "C"}
	if ti, st, art := rd.display(s); ti != "Filtri organi" || st != "Aleem — FIP Électro" || art != "C" {
		t.Errorf("meta: %q %q %q", ti, st, art)
	}
}

func TestCustomAccountIsTuneInDefault(t *testing.T) {
	if a := customAccount(station{Stream: "https://radio.example.com/fip-electro"}); a != "tunein:server_default" {
		t.Errorf("account = %s", a)
	}
}

func TestRootEntryIcon(t *testing.T) {
	rd := &radios{stations: []station{{Mount: "a", Name: "A"}, {Mount: "b", Name: "B", Logo: "https://x/b.png"}}}
	if got := rd.rootEntry()["artworkUrl"]; got != "https://x/b.png" {
		t.Fatalf("default icon = %v", got)
	}
	radioIcon = "https://x/tile.png"
	defer func() { radioIcon = "" }()
	if got := rd.rootEntry()["artworkUrl"]; got != "https://x/tile.png" {
		t.Fatalf("custom icon = %v", got)
	}
}

func TestWatchWithoutMetadataServer(t *testing.T) {
	rd := &radios{meta: map[string]nowMeta{}}
	rd.watch("mp3-192")
	if rd.watching != "" || rd.stopW != nil {
		t.Fatalf("watch started without a metadata server: %q", rd.watching)
	}
}
