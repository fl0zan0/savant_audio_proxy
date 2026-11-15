package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// JSON-lines frame log with a simple size cap (one rotated copy), so it can't fill /data.
type jlog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	max  int64
}

var L = &jlog{}

func (l *jlog) open(path string, max int64) error {
	l.path, l.max = path, max
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_ = f.Chmod(0o644) // readable by the RPM login user for debugging
	st, _ := f.Stat()
	l.f, l.size = f, st.Size()
	return nil
}

func (l *jlog) rec(m map[string]any) {
	m["t"] = float64(time.Now().UnixMilli()) / 1000
	b := append(marshal(m), '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		os.Stdout.Write(b)
		return
	}
	if l.max > 0 && l.size+int64(len(b)) > l.max {
		l.f.Close()
		_ = os.Rename(l.path, l.path+".1")
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			l.f = nil
			return
		}
		l.f, l.size = f, 0
	}
	n, _ := l.f.Write(b)
	l.size += int64(n)
}

func logEvent(event string, kv ...any) {
	m := map[string]any{"event": event}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	L.rec(m)
}
