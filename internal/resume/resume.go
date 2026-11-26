// Package resume is a reliable byte stream between two endpoints that
// survives the loss and replacement of its transport.
//
// Each side keeps the bytes it wrote until the other side acknowledges them.
// When a new transport is attached, both sides first say how many bytes they
// have received; each then drops what the other already has and resends the
// rest. Every byte is delivered exactly once and in order, whatever happens to
// the transports in between: a command's output or a file write that was in
// flight when the network dropped arrives after the reconnect.
//
// On a transport, frames are: type (1 byte) | length (4 bytes, big endian) |
// payload. Both sides send a ping every PingEvery; a transport that has been
// silent for DeadAfter is closed, so a half-open connection is noticed even
// when the operating system does not report it.
package resume

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	fResume byte = 1 // 8 bytes: total bytes received
	fData   byte = 2 // stream bytes
	fAck    byte = 3 // 8 bytes: total bytes received
	fPing   byte = 4 // empty; the other side acks what it has at once
)

const (
	maxChunk = 32 << 10
	maxFrame = 1 << 20
	// ackDelay: data received is acknowledged with the next data going the
	// other way, or on its own this long after it arrived. A lone ack is a
	// small packet, and on links that hold small packets back (Nagle; ssh
	// does not disable it for non-terminal sessions, nor does a ProxyJump
	// gateway) the reply that follows it waits a round trip: an ack sent
	// right away made every keystroke's echo take two round trips.
	ackDelay    = 200 * time.Millisecond
	ackBytes    = 256 << 10
	tick        = 50 * time.Millisecond
	handshakeTO = 20 * time.Second
)

// ErrClosed is returned by Read, Write and Attach after Close.
var ErrClosed = errors.New("resume: connection closed")

// Options tune a Conn. Zero values mean the defaults.
type Options struct {
	PingEvery time.Duration // default 5s
	DeadAfter time.Duration // default 15s
	// MaxUnacked is how many written bytes may wait for an acknowledgement
	// before Write blocks (also the receive buffer limit). Default 8 MiB.
	MaxUnacked int
}

// Conn is one end of a resumable stream. Read and Write may be used while no
// transport is attached: Write buffers (up to MaxUnacked), Read waits.
type Conn struct {
	opt  Options
	mu   sync.Mutex
	cond *sync.Cond

	buf     []byte // written bytes [acked, written)
	acked   uint64
	written uint64

	rbuf  []byte // received, not yet read
	recvd uint64 // total received

	cur    *attachment
	closed bool
}

// New returns a Conn with no transport attached.
func New(opt Options) *Conn {
	if opt.PingEvery == 0 {
		opt.PingEvery = 5 * time.Second
	}
	if opt.DeadAfter == 0 {
		opt.DeadAfter = 15 * time.Second
	}
	if opt.MaxUnacked == 0 {
		opt.MaxUnacked = 8 << 20
	}
	c := &Conn{opt: opt}
	c.cond = sync.NewCond(&c.mu)
	return c
}

type attachment struct {
	tr       io.ReadWriteCloser
	sentTo   uint64 // stream offset written on this transport so far
	ackSent  uint64
	ackDue   time.Time // when a lone ack is due, if recvd > ackSent
	pingNow  bool      // send a ping at once (RequestAck)
	lastSend time.Time
	lastRecv time.Time
	stopped  bool
	err      error
	done     chan struct{}
	wg       sync.WaitGroup
}

// stop ends the attachment (once); c.mu must be held.
func (c *Conn) stop(a *attachment, err error) {
	if a.stopped {
		return
	}
	a.stopped, a.err = true, err
	a.tr.Close()
	close(a.done)
	if c.cur == a {
		c.cur = nil
	}
	c.cond.Broadcast()
}

