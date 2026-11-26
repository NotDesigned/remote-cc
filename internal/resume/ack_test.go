package resume

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
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
	writeFrame(y, fPing, []byte{1})
	if f, _ := next(ackDelay / 2); f.typ != fAck || !bytes.Equal(f.p, u64(2)) {
		t.Fatalf("got %d %x, want an ack for the probe at once", f.typ, f.p)
	}
	// A probe is answered even with nothing new to acknowledge.
	writeFrame(y, fPing, []byte{1})
	if f, _ := next(ackDelay / 2); f.typ != fAck || !bytes.Equal(f.p, u64(2)) {
		t.Fatalf("got %d %x, want an answer to the probe", f.typ, f.p)
	}
	// Without a reply or a ping, the ack comes after ackDelay.
	writeFrame(y, fData, []byte("k"))
	start := time.Now()
	if f, _ := next(5 * ackDelay); f.typ != fAck || !bytes.Equal(f.p, u64(3)) || time.Since(start) < ackDelay/2 {
		t.Fatalf("got %d %x after %v", f.typ, f.p, time.Since(start))
	}
}

// An unanswered probe ends the transport after ProbeWait (ErrSilent); an
// answered one does not.
func TestProbe(t *testing.T) {
	a, b := New(Options{ProbeWait: 300 * time.Millisecond}), New(Options{})
	defer a.Close()
	defer b.Close()
	_, _, done := attach(t, a, b)
	a.Probe()
	select {
	case <-done:
		t.Fatalf("an answered probe ended the transport: %v", a.LastError())
	case <-time.After(time.Second):
	}

	x, y := net.Pipe()
	go writeFrame(y, fResume, u64(0))
	go func() { // the other side reads, but never answers
		readFrame(y)
		io.Copy(io.Discard, y)
	}()
	c := New(Options{ProbeWait: 300 * time.Millisecond})
	defer c.Close()
	done, err := c.Attach(x)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c.Probe()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("an unanswered probe did not end the transport")
	}
	if !errors.Is(c.LastError(), ErrSilent) || time.Since(start) > 2*time.Second {
		t.Fatalf("ended after %v: %v", time.Since(start), c.LastError())
	}
}

// A stall of this process (an overloaded computer: the ticker ran late)
// is not silence of the other side: the transport is probed, not closed.
func TestStallIsNotSilence(t *testing.T) {
	opt := Options{PingEvery: 100 * time.Millisecond, DeadAfter: 400 * time.Millisecond, ProbeWait: time.Second}
	a, b := New(opt), New(opt)
	defer a.Close()
	defer b.Close()
	var mu sync.Mutex
	var skew time.Duration
	a.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return time.Now().Add(skew)
	}
	_, _, done := attach(t, a, b)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	skew = 3 * time.Second // as if a's process had not run for 3 s
	mu.Unlock()
	select {
	case <-done:
		t.Fatalf("a stall ended the transport: %v", a.LastError())
	case <-time.After(time.Second):
	}
}

// While this side reads slowly, the other side's pings wait behind the data
// held back: no silence either.
func TestHoldingIsNotSilence(t *testing.T) {
	opt := Options{PingEvery: 100 * time.Millisecond, DeadAfter: 400 * time.Millisecond, MaxUnacked: 1 << 10}
	a, b := New(opt), New(opt)
	defer a.Close()
	defer b.Close()
	_, _, done := attach(t, a, b)
	go b.Write(make([]byte, 4<<10)) // nobody reads a
	select {
	case <-done:
		t.Fatalf("a slow reader ended the transport: %v", a.LastError())
	case <-time.After(1500 * time.Millisecond):
	}
	go io.Copy(io.Discard, a)
	start := time.Now()
	for !b.Flushed() {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the data never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
