package main

// Minimal RFC 6455 framing (no extensions, no fragmentation reassembly needed: every frame
// seen between the Savant host and music-go is a single FIN frame).

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

type frame struct {
	fin     bool
	op      byte
	payload []byte // unmasked
	raw     []byte // exactly as received (still masked if it was)
}

func readFrame(r *bufio.Reader) (*frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	raw := []byte{h[0], h[1]}
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return nil, err
		}
		raw = append(raw, e[:]...)
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return nil, err
		}
		raw = append(raw, e[:]...)
		n = binary.BigEndian.Uint64(e[:])
	}
	if n > 64<<20 {
		return nil, fmt.Errorf("frame too large: %d", n)
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return nil, err
		}
		raw = append(raw, key[:]...)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	raw = append(raw, p...)
	if masked {
		for i := range p {
			p[i] ^= key[i%4]
		}
	}
	return &frame{fin: h[0]&0x80 != 0, op: h[0] & 0x0f, payload: p, raw: raw}, nil
}

func encodeFrame(op byte, payload []byte, mask bool) []byte {
	n := len(payload)
	out := []byte{0x80 | op}
	var mbit byte
	if mask {
		mbit = 0x80
	}
	switch {
	case n < 126:
		out = append(out, mbit|byte(n))
	case n < 65536:
		out = append(out, mbit|126, byte(n>>8), byte(n))
	default:
		var e [8]byte
		binary.BigEndian.PutUint64(e[:], uint64(n))
		out = append(out, mbit|127)
		out = append(out, e[:]...)
	}
	if !mask {
		return append(out, payload...)
	}
	var key [4]byte
	_, _ = rand.Read(key[:])
	out = append(out, key[:]...)
	start := len(out)
	out = append(out, payload...)
	for i := start; i < len(out); i++ {
		out[i] ^= key[(i-start)%4]
	}
	return out
}

// readHTTPHead reads up to and including the blank line ending an HTTP header block.
func readHTTPHead(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		line, err := r.ReadSlice('\n')
		buf = append(buf, line...)
		if err != nil {
			return buf, err
		}
		if len(buf) > 64<<10 {
			return buf, errors.New("header too large")
		}
		if strings.HasSuffix(string(buf), "\r\n\r\n") {
			return buf, nil
		}
	}
}

func is101(head []byte) bool {
	first := strings.SplitN(string(head), "\r\n", 2)[0]
	return strings.Contains(first, " 101 ")
}

// wsDial opens a client WebSocket connection to ws://addr/path.
func wsDial(addr, path string, timeout time.Duration) (net.Conn, *bufio.Reader, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, nil, err
	}
	var k [16]byte
	_, _ = rand.Read(k[:])
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, addr, base64.StdEncoding.EncodeToString(k[:]))
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, nil, err
	}
	r := bufio.NewReaderSize(c, 64<<10)
	head, err := readHTTPHead(r)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if !is101(head) {
		c.Close()
		return nil, nil, fmt.Errorf("upgrade refused: %s", strings.SplitN(string(head), "\r\n", 2)[0])
	}
	_ = c.SetDeadline(time.Time{})
	return c, r, nil
}
