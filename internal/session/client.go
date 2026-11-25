package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"remote-cc/internal/proto"
	"remote-cc/internal/resume"
)

// ClientConfig is what Run needs from rcc.
type ClientConfig struct {
	Token string
	// Connect returns a new connection to the daemon: the first one starts
	// the session on the server, later ones reach the running daemon.
	Connect func(first bool) (io.ReadWriteCloser, error)
	// Files serves the project directory to sshfs over rw.
	Files func(rw io.ReadWriteCloser) error
	// Commands serves the commands rcc-shim sends (one connection each).
	Commands func(l net.Listener) error

	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Terminal       Terminal // nil: not a terminal (no raw mode, fixed size)
	Env            map[string]string

	// Hangup ends Claude Code (SIGHUP to it on the server).
	Hangup <-chan struct{}
	Grace  time.Duration
	Logf   func(string, ...any)
	// Backoff is the wait before each reconnect attempt; default 1s, 2s, 4s,
	// 8s, then 10s.
	Backoff func(attempt int) time.Duration
}

// StartError: the first connection did not reach a session daemon (the
// server-side start failed; its stderr says why).
type StartError struct{ Err error }

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Terminal is this side's terminal.
type Terminal interface {
	MakeRaw() (restore func(), err error)
	Size() (Size, bool)
	// Resized delivers a value when the window size changes.
	Resized() <-chan struct{}
}

// Run runs the session until Claude Code exits and returns its exit code.
func Run(cfg ClientConfig) (int, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Grace == 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(n int) time.Duration { return min(time.Second<<min(n, 4), 10*time.Second) }
	}
	tr, err := cfg.Connect(true)
	if err != nil {
		return 1, err
	}
	if err := sayHello(tr, cfg.Token); err != nil {
		tr.Close()
		return 1, &StartError{err}
	}
	link := resume.New(resume.Options{})
	defer link.Close()
	done, err := link.Attach(tr)
	if err != nil {
		return 1, err
	}
	mux, err := yamux.Client(link, muxConfig())
	if err != nil {
		return 1, err
	}
	defer mux.Close()

	open := func(kind byte) (*yamux.Stream, error) {
		st, err := mux.OpenStream()
		if err == nil {
			_, err = st.Write([]byte{kind})
		}
		return st, err
	}
	fs, err := open(kindFiles)
	if err != nil {
		return 1, err
	}
	go cfg.Files(fs)
	ts, err := open(kindTerminal)
	if err != nil {
		return 1, err
	}
	pc := proto.NewConn(ts)
	size := Size{Rows: 24, Cols: 80}
	if cfg.Terminal != nil {
		if s, ok := cfg.Terminal.Size(); ok {
			size = s
		}
	}
	if err := pc.WriteJSON(tOpen, Open{Size: size, Env: cfg.Env}); err != nil {
		return 1, err
	}
	go cfg.Commands(mux)

	if cfg.Terminal != nil {
		if restore, err := cfg.Terminal.MakeRaw(); err == nil {
			defer restore()
		}
		go func() {
			for range cfg.Terminal.Resized() {
				if s, ok := cfg.Terminal.Size(); ok {
					pc.WriteJSON(tResize, s)
				}
			}
		}()
	}

	var disconnected atomic.Bool
	abort := make(chan struct{})
	var abortOnce sync.Once
	go func() { // keyboard → Claude Code; Ctrl-C while disconnected gives up
		buf := make([]byte, 32<<10)
		for {
			n, err := cfg.Stdin.Read(buf)
			if n > 0 {
				if disconnected.Load() && bytes.IndexByte(buf[:n], 3) >= 0 {
					abortOnce.Do(func() { close(abort) })
					return
				}
				if pc.Write(tData, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	type result struct {
		code int
		err  error
	}
	results := make(chan result, 1)
	go func() { // Claude Code's terminal → this terminal
		for {
			f, err := pc.Read()
			if err != nil {
				results <- result{1, fmt.Errorf("the session ended unexpectedly: %w", err)}
				return
			}
			switch f.Type {
			case tData:
				cfg.Stdout.Write(f.Payload)
			case tExit:
				var e Exit
				json.Unmarshal(f.Payload, &e)
				results <- result{e.Code, nil}
				return
			case tError:
				results <- result{1, errors.New(string(f.Payload))}
				return
			}
		}
	}()

	hangup := cfg.Hangup
	for {
		select {
		case r := <-results:
			// Tell the daemon (it waits for this), and let it arrive.
			ts.Close()
			waitFlushed(link, done, 3*time.Second)
			return r.code, r.err
		case <-hangup:
			hangup = nil
			pc.Write(tHangup, nil)
		case <-done:
			disconnected.Store(true)
			fmt.Fprint(cfg.Stderr, "\r\nrcc: connection lost; reconnecting (Ctrl-C ends the session) …\r\n")
			next, err := reconnect(cfg, link, abort, hangup)
			if err != nil {
				return 1, err
			}
			done = next
			disconnected.Store(false)
		}
	}
}

// reconnect attaches new connections until one resumes the stream.
func reconnect(cfg ClientConfig, link *resume.Conn, abort, hangup <-chan struct{}) (<-chan struct{}, error) {
	giveUp := time.Now().Add(cfg.Grace + time.Minute)
	for attempt := 0; ; attempt++ {
		select {
		case <-abort:
			return nil, fmt.Errorf("gave up reconnecting; the session on the server ends by itself within %v", cfg.Grace)
		case <-hangup:
			return nil, errors.New("connection lost while ending the session")
		case <-time.After(cfg.Backoff(attempt)):
		}
		tr, err := cfg.Connect(false)
		if err == nil {
			// A frozen server may accept the connection and say nothing.
			t := time.AfterFunc(20*time.Second, func() { tr.Close() })
			err = sayHello(tr, cfg.Token)
			t.Stop()
			if err != nil {
				tr.Close()
			}
		}
		if err == nil {
			var done <-chan struct{}
			if done, err = link.Attach(tr); err == nil {
				cfg.Logf("reconnected after %d attempt(s)", attempt+1)
				return done, nil
			}
		}
		cfg.Logf("reconnect attempt %d: %v", attempt+1, err)
		if errors.Is(err, ErrEnded) {
			return nil, err
		}
		if time.Now().After(giveUp) {
			return nil, fmt.Errorf("could not reconnect: %w", err)
		}
	}
}

// waitFlushed waits until the daemon has everything written so far, or has
// closed the connection (it ends the session once it has the close).
func waitFlushed(link *resume.Conn, done <-chan struct{}, limit time.Duration) {
	deadline := time.After(limit)
	for !link.Flushed() {
		select {
		case <-done:
			return
		case <-deadline:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// LocalEnv is the terminal environment to pass on (TerminalEnv from os).
func LocalEnv() map[string]string {
	env := map[string]string{}
	for _, k := range TerminalEnv {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}
