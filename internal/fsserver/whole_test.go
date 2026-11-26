package fsserver

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"testing"
)

type writes struct {
	io.ReadWriteCloser
	calls [][]byte
}

func (w *writes) Write(p []byte) (int, error) {
	w.calls = append(w.calls, append([]byte(nil), p...))
	return len(p), nil
}

func packet(body string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	return append(b, body...)
}

func TestWholePacketsOneWriteEach(t *testing.T) {
	w := &writes{}
	wp := &wholePackets{ReadWriteCloser: w}
	p := packet("\x67header-and-data")
	wp.Write(p[:9]) // pkg/sftp: header, then data
	if len(w.calls) != 0 {
		t.Fatalf("a partial packet went out: %q", w.calls)
	}
	wp.Write(p[9:])
	if len(w.calls) != 1 || !bytes.Equal(w.calls[0], p) {
		t.Fatalf("writes %q, want one %q", w.calls, p)
	}
}

func TestWholePacketsKeepsTheStream(t *testing.T) {
	var all []byte
	for i := 0; i < 200; i++ {
		all = append(all, packet(string(bytes.Repeat([]byte{byte(i)}, rand.Intn(300))))...)
	}
	w := &writes{}
	wp := &wholePackets{ReadWriteCloser: w}
	for rest := all; len(rest) > 0; {
		n := min(len(rest), 1+rand.Intn(100))
		if k, err := wp.Write(rest[:n]); k != n || err != nil {
			t.Fatal(k, err)
		}
		rest = rest[n:]
	}
	if got := bytes.Join(w.calls, nil); !bytes.Equal(got, all) {
		t.Fatal("the byte stream changed")
	}
	for _, c := range w.calls { // every write ends on a packet boundary
		for len(c) > 0 {
			n := 4 + int(binary.BigEndian.Uint32(c))
			if n > len(c) {
				t.Fatal("a write split a packet")
			}
			c = c[n:]
		}
	}
}
