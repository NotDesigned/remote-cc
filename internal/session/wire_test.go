package session

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

type recordWrites struct {
	io.ReadWriteCloser
	calls [][]byte
}

func (w *recordWrites) Write(p []byte) (int, error) {
	w.calls = append(w.calls, append([]byte(nil), p...))
	return len(p), nil
}

func yamuxHeader(typ byte, length uint32) []byte {
	h := make([]byte, yamuxHeaderSize)
	h[1] = typ
	binary.BigEndian.PutUint32(h[8:], length)
	return h
}

func TestWholeFrames(t *testing.T) {
	r := &recordWrites{}
	w := &wholeFrames{ReadWriteCloser: r}
	// A data frame: header and body in one write.
	w.Write(yamuxHeader(yamuxTypeData, 3))
	if len(r.calls) != 0 {
		t.Fatal("the header went out alone")
	}
	w.Write([]byte("abc"))
	// A window update (no body) and an empty data frame (flags only) go at once.
	w.Write(yamuxHeader(1, 1<<20))
	w.Write(yamuxHeader(yamuxTypeData, 0))
	want := [][]byte{append(yamuxHeader(yamuxTypeData, 3), "abc"...), yamuxHeader(1, 1<<20), yamuxHeader(yamuxTypeData, 0)}
	if len(r.calls) != len(want) {
		t.Fatalf("%d writes, want %d", len(r.calls), len(want))
	}
	for i := range want {
		if !bytes.Equal(r.calls[i], want[i]) {
			t.Fatalf("write %d: %x, want %x", i, r.calls[i], want[i])
		}
	}
}
