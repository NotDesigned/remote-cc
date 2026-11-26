package resume

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// Received data is acknowledged with the reply, not with a lone ack first:
// the reply would wait a round trip behind it on a Nagle link. A ping is
// acknowledged at once.
func TestAckRidesWithReply(t *testing.T) {
	a := New(Options{})
	defer a.Close()
	x, y := net.Pipe()
	defer y.Close()
	go writeFrame(y, fResume, u64(0))
	errs := make(chan error, 1)
	go func() { _, err := a.Attach(x); errs <- err }()
	if typ, _, err := readFrame(y); err != nil || typ != fResume {
		t.Fatal(typ, err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	type frame struct {
		typ byte
		p   []byte
	}
	frames := make(chan frame, 16)
	go func() {
		for {
			typ, p, err := readFrame(y)
			if err != nil {
				close(frames)
				return
			}
			if typ != fPing {
				frames <- frame{typ, p}
			}
		}
	}()
	next := func(within time.Duration) (frame, bool) {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("transport closed")
			}
			return f, true
		case <-time.After(within):
			return frame{}, false
		}
	}

	writeFrame(y, fData, []byte("k"))
	if f, ok := next(ackDelay / 2); ok {
		t.Fatalf("frame %d sent before the reply", f.typ)
	}
	io.ReadFull(a, make([]byte, 1))
	a.Write([]byte("e"))
	if f, _ := next(time.Second); f.typ != fData || string(f.p) != "e" {
		t.Fatalf("got %d %q, want the reply", f.typ, f.p)
	}
	if f, _ := next(time.Second); f.typ != fAck || !bytes.Equal(f.p, u64(1)) {
		t.Fatalf("got %d %x, want the ack with the reply", f.typ, f.p)
	}

	writeFrame(y, fData, []byte("k"))
	writeFrame(y, fPing, nil)
	if f, _ := next(ackDelay / 2); f.typ != fAck || !bytes.Equal(f.p, u64(2)) {
		t.Fatalf("got %d %x, want an ack for the ping at once", f.typ, f.p)
	}
	// Without a reply or a ping, the ack comes after ackDelay.
	writeFrame(y, fData, []byte("k"))
	start := time.Now()
	if f, _ := next(5 * ackDelay); f.typ != fAck || !bytes.Equal(f.p, u64(3)) || time.Since(start) < ackDelay/2 {
		t.Fatalf("got %d %x after %v", f.typ, f.p, time.Since(start))
	}
}