// Attach makes tr the transport, replacing (and closing) the current one.
// It returns once both sides have resumed, with a channel that is closed when
// this transport fails or is replaced. Err tells why.
func (c *Conn) Attach(tr io.ReadWriteCloser) (done <-chan struct{}, err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		tr.Close()
		return nil, ErrClosed
	}
	old := c.cur
	if old != nil {
		c.stop(old, errors.New("resume: transport replaced"))
	}
	c.mu.Unlock()
	if old != nil {
		old.wg.Wait() // its reader no longer changes recvd
	}

	c.mu.Lock()
	recvd := c.recvd
	c.mu.Unlock()
	br := bufio.NewReaderSize(tr, 64<<10)
	peer, err := handshake(tr, br, recvd)
	if err != nil {
		tr.Close()
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		tr.Close()
		return nil, ErrClosed
	}
	if c.cur != nil { // a concurrent Attach won
		tr.Close()
		return nil, errors.New("resume: transport replaced")
	}
	if peer < c.acked || peer > c.written {
		tr.Close()
		return nil, fmt.Errorf("resume: peer has %d bytes, this side keeps [%d, %d]: not the same stream", peer, c.acked, c.written)
	}
	c.buf = c.buf[peer-c.acked:]
	c.acked = peer
	now := time.Now()
	a := &attachment{tr: tr, sentTo: peer, ackSent: recvd, lastSend: now, lastRecv: now, done: make(chan struct{})}
	c.cur = a
	c.cond.Broadcast()
	a.wg.Add(3)
	go c.reader(a, br)
	go c.writer(a)
	go c.ticker(a)
	return a.done, nil
}

func handshake(tr io.ReadWriteCloser, br *bufio.Reader, recvd uint64) (uint64, error) {
	werr := make(chan error, 1)
	go func() { werr <- writeFrame(tr, fResume, u64(recvd)) }()
	type res struct {
		n   uint64
		err error
	}
	rres := make(chan res, 1)
	go func() {
		typ, p, err := readFrame(br)
		if err == nil && (typ != fResume || len(p) != 8) {
			err = fmt.Errorf("resume: expected a resume frame, got type %d", typ)
		}
		if err != nil {
			rres <- res{err: err}
			return
		}
		rres <- res{n: binary.BigEndian.Uint64(p)}
	}()
	timer := time.NewTimer(handshakeTO)
	defer timer.Stop()
	var r res
	select {
	case r = <-rres:
	case <-timer.C:
		tr.Close()
		return 0, errors.New("resume: no answer from the other side")
	}
	if r.err != nil {
		return 0, r.err
	}
	select {
	case err := <-werr:
		if err != nil {
			return 0, err
		}
	case <-timer.C:
		tr.Close()
		return 0, errors.New("resume: could not write to the other side")
	}
	return r.n, nil
}

