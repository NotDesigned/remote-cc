//go:build linux

package session

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// harness runs a daemon (no mount) and a client in this process, connected
// through the daemon's socket; cut() drops the current connection.
type harness struct {
	t      *testing.T
	run    string
	mu     sync.Mutex
	conns  []net.Conn
	out    syncBuffer
	stdinW *io.PipeWriter
	dials  int
}

func newHarness(t *testing.T, launch string, grace time.Duration) (*harness, *daemon) {
	h := &harness{t: t, run: t.TempDir()}
	d, err := newDaemon(DaemonConfig{Run: h.run, Grace: grace, Launch: []string{"sh", "-c", launch},
		Log: log.New(testWriter{t}, "daemon: ", log.Lmicroseconds)})
	if err != nil {
		t.Fatal(err)
	}
	go d.serve()
	t.Cleanup(func() { d.end("test over") })
	return h, d
}

func (h *harness) connect(first bool) (io.ReadWriteCloser, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dials++
	c, err := net.Dial("unix", filepath.Join(h.run, "session.sock"))
	if err != nil {
		// What __rcc-dial reports when the daemon is gone.
		r, w := net.Pipe()
		go func() {
			readLine(w, 256) // the hello
			io.WriteString(w, "error "+endedMessage+"\n")
			w.Close()
		}()
		return r, nil
	}
	h.conns = append(h.conns, c)
	return c, nil
}

func (h *harness) cut() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		c.Close()
	}
	h.conns = nil
}

// echo serves commands: it answers each line with "re:" + the line.
func echo(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			s := bufio.NewScanner(c)
			for s.Scan() {
				time.Sleep(300 * time.Millisecond) // long enough to be cut mid-command
				io.WriteString(c, "re:"+s.Text()+"\n")
			}
		}()
	}
}

func (h *harness) start(grace time.Duration) chan [2]any {
	stdinR, stdinW := io.Pipe()
	h.stdinW = stdinW
	res := make(chan [2]any, 1)
	go func() {
		code, err := Run(ClientConfig{
			Token:    "tok",
			Connect:  h.connect,
			Files:    func(rw io.ReadWriteCloser) error { io.Copy(io.Discard, rw); return nil },
			Commands: echo,
			Stdin:    stdinR,
			Stdout:   &h.out,
			Stderr:   io.Discard,
			Grace:    grace,
			Backoff:  func(int) time.Duration { return 50 * time.Millisecond },
		})
		res <- [2]any{code, err}
	}()
	return res
}

