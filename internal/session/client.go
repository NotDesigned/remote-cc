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
	// Backoff is the wait before each reconnect attempt; default none for
	// the first, then 1s, 2s, 4s, 8s, then 10s.
	Backoff func(attempt int) time.Duration
	// Reconnecting is called before a new connection is tried, with the
	// reason: ErrQuiet when nothing was heard for QuietAfter (the old
	// connection stays and may yet carry on: the shared ssh connection must
	// not be ended, only kept from carrying new ones), or why the connection
	// was lost (resume.ErrSilent: nothing was heard for too long, so the
	// shared ssh connection is most likely dead too).
	Reconnecting func(reason error)
	// QuietAfter: once nothing was heard for this long, a new connection is
	// tried alongside the quiet one, and takes over once the daemon answers
	// on it (default 15s; negative: never). The quiet one is given up only
	// much later (resume.DefaultDeadAfter): on a lossy link TCP stalls for
	// that long and then carries on.
	QuietAfter time.Duration
	// NetworkEvery is how often this computer's network addresses are
	// checked for a change (default 2s; negative: never).
	NetworkEvery time.Duration
}

// ErrQuiet: nothing was heard for a while (ClientConfig.QuietAfter).
var ErrQuiet = errors.New("nothing heard for a while")

// DefaultQuietAfter is ClientConfig.QuietAfter's default: three pings
// missed. A lossy link stalls TCP about this long now and then; whichever of
// the old connection and a new one gets through first carries on.
const DefaultQuietAfter = 15 * time.Second

