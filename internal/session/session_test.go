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