func (h *harness) waitOutput(want string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(h.out.String(), want) {
		if time.Now().After(deadline) {
			h.t.Fatalf("waiting for %q; output so far: %q", want, h.out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The terminal and a command in flight survive dropped connections; the
// exit code comes back.
func TestSessionSurvivesDrops(t *testing.T) {
	h, _ := newHarness(t, `printf 'ready\n'; read a; printf 'got:%s\n' "$a"; read b; printf 'got:%s\n' "$b"; exit 7`, time.Minute)
	res := h.start(time.Minute)
	h.waitOutput("ready")

	// A command (as rcc-shim sends it) that is cut while it runs.
	cmd, err := net.Dial("unix", filepath.Join(h.run, "s"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(cmd, "hello\n")
	time.Sleep(100 * time.Millisecond)
	h.cut()
	reply := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(cmd).ReadString('\n')
		reply <- l
	}()
	select {
	case l := <-reply:
		if l != "re:hello\n" {
			t.Fatalf("command reply %q", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("command reply lost across the reconnect")
	}

	// Typing while disconnected arrives after the reconnect.
	h.cut()
	h.stdinW.Write([]byte("one\n"))
	h.waitOutput("got:one")
	h.cut()
	h.stdinW.Write([]byte("two\n"))
	h.waitOutput("got:two")

	select {
	case r := <-res:
		if r[1] != nil || r[0].(int) != 7 {
			t.Fatalf("exit %v %v", r[0], r[1])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit")
	}
	if n := strings.Count(h.out.String(), "got:one"); n != 1 {
		t.Fatalf("output repeated or lost (%d): %q", n, h.out.String())
	}
	if h.dials < 4 {
		t.Fatalf("expected reconnects, dialed %d times", h.dials)
	}
	waitGone(t, h.run)
}

// A first connection that says hello but drops before the stream resumes
// must not leave the session without its multiplexer: the next one works.
func TestFirstAttachFails(t *testing.T) {
	h, d := newHarness(t, `printf 'ready\n'; sleep 60`, time.Minute)
	c, err := net.Dial("unix", filepath.Join(h.run, "session.sock"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, helloPrefix+"tok\n")
	if l, _ := readLine(c, 256); l != "ok" {
		t.Fatalf("hello answer %q", l)
	}
	c.Close() // before the resume handshake
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		linked := d.link != nil
		d.mu.Unlock()
		if linked || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the failed Attach returns
	h.start(time.Minute)
	h.waitOutput("ready")
}

// A Ctrl-C typed just as a reconnect succeeds must not cut the keyboard off
// Claude Code, nor make the next disconnect give up at once.
func TestCtrlCRacingReconnect(t *testing.T) {
	h, _ := newHarness(t, `printf 'ready\n'; while read a; do printf 'got:%s\n' "$a"; done`, time.Minute)
	stdinR, stdinW := io.Pipe()
	var once sync.Once
	reconnected := make(chan struct{}, 4)
	go Run(ClientConfig{
		Token:    "tok",
		Connect:  h.connect,
		Files:    func(rw io.ReadWriteCloser) error { io.Copy(io.Discard, rw); return nil },
		Commands: echo,
		Stdin:    stdinR,
		Stdout:   &h.out,
		Stderr:   io.Discard,
		Grace:    time.Minute,
		Backoff:  func(int) time.Duration { return 50 * time.Millisecond },
		Logf: func(f string, a ...any) {
			if strings.HasPrefix(f, "reconnected") {
				// The stream is back, the client does not know yet.
				once.Do(func() {
					stdinW.Write([]byte{3})
					time.Sleep(200 * time.Millisecond)
				})
				reconnected <- struct{}{}
			}
		},
	})
	h.waitOutput("ready")
	h.cut()
	<-reconnected
	go stdinW.Write([]byte("one\n")) // blocks forever if the keyboard was cut off
	h.waitOutput("got:one")
	h.cut()
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("gave up on the next disconnect")
	}
	stdinW.Write([]byte("two\n"))
	h.waitOutput("got:two")
}

// Without a connection for the grace period, the daemon ends the session
// and removes its directory; the client then hears that it ended.
func TestGracePeriod(t *testing.T) {
	h, d := newHarness(t, `printf 'ready\n'; sleep 60`, 500*time.Millisecond)
	// The network stays down until the daemon has given up; then the
	// client reaches the server and hears that the session ended.
	var refused, networkUp atomic.Bool
	res := make(chan [2]any, 1)
	stdinR, _ := io.Pipe()
	go func() {
		code, err := Run(ClientConfig{
			Token: "tok",
			Connect: func(first bool) (io.ReadWriteCloser, error) {
				if !first && !networkUp.Load() {
					refused.Store(true)
					return nil, errors.New("network down")
				}
				return h.connect(first)
			},
			Files:    func(rw io.ReadWriteCloser) error { io.Copy(io.Discard, rw); return nil },
			Commands: echo,
			Stdin:    stdinR,
			Stdout:   &h.out,
			Stderr:   io.Discard,
			Grace:    500 * time.Millisecond,
			Backoff:  func(int) time.Duration { return 100 * time.Millisecond },
		})
		res <- [2]any{code, err}
	}()
	h.waitOutput("ready")
	h.cut()
	select {
	case <-d.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon kept the session past the grace period")
	}
	waitGone(t, h.run)
	networkUp.Store(true)
	select {
	case r := <-res:
		if !errors.Is(r[1].(error), ErrEnded) || !refused.Load() {
			t.Fatalf("client: %v %v", r[0], r[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client kept trying")
	}
}

// A reconnect to an ended session reports ErrEnded right away.
func TestReconnectToEndedSession(t *testing.T) {
	h, d := newHarness(t, `printf 'ready\n'; sleep 60`, time.Minute)
	res := h.start(time.Minute)
	h.waitOutput("ready")
	d.end("ended on the server")
	select {
	case r := <-res:
		if r[1] == nil {
			t.Fatal("no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client did not notice the ended session")
	}
}

func waitGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not removed", dir)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// The resume handshake goes right behind the hello; a refused hello still
// fails the start, with the daemon's reason.
func TestRefusedHello(t *testing.T) {
	h, _ := newHarness(t, `printf 'ready\n'; sleep 60`, time.Minute)
	h.start(time.Minute)
	h.waitOutput("ready")
	_, err := Run(ClientConfig{
		Token:    "other",
		Connect:  h.connect,
		Files:    func(rw io.ReadWriteCloser) error { return nil },
		Commands: echo,
		Stdin:    strings.NewReader(""),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	var se *StartError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "wrong session token") {
		t.Fatalf("got %v", err)
	}
}

type fakeTerm struct{ size Size }

func (f fakeTerm) MakeRaw() (func(), error) { return func() {}, nil }
func (f fakeTerm) Size() (Size, bool)       { return f.size, true }
func (f fakeTerm) Resized() <-chan struct{} { return nil }
func (h *harness) outputSince(n int) string { return h.out.String()[n:] }
func (h *harness) outputLen() int           { return len(h.out.String()) }
func (h *harness) connectUnless(down *atomic.Bool) func(bool) (io.ReadWriteCloser, error) {
	return func(first bool) (io.ReadWriteCloser, error) {
		if down.Load() {
			return nil, errors.New("network down")
		}
		return h.connect(first)
	}
}

// winchScript prints the terminal size whenever it changes.
const winchScript = `trap 'echo "size=$(stty size)"' WINCH; printf 'ready\n'; while :; do if read a; then printf 'got:%s\n' "$a"; fi; done`

// A short drop: the first attempt goes at once, Reconnecting is called once,
// what was typed arrives, and the screen is left alone: no status, no
// redraw (Claude Code's interface would leave a copy of itself behind).
func TestShortDropLeavesScreen(t *testing.T) {
	h, _ := newHarness(t, winchScript, time.Minute)
	stdinR, stdinW := io.Pipe()
	var stderr syncBuffer
	var drops atomic.Int32
	go Run(ClientConfig{
		Token:        "tok",
		Connect:      h.connect,
		Files:        func(rw io.ReadWriteCloser) error { io.Copy(io.Discard, rw); return nil },
		Commands:     echo,
		Stdin:        stdinR,
		Stdout:       &h.out,
		Stderr:       &stderr,
		Terminal:     fakeTerm{Size{Rows: 24, Cols: 80}},
		Grace:        time.Minute,
		Reconnecting: func(error) { drops.Add(1) },
	})
	h.waitOutput("ready")
	mark := h.outputLen()
	h.cut()
	go stdinW.Write([]byte("typed while away\n"))
	start := time.Now()
	h.waitOutput("got:typed while away")
	if d := time.Since(start); d > time.Second {
		t.Errorf("reconnected after %v; the first attempt should go at once", d)
	}
	time.Sleep(500 * time.Millisecond)
	if drops.Load() != 1 {
		t.Errorf("Reconnecting called %d times", drops.Load())
	}
	if s := stderr.String(); s != "" {
		t.Errorf("status printed for a short drop: %q", s)
	}
	if out := h.outputSince(mark); strings.Contains(out, "size=") {
		t.Errorf("the terminal was resized: %q", out)
	}
}

// A longer outage: one status line, never a newline, shorter than the
// width; erased once the connection is back, and then a redraw that changes
// the height for a moment, never the width.
func TestOutageStatusAndRedraw(t *testing.T) {
	h, _ := newHarness(t, winchScript, time.Minute)
	stdinR, _ := io.Pipe()
	var stderr syncBuffer
	var down atomic.Bool
	go Run(ClientConfig{
		Token:    "tok",
		Connect:  h.connectUnless(&down),
		Files:    func(rw io.ReadWriteCloser) error { io.Copy(io.Discard, rw); return nil },
		Commands: echo,
		Stdin:    stdinR,
		Stdout:   &h.out,
		Stderr:   &stderr,
		Terminal: fakeTerm{Size{Rows: 24, Cols: 40}},
		Grace:    time.Minute,
		Backoff:  func(int) time.Duration { return 100 * time.Millisecond },
	})
	h.waitOutput("ready")
	mark := h.outputLen()
	down.Store(true)
	h.cut()
	time.Sleep(1500 * time.Millisecond)
	s := stderr.String()
	if !strings.HasPrefix(s, "\rrcc: connection lost") || strings.Contains(s, "\n") {
		t.Fatalf("status %q", s)
	}
	for _, line := range strings.Split(s, "\r") {
		if l := len([]rune(strings.TrimSuffix(line, "\x1b[K"))); l >= 40 {
			t.Errorf("status line %q is %d wide, the terminal 40", line, l)
		}
	}
	down.Store(false)
	h.waitOutput("size=24 40")
	if out := h.outputSince(mark); !strings.Contains(out, "size=23 40") || strings.Count(out, "size=") != 2 {
		t.Errorf("redraw: %q", out)
	}
	if s := stderr.String(); !strings.HasSuffix(s, "\r\x1b[K") || strings.Contains(s, "\n") {
		t.Errorf("status not erased: %q", s)
	}
}

func TestStatusLine(t *testing.T) {
	var out bytes.Buffer
	s := &statusLine{out: &out, width: func() int { return 10 }}
	s.begin(time.Hour)
	s.set("lost")
	if s.end() || out.Len() != 0 {
		t.Fatalf("a short outage printed %q", out.String())
	}
	s.begin(0)
	s.set("connection lost")
	w := s.writer(&out)
	w.Write([]byte("X"))
	s.set("again")
	if !s.end() {
		t.Fatal("end: the status was shown")
	}
	if want := "\rrcc: conn\x1b[K\r\x1b[KX\rrcc: agai\x1b[K\r\x1b[K"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

// stalled is a connection that carries nothing while stalled (as TCP on a
// lossy link while it retransmits), then carries on. Closing it ends the
// stall, as closing ssh's pipes does.
type stalled struct {
	net.Conn
	mu     sync.Mutex
	cond   *sync.Cond
	on     bool
	closed bool
}

func (s *stalled) wait() {
	s.mu.Lock()
	for s.on && !s.closed {
		s.cond.Wait()
	}
	s.mu.Unlock()
}

func (s *stalled) Close() error {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	return s.Conn.Close()
}

func (s *stalled) set(on bool) {
	s.mu.Lock()
	s.on = on
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *stalled) Read(p []byte) (int, error) {
	n, err := s.Conn.Read(p)
	s.wait()
	return n, err
}

func (s *stalled) Write(p []byte) (int, error) {
	s.wait()
	return s.Conn.Write(p)
}

// quietRun runs a client whose first connection can be stalled; later
// connections fail while down is set.
func quietRun(t *testing.T, down *atomic.Bool, reasons chan<- error) (*harness, *stalled, *io.PipeWriter, *syncBuffer) {
	h, _ := newHarness(t, winchScript, time.Minute)
	var first *stalled
	connect := func(isFirst bool) (io.ReadWriteCloser, error) {
		if !isFirst && down.Load() {
			return nil, errors.New("network down")
		}
		c, err := h.connect(isFirst)
		if err != nil || !isFirst {
			return c, err
		}
		first = &stalled{Conn: c.(net.Conn)}
		first.cond = sync.NewCond(&first.mu)
		return first, nil
	}
	stdinR, stdinW := io.Pipe()
	stderr := &syncBuffer{}
	go Run(ClientConfig{
		Token:        "tok",
		Connect:      connect,
		Files:        func(rw io.ReadWriteCloser) error { io.Copy(io.Discard, rw); return nil },
		Commands:     echo,
		Stdin:        stdinR,
		Stdout:       &h.out,
		Stderr:       stderr,
		Terminal:     fakeTerm{Size{Rows: 24, Cols: 80}},
		Grace:        time.Minute,
		QuietAfter:   400 * time.Millisecond,
		Reconnecting: func(err error) { reasons <- err },
	})
	h.waitOutput("ready")
	return h, first, stdinW, stderr
}

// A quiet connection is kept while a new one is tried alongside: when the
// new one cannot get through and the old one carries on, nothing shows.
func TestQuietLinkCarriesOn(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	reasons := make(chan error, 10)
	h, first, stdinW, stderr := quietRun(t, &down, reasons)
	mark := h.outputLen()
	first.set(true)
	go stdinW.Write([]byte("typed in the stall\n"))
	time.Sleep(1500 * time.Millisecond)
	first.set(false)
	h.waitOutput("got:typed in the stall")
	time.Sleep(300 * time.Millisecond)
	if len(reasons) == 0 {
		t.Fatal("no new connection was tried")
	}
	for len(reasons) > 0 {
		if err := <-reasons; !errors.Is(err, ErrQuiet) {
			t.Errorf("Reconnecting(%v): the quiet connection was given up", err)
		}
	}
	if s := stderr.String(); s != "" {
		t.Errorf("status printed: %q", s)
	}
	if out := h.outputSince(mark); strings.Contains(out, "size=") {
		t.Errorf("the terminal was resized: %q", out)
	}
	if h.dials != 1 {
		t.Errorf("%d connections reached the daemon", h.dials)
	}
}

// A quiet connection that stays quiet: the one tried alongside takes over,
// and nothing shows either.
func TestQuietLinkReplaced(t *testing.T) {
	var down atomic.Bool
	reasons := make(chan error, 10)
	h, first, stdinW, stderr := quietRun(t, &down, reasons)
	mark := h.outputLen()
	first.set(true) // for good
	start := time.Now()
	go stdinW.Write([]byte("typed in the stall\n"))
	h.waitOutput("got:typed in the stall")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took over after %v", d)
	}
	if err := <-reasons; !errors.Is(err, ErrQuiet) {
		t.Errorf("Reconnecting(%v)", err)
	}
	time.Sleep(300 * time.Millisecond)
	if s := stderr.String(); s != "" {
		t.Errorf("status printed: %q", s)
	}
	if out := h.outputSince(mark); strings.Contains(out, "size=") {
		t.Errorf("the terminal was resized: %q", out)
	}
}

// A quiet connection that lasts shows on the status line; once it carries
// on, the status is erased and the interface redrawn (the height only).
func TestLongQuietShowsStatus(t *testing.T) {
	defer func(d time.Duration) { quietShowAfter = d }(quietShowAfter)
	quietShowAfter = 300 * time.Millisecond
	var down atomic.Bool
	down.Store(true)
	h, first, _, stderr := quietRun(t, &down, make(chan error, 10))
	mark := h.outputLen()
	first.set(true)
	time.Sleep(1500 * time.Millisecond)
	if s := stderr.String(); !strings.HasPrefix(s, "\rrcc: nothing heard") || strings.Contains(s, "\n") {
		t.Fatalf("status %q", s)
	}
	first.set(false)
	h.waitOutput("size=24 80")
	if out := h.outputSince(mark); !strings.Contains(out, "size=23 80") {
		t.Errorf("redraw: %q", out)
	}
	if s := stderr.String(); !strings.HasSuffix(s, "\r\x1b[K") || strings.Contains(s, "\n") {
		t.Errorf("status not erased: %q", s)
	}
}
