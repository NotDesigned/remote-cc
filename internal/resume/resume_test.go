package resume

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// attach connects a and b through a fresh in-memory transport.
func attach(t *testing.T, a, b *Conn) (net.Conn, net.Conn, <-chan struct{}) {
	t.Helper()
	x, y := net.Pipe()
	errs := make(chan error, 2)
	var doneA <-chan struct{}
	go func() {
		var err error
		doneA, err = a.Attach(x)
		errs <- err
	}()
	go func() { _, err := b.Attach(y); errs <- err }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	return x, y, doneA
}

func TestEcho(t *testing.T) {
	a, b := New(Options{}), New(Options{})
	defer a.Close()
	defer b.Close()
	attach(t, a, b)
	if _, err := a.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(b, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("%q %v", buf, err)
	}
}

// Data written in both directions arrives exactly once and in order, while
// the transport is cut again and again, also mid-frame.
func TestSurvivesTransportLoss(t *testing.T) {
	a, b := New(Options{MaxUnacked: 256 << 10}), New(Options{MaxUnacked: 256 << 10})
	defer a.Close()
	defer b.Close()
	const size = 8 << 20
	dataAB, dataBA := random(size), random(size)

	var wg sync.WaitGroup
	got := map[string][32]byte{}
	var mu sync.Mutex
	send := func(c *Conn, d []byte) {
		defer wg.Done()
		for off := 0; off < len(d); {
			n := min(len(d)-off, 7919) // odd sizes, to split frames anywhere
			if _, err := c.Write(d[off : off+n]); err != nil {
				t.Error(err)
				return
			}
			off += n
		}
	}
	recv := func(name string, c *Conn) {
		defer wg.Done()
		h := sha256.New()
		if _, err := io.CopyN(h, c, size); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		var s [32]byte
		copy(s[:], h.Sum(nil))
		got[name] = s
		mu.Unlock()
	}
	wg.Add(4)
	go send(a, dataAB)
	go send(b, dataBA)
	go recv("ab", b)
	go recv("ba", a)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	for cuts := 0; ; cuts++ {
		x, y, _ := attach(t, a, b)
		select {
		case <-finished:
			if cuts < 3 {
				t.Logf("only %d cuts happened before the transfer finished", cuts)
			}
			mu.Lock()
			defer mu.Unlock()
			if got["ab"] != sha256.Sum256(dataAB) || got["ba"] != sha256.Sum256(dataBA) {
				t.Fatal("data differs after reconnects")
			}
			return
		case <-time.After(30 * time.Millisecond):
		}
		// Cut: either side may notice first.
		if cuts%2 == 0 {
			x.Close()
		} else {
			y.Close()
		}
		if cuts > 2000 {
			t.Fatal("transfer did not finish")
		}
	}
}

// While no transport is attached, Write buffers up to MaxUnacked and then
// blocks; everything arrives after the next attach.
func TestBuffersWhileDetached(t *testing.T) {
	a, b := New(Options{MaxUnacked: 1024}), New(Options{MaxUnacked: 1024})
	defer a.Close()
	defer b.Close()
	wrote := make(chan struct{})
	go func() {
		a.Write(bytes.Repeat([]byte("x"), 3000))
		close(wrote)
	}()
	select {
	case <-wrote:
		t.Fatal("Write did not block with no transport and a full buffer")
	case <-time.After(100 * time.Millisecond):
	}
	attach(t, a, b)
	buf := make([]byte, 3000)
	if _, err := io.ReadFull(b, buf); err != nil {
		t.Fatal(err)
	}
	<-wrote
}

// A transport that stays silent is closed after DeadAfter.
func TestSilentTransport(t *testing.T) {
	a := New(Options{PingEvery: time.Hour, DeadAfter: 200 * time.Millisecond})
	defer a.Close()
	x, y := net.Pipe()
	go func() { // the other side answers the handshake, then says nothing
		readFrame(y)
		writeFrame(y, fResume, u64(0))
		io.Copy(io.Discard, y)
	}()
	done, err := a.Attach(x)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("silent transport was not closed")
	}
}

// Attaching a different stream (the other side does not have what this side
// says it acknowledged) fails instead of corrupting data.
func TestRefusesForeignStream(t *testing.T) {
	a, b := New(Options{}), New(Options{})
	defer a.Close()
	defer b.Close()
	attach(t, a, b)
	a.Write([]byte("0123456789"))
	io.ReadFull(b, make([]byte, 10))
	for !a.Flushed() { // the ack reaches a
		time.Sleep(20 * time.Millisecond)
	}

	fresh := New(Options{}) // knows nothing of the 10 bytes
	defer fresh.Close()
	x, y := net.Pipe()
	errs := make(chan error, 2)
	go func() { _, err := a.Attach(x); errs <- err }()
	go func() { _, err := fresh.Attach(y); errs <- err }()
	if e1, e2 := <-errs, <-errs; e1 == nil && e2 == nil {
		t.Fatal("a stream that lost data was resumed")
	}
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}