// quietShowAfter: a quiet connection shows on the status line once it has
// been quiet this long more (and neither it nor a new one is through).
var quietShowAfter = 5 * time.Second

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
		cfg.Backoff = func(n int) time.Duration {
			if n == 0 {
				return 0
			}
			return min(time.Second<<min(n-1, 4), 10*time.Second)
		}
	}
	if cfg.NetworkEvery == 0 {
		cfg.NetworkEvery = 2 * time.Second
	}
	if cfg.QuietAfter == 0 {
		cfg.QuietAfter = DefaultQuietAfter
	}
	tr, err := cfg.Connect(true)
	if err != nil {
		return 1, err
	}
	link := resume.New(resume.Options{})
	defer link.Close()
	done, err := attachHello(link, tr, cfg.Token)
	if err != nil {
		return 1, &StartError{err}
	}
	// A new network (Wi-Fi switch, VPN) probably took the connection
	// with it: check it now rather than after the silence timeout.
	netChanged := make(chan struct{}, 1)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go watchNetwork(stopWatch, cfg.NetworkEvery, func() {
		cfg.Logf("network changed")
		link.Probe()
		select {
		case netChanged <- struct{}{}:
		default:
		}
	})
	status := &statusLine{out: cfg.Stderr, width: func() int {
		if cfg.Terminal != nil {
			if s, ok := cfg.Terminal.Size(); ok {
				return int(s.Cols)
			}
		}
		return 0
	}}
	stdout := status.writer(cfg.Stdout)
	mux, err := yamux.Client(&wholeFrames{ReadWriteCloser: link}, muxConfig())
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

	// Ctrl-C while disconnected asks reconnect to give up. The check and the
	// request happen under mu, and the main loop clears both under mu once
	// reconnected: a Ctrl-C racing the reconnect can neither end the next
	// disconnect at once nor stop the keyboard from reaching Claude Code.
	var (
		mu           sync.Mutex
		disconnected bool
	)
	abort := make(chan struct{}, 1)
	go func() { // keyboard → Claude Code
		buf := make([]byte, 32<<10)
		for {
			n, err := cfg.Stdin.Read(buf)
			if n > 0 {
				mu.Lock()
				giveUp := disconnected && bytes.IndexByte(buf[:n], 3) >= 0
				if giveUp {
					select {
					case abort <- struct{}{}:
					default:
					}
				}
				mu.Unlock()
				if !giveUp && pc.Write(tData, buf[:n]) != nil {
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
				stdout.Write(f.Payload)
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

	// A new connection tried alongside a quiet one (see QuietAfter).
	var (
		early     chan attachResult
		nextEarly time.Time
		quietC    <-chan time.Time
		quiet     bool // the status line is on for it
	)
	// quietOver ends the status of a quiet connection that is through
	// again, or replaced.
	quietOver := func() {
		if quiet {
			quiet = false
			if status.end() {
				pc.Write(tRedraw, nil)
			}
		}
	}
	if cfg.QuietAfter > 0 {
		t := time.NewTicker(min(cfg.QuietAfter/4, time.Second))
		defer t.Stop()
		quietC = t.C
	}
	hangup := cfg.Hangup
	for {
		select {
		case <-quietC:
			q := link.Silence()
			if q < cfg.QuietAfter {
				quietOver()
			}
			if early != nil || time.Now().Before(nextEarly) {
				break
			}
			if q >= cfg.QuietAfter {
				if !quiet {
					quiet = true
					status.begin(quietShowAfter)
					status.set("nothing heard from the server for a while; trying a new connection … (typing is kept)")
				}
				cfg.Logf("nothing heard for %v; trying a new connection alongside", q.Round(100*time.Millisecond))
				if cfg.Reconnecting != nil {
					cfg.Reconnecting(ErrQuiet)
				}
				early = make(chan attachResult, 1)
				go func(ch chan<- attachResult) { ch <- dialAttach(cfg, link, true) }(early)
			}
		case r := <-early:
			early = nil
			if r.err != nil {
				cfg.Logf("new connection alongside: %v", r.err)
				nextEarly = time.Now().Add(5 * time.Second)
				break
			}
			cfg.Logf("switched to the new connection")
			done = r.done
			quietOver()
		case r := <-results:
			// Tell the daemon (it waits for this), and let it arrive.
			ts.Close()
			waitFlushed(link, done, 3*time.Second)
			return r.code, r.err
		case <-hangup:
			hangup = nil
			pc.Write(tHangup, nil)
		case <-done:
			mu.Lock()
			disconnected = true
			mu.Unlock()
			cfg.Logf("connection lost: %v", link.LastError())
			next, err := reconnect(cfg, link, status, abort, hangup, netChanged, early)
			early, quiet = nil, false
			if err != nil {
				return 1, err
			}
			done = next
			if status.end() {
				// The status took a line of the screen: have Claude
				// Code draw its interface again.
				pc.Write(tRedraw, nil)
			}
			mu.Lock()
			disconnected = false
			select { // a Ctrl-C that lost the race to the reconnect
			case <-abort:
			default:
			}
			mu.Unlock()
		}
	}
}

type attachResult struct {
	done <-chan struct{}
	err  error
}

// dialAttach connects anew and resumes link on the new connection. With
// confirmed, the current connection is kept until the daemon has answered
// the hello on the new one (it may yet carry on); otherwise the resume
// handshake follows the hello at once, a round trip sooner.
func dialAttach(cfg ClientConfig, link *resume.Conn, confirmed bool) attachResult {
	tr, err := cfg.Connect(false)
	if err != nil {
		return attachResult{err: err}
	}
	// A frozen server may accept the connection and say nothing: the
	// answer, and the resume handshake, are given 20s.
	if confirmed {
		h, err := sayHello(tr, cfg.Token, 20*time.Second)
		if err != nil {
			return attachResult{err: err}
		}
		done, err := link.Attach(h)
		return attachResult{done, err}
	}
	done, err := attachHello(link, tr, cfg.Token)
	return attachResult{done, err}
}

// reconnect attaches new connections until one resumes the stream. The
// first attempt goes at once (or is the one already under way, pending),
// and a network change skips the wait before the next. The status line
// tells how it goes once the outage lasts.
func reconnect(cfg ClientConfig, link *resume.Conn, status *statusLine, abort, hangup, netChanged <-chan struct{}, pending <-chan attachResult) (<-chan struct{}, error) {
	start := time.Now()
	giveUp := start.Add(cfg.Grace + time.Minute)
	const hint = " … (typing is kept; Ctrl-C ends the session)"
	status.begin(statusShowAfter)
	status.set("connection lost; reconnecting" + hint)
	gaveUp := func() (<-chan struct{}, error) {
		status.final("gave up reconnecting")
		return nil, fmt.Errorf("gave up reconnecting; the session on the server ends by itself within %v", cfg.Grace)
	}
	if cfg.Reconnecting != nil && pending == nil {
		cfg.Reconnecting(link.LastError())
	}
	for attempt := 0; ; attempt++ {
		if wait := cfg.Backoff(attempt); wait > 0 && pending == nil {
			select {
			case <-abort:
				return gaveUp()
			case <-hangup:
				return nil, errors.New("connection lost while ending the session")
			case <-netChanged:
			case <-time.After(wait):
			}
		}
		if attempt > 0 {
			status.set(fmt.Sprintf("connection lost %v ago; reconnecting (attempt %d)%s",
				time.Since(start).Round(time.Second), attempt+1, hint))
		}
		res := pending
		pending = nil
		if res == nil {
			ch := make(chan attachResult, 1)
			go func() { ch <- dialAttach(cfg, link, false) }()
			res = ch
		}
		var r attachResult
		select {
		case r = <-res:
		case <-abort: // the attempt goes on; Run's end closes link
			return gaveUp()
		case <-hangup:
			return nil, errors.New("connection lost while ending the session")
		}
		if r.err == nil {
			cfg.Logf("reconnected after %d attempt(s), %v", attempt+1, time.Since(start).Round(time.Millisecond))
			return r.done, nil
		}
		cfg.Logf("reconnect attempt %d: %v", attempt+1, r.err)
		if errors.Is(r.err, ErrEnded) {
			status.final("the session on the server has ended")
			return nil, r.err
		}
		if time.Now().After(giveUp) {
			status.final("could not reconnect")
			return nil, fmt.Errorf("could not reconnect: %w", r.err)
		}
	}
}

// waitFlushed waits until the daemon has everything written so far, or has
// closed the connection (it ends the session once it has the close).
func waitFlushed(link *resume.Conn, done <-chan struct{}, limit time.Duration) {
	deadline := time.After(limit)
	for !link.Flushed() {
		link.RequestAck() // the daemon would otherwise ack only after its ack delay
		select {
		case <-done:
			return
		case <-deadline:
			return
		case <-time.After(25 * time.Millisecond):
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