func (c *Conn) reader(a *attachment, br *bufio.Reader) {
	defer a.wg.Done()
	for {
		typ, p, err := readFrame(br)
		c.mu.Lock()
		if err != nil {
			c.stop(a, err)
			c.mu.Unlock()
			return
		}
		if a.stopped {
			c.mu.Unlock()
			return
		}
		a.lastRecv = time.Now()
		switch typ {
		case fData:
			// Hold back while the reader of this Conn is far behind.
			for len(c.rbuf) >= c.opt.MaxUnacked && !a.stopped && !c.closed {
				c.cond.Wait()
			}
			if a.stopped || c.closed {
				c.mu.Unlock()
				return
			}
			if c.recvd == a.ackSent {
				a.ackDue = a.lastRecv.Add(ackDelay)
			}
			c.rbuf = append(c.rbuf, p...)
			c.recvd += uint64(len(p))
		case fAck:
			if len(p) == 8 {
				if n := binary.BigEndian.Uint64(p); n > c.acked && n <= c.written {
					c.buf = c.buf[n-c.acked:]
					c.acked = n
				}
			}
		case fPing:
			if c.recvd != a.ackSent { // the other side waits for an ack
				a.ackDue = a.lastRecv
			}
		default:
			c.stop(a, fmt.Errorf("resume: unknown frame type %d", typ))
			c.mu.Unlock()
			return
		}
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

func (c *Conn) writer(a *attachment) {
	defer a.wg.Done()
	bw := bufio.NewWriterSize(a.tr, 64<<10)
	for {
		c.mu.Lock()
		var data []byte
		var ack bool
		var ackN uint64
		var ping bool
		for {
			if a.stopped {
				c.mu.Unlock()
				return
			}
			now := time.Now()
			if a.sentTo < c.written {
				start := a.sentTo - c.acked
				end := min(start+maxChunk, uint64(len(c.buf)))
				data = append([]byte(nil), c.buf[start:end]...)
				a.sentTo += uint64(len(data))
			}
			// Acks ride along with data, else wait (see ackDelay).
			if c.recvd != a.ackSent && (data != nil || !now.Before(a.ackDue) || c.recvd-a.ackSent >= ackBytes) {
				ack, ackN = true, c.recvd
				a.ackSent, a.ackDue = c.recvd, time.Time{}
			}
			if now.Sub(a.lastSend) >= c.opt.PingEvery || a.pingNow {
				ping, a.pingNow = true, false
			}
			if data != nil || ack || ping {
				a.lastSend = now
				break
			}
			c.cond.Wait()
		}
		c.mu.Unlock()

		var err error
		if data != nil {
			err = writeFrame(bw, fData, data)
		}
		if err == nil && ack {
			err = writeFrame(bw, fAck, u64(ackN))
		}
		if err == nil && ping {
			err = writeFrame(bw, fPing, nil)
		}
		if err == nil {
			err = bw.Flush()
		}
		if err != nil {
			c.mu.Lock()
			c.stop(a, err)
			c.mu.Unlock()
			return
		}
	}
}

// ticker wakes the writer for acks and pings, and closes a silent transport.
func (c *Conn) ticker(a *attachment) {
	defer a.wg.Done()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-t.C:
		}
		c.mu.Lock()
		if time.Since(a.lastRecv) > c.opt.DeadAfter {
			c.stop(a, fmt.Errorf("resume: nothing heard from the other side for %v", c.opt.DeadAfter))
		}
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

// Read reads stream bytes, waiting (also across reconnects) until some arrive.
func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.rbuf) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.rbuf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	if len(c.rbuf) == 0 {
		c.rbuf = nil
	}
	c.cond.Broadcast()
	return n, nil
}

// Write queues p for the other side. It blocks while MaxUnacked bytes are
// waiting for an acknowledgement (e.g. while no transport is attached).
func (c *Conn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for len(p) > 0 {
		for !c.closed && len(c.buf) >= c.opt.MaxUnacked {
			c.cond.Wait()
		}
		if c.closed {
			return n, ErrClosed
		}
		k := min(c.opt.MaxUnacked-len(c.buf), len(p))
		c.buf = append(c.buf, p[:k]...)
		c.written += uint64(k)
		p, n = p[k:], n+k
		c.cond.Broadcast()
	}
	return n, nil
}

// Close ends the stream on this side and closes the transport.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.cur != nil {
		c.stop(c.cur, ErrClosed)
	}
	c.cond.Broadcast()
	return nil
}

// RequestAck asks the other side to acknowledge what it has received now,
// rather than after its ack delay (see Flushed).
func (c *Conn) RequestAck() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil {
		c.cur.pingNow = true
		c.cond.Broadcast()
	}
}

// Flushed reports whether everything written so far has been acknowledged.
func (c *Conn) Flushed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acked == c.written
}

// Attached reports whether a transport is currently attached.
func (c *Conn) Attached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur != nil
}

func u64(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

func writeFrame(w io.Writer, typ byte, p []byte) error {
	var h [5]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if len(p) > 0 {
		_, err := w.Write(p)
		return err
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("resume: frame of %d bytes", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}
