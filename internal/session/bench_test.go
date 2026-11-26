package session

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"remote-cc/internal/resume"
)

// muxPair is both ends of the session's stack (resumable stream, yamux)
// over a local TCP connection.
func muxPair(t testing.TB) (client, server *yamux.Session) {
	return muxPairDelay(t, 0)
}

// delayConn delivers what is written to it after d.
type delayConn struct {
	net.Conn
	ch chan delayed
}

type delayed struct {
	at time.Time
	b  []byte
}

func newDelayConn(c net.Conn, d time.Duration) net.Conn {
	if d == 0 {
		return c
	}
	dc := &delayConn{Conn: c, ch: make(chan delayed, 1<<16)}
	go func() {
		for x := range dc.ch {
			time.Sleep(time.Until(x.at))
			if _, err := c.Write(x.b); err != nil {
				return
			}
		}
	}()
	_ = d
	return &delayWriter{dc, d}
}

type delayWriter struct {
	*delayConn
	d time.Duration
}

func (w *delayWriter) Write(p []byte) (int, error) {
	w.ch <- delayed{time.Now().Add(w.d), append([]byte(nil), p...)}
	return len(p), nil
}

func muxPairDelay(t testing.TB, oneWay time.Duration) (client, server *yamux.Session) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); acc <- c }()
	a, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-acc
	a, b = newDelayConn(a, oneWay), newDelayConn(b, oneWay)
	la, lb := resume.New(resume.Options{}), resume.New(resume.Options{})
	errs := make(chan error, 2)
	go func() { _, err := la.Attach(a); errs <- err }()
	go func() { _, err := lb.Attach(b); errs <- err }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	client, _ = yamux.Client(&wholeFrames{ReadWriteCloser: la}, muxConfig())
	server, _ = yamux.Server(&wholeFrames{ReadWriteCloser: lb}, muxConfig())
	t.Cleanup(func() { client.Close(); server.Close(); la.Close(); lb.Close() })
	return client, server
}

func BenchmarkStreamThroughput(b *testing.B) {
	c, s := muxPair(b)
	go func() {
		st, err := s.AcceptStream()
		if err != nil {
			return
		}
		io.Copy(io.Discard, st)
	}()
	st, err := c.OpenStream()
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 32<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamThroughput25ms(b *testing.B) {
	c, s := muxPairDelay(b, 25*time.Millisecond)
	go func() {
		st, err := s.AcceptStream()
		if err != nil {
			return
		}
		io.Copy(io.Discard, st)
	}()
	st, err := c.OpenStream()
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 32<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamRoundTrip(b *testing.B) {
	c, s := muxPair(b)
	go func() {
		st, err := s.AcceptStream()
		if err != nil {
			return
		}
		io.Copy(st, st)
	}()
	st, err := c.OpenStream()
	if err != nil {
		b.Fatal(err)
	}
	p := make([]byte, 100)
	start := time.Now()
	for i := 0; i < b.N; i++ {
		st.Write(p)
		io.ReadFull(st, p)
	}
	b.ReportMetric(float64(time.Since(start).Microseconds())/float64(b.N), "µs/rt")
}
