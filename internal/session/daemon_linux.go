//go:build linux

package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/sys/unix"

	"remote-cc/internal/proto"
	"remote-cc/internal/resume"
)

const daemonChildEnv = "RCC_DAEMON_CHILD"

// mountScript runs in a private mount namespace (unshare -m): it binds the
// history profile ($3) over the account's projects/ and history.jsonl ($4),
// then becomes sshfs on stdio, mounting the project directory $1 with
// options $2.
const mountScript = `set -e
mkdir -p "$1"
if [ -n "$3" ]; then
  umask 077
  mkdir -p "$3/projects" "$4/projects"
  [ -e "$3/history.jsonl" ] || : > "$3/history.jsonl"
  [ -e "$4/history.jsonl" ] || : > "$4/history.jsonl"
  mount --bind "$3/projects" "$4/projects"
  mount --bind "$3/history.jsonl" "$4/history.jsonl"
fi
exec sshfs -f -o "$2" ":$1" "$1"`

// sshfs' directory cache matters for speed: a listing brings every entry's
// attributes in one round trip, and without it each file costs its own round
// trip to the computer. Files also change there (commands and git run
// there), so it is kept short (5s), and "not found" is never cached (git
// creating a worktree and Claude checking it right after).
const sshfsOptions = "slave,dir_cache=yes,dcache_timeout=5,attr_timeout=1,entry_timeout=1,negative_timeout=0"

// DaemonConfig describes one session on the server.
type DaemonConfig struct {
	Run   string // the session's directory: sockets, token, launch.sh
	Root  string // project directory to mount ("" = no mount, for tests)
	Prof  string // history profile directory, "" = the account's own
	Cfg   string // the account's config directory (CLAUDE_CONFIG_DIR)
	Grace time.Duration
	// Launch is the command started in the pseudo-terminal once the mount
	// is up; default: sh RUN/launch.sh.
	Launch []string
	Log    *log.Logger
}

// Daemon is `rcc-shim __rcc-daemon RUN`. It returns once the daemon listens
// (the daemon itself runs on, detached), with RUN's parameters from the
// environment: R, PROF, CFG.
func Daemon(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rcc-shim __rcc-daemon RUN")
		return 2
	}
	run := args[0]
	if os.Getenv(daemonChildEnv) == "" {
		return spawnDaemon(run)
	}
	cfg := DaemonConfig{Run: run, Root: os.Getenv("R"), Prof: os.Getenv("PROF"), Cfg: os.Getenv("CFG"), Grace: DefaultGrace,
		Log: log.New(os.Stderr, "", log.LstdFlags)}
	ready := os.NewFile(3, "ready")
	d, err := newDaemon(cfg)
	if err != nil {
		fmt.Fprintf(ready, "%v\n", err)
		return 1
	}
	fmt.Fprintln(ready, "ok")
	ready.Close()
	d.serve()
	return 0
}

// spawnDaemon starts the daemon in its own session (so ssh's hangup does
// not reach it) and waits until it listens.
func spawnDaemon(run string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc-shim:", err)
		return 1
	}
	logf, err := os.OpenFile(filepath.Join(run, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc-shim:", err)
		return 1
	}
	defer logf.Close()
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc-shim:", err)
		return 1
	}
	cmd := exec.Command(exe, "__rcc-daemon", run)
	cmd.Env = append(os.Environ(), daemonChildEnv+"=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "rcc-shim:", err)
		return 1
	}
	w.Close()
	cmd.Process.Release()
	msg, _ := io.ReadAll(io.LimitReader(r, 4096))
	if s := strings.TrimSpace(string(msg)); s != "ok" {
		if s == "" {
			s = "the session daemon did not start (see " + filepath.Join(run, "daemon.log") + ")"
		}
		fmt.Fprintln(os.Stderr, "rcc:", s)
		return 1
	}
	return 0
}

type daemon struct {
	cfg  DaemonConfig
	logf func(string, ...any)

	sessL, cmdL net.Listener

	mu         sync.Mutex
	token      string
	link       *resume.Conn
	mux        *yamux.Session
	muxReady   chan struct{}
	attaches   int
	detachedAt time.Time // zero while connected
	term       *os.File  // pseudo-terminal master, once Claude Code runs

	mounted   chan struct{} // closed when the project directory is mounted
	mountErr  chan string   // why the mount failed
	fsPid     int
	fsStopped chan struct{}

	claude     *exec.Cmd
	claudeDone chan struct{}

	ending   sync.Once
	finished chan struct{}
}

func newDaemon(cfg DaemonConfig) (*daemon, error) {
	if cfg.Grace == 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	if len(cfg.Launch) == 0 {
		cfg.Launch = []string{"sh", filepath.Join(cfg.Run, "launch.sh")}
	}
	d := &daemon{cfg: cfg, logf: cfg.Log.Printf, muxReady: make(chan struct{}), mounted: make(chan struct{}),
		mountErr: make(chan string, 1), fsStopped: make(chan struct{}), claudeDone: make(chan struct{}), finished: make(chan struct{})}
	listen := func(name string) (net.Listener, error) {
		p := filepath.Join(cfg.Run, name)
		os.Remove(p)
		l, err := net.Listen("unix", p)
		if err == nil {
			os.Chmod(p, 0o600)
		}
		return l, err
	}
	var err error
	if d.sessL, err = listen("session.sock"); err != nil {
		return nil, err
	}
	if d.cmdL, err = listen("s"); err != nil {
		d.sessL.Close()
		return nil, err
	}
	os.WriteFile(filepath.Join(cfg.Run, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	if cfg.Root == "" {
		close(d.mounted)
	}
	return d, nil
}

func (d *daemon) serve() {
	// Handled, not ignored: an ignored SIGHUP would be inherited by Claude
	// Code, and hanging it up would then do nothing.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-term:
			d.end("terminated")
		case <-d.finished:
		}
	}()
	go d.acceptCommands()
	go d.supervise()
	go func() {
		for {
			c, err := d.sessL.Accept()
			if err != nil {
				return
			}
			go d.transport(c)
		}
	}()
	<-d.finished
}

// transport takes a new connection from rcc: hello, then resume the stream.
func (d *daemon) transport(c net.Conn) {
	c.SetDeadline(time.Now().Add(20 * time.Second))
	line, err := readLine(c, 256)
	c.SetDeadline(time.Time{})
	tok, ok := strings.CutPrefix(line, helloPrefix)
	if err != nil || !ok || tok == "" {
		c.Close()
		return
	}
	d.mu.Lock()
	if d.token == "" {
		d.token = tok
		os.WriteFile(filepath.Join(d.cfg.Run, "token"), []byte(tok+"\n"), 0o600)
	}
	if tok != d.token {
		d.mu.Unlock()
		io.WriteString(c, "error wrong session token\n")
		c.Close()
		return
	}
	if d.link == nil {
		d.link = resume.New(resume.Options{})
	}
	link := d.link
	d.mu.Unlock()
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		c.Close()
		return
	}
	done, err := link.Attach(c)
	if err != nil {
		d.logf("attach: %v", err)
		return
	}
	// The multiplexer starts with the first attach that succeeds (not the
	// first connection: that one may drop before the stream resumed).
	d.mu.Lock()
	var newMux *yamux.Session
	if d.mux == nil {
		mux, err := yamux.Server(&wholeFrames{ReadWriteCloser: link}, muxConfig())
		if err != nil {
			d.mu.Unlock()
			d.end("yamux: " + err.Error())
			return
		}
		d.mux, newMux = mux, mux
	}
	d.mu.Unlock()
	if newMux != nil {
		close(d.muxReady)
		go d.acceptStreams(newMux)
	}
	d.mu.Lock()
	d.attaches++
	n := d.attaches
	d.detachedAt = time.Time{}
	d.mu.Unlock()
	// No redraw here: the stream resumed where it was, so the screen is
	// as Claude Code left it (rcc asks for one if it printed on it).
	d.logf("connected (%d)", n)
	<-done
	d.mu.Lock()
	if !link.Attached() {
		d.detachedAt = time.Now()
		d.logf("connection lost; waiting up to %v", d.cfg.Grace)
	}
	d.mu.Unlock()
}

// supervise ends the session when rcc stays away for the grace period (or
// never connects).
func (d *daemon) supervise() {
	start := time.Now()
	for {
		select {
		case <-d.finished:
			return
		case <-time.After(time.Second):
		}
		d.mu.Lock()
		never := d.attaches == 0 && time.Since(start) > time.Minute
		away := !d.detachedAt.IsZero() && time.Since(d.detachedAt) > d.cfg.Grace
		d.mu.Unlock()
		if never {
			d.end("rcc never connected")
		}
		if away {
			d.end(fmt.Sprintf("no connection for %v", d.cfg.Grace))
		}
	}
}

func (d *daemon) acceptStreams(mux *yamux.Session) {
	for {
		st, err := mux.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			var k [1]byte
			if _, err := io.ReadFull(st, k[:]); err != nil {
				st.Close()
				return
			}
			switch k[0] {
			case kindFiles:
				d.files(st)
			case kindTerminal:
				d.terminal(st)
			default:
				st.Close()
			}
		}()
	}
}

// acceptCommands relays each rcc-shim connection to rcc as a new stream.
func (d *daemon) acceptCommands() {
	for {
		c, err := d.cmdL.Accept()
		if err != nil {
			return
		}
		go func() {
			select {
			case <-d.muxReady:
			case <-d.finished:
				c.Close()
				return
			}
			st, err := d.mux.OpenStream()
			if err != nil {
				c.Close()
				return
			}
			splice(c, st)
		}()
	}
}

// files runs sshfs on the stream, in a private mount namespace.
func (d *daemon) files(st *yamux.Stream) {
	if d.cfg.Root == "" {
		st.Close()
		return
	}
	opts := fmt.Sprintf("%s,uid=%d,gid=%d", sshfsOptions, os.Getuid(), os.Getgid())
	cmd := exec.Command("unshare", "-m", "--propagation", "private", "sh", "-c", mountScript, "sh", d.cfg.Root, opts, d.cfg.Prof, d.cfg.Cfg)
	var errBuf limitedBuffer
	cmd.Stderr = io.MultiWriter(&errBuf, d.cfg.Log.Writer())
	inR, inW, err1 := os.Pipe()
	outR, outW, err2 := os.Pipe()
	if err := errors.Join(err1, err2); err != nil {
		d.mountErr <- err.Error()
		return
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		d.mountErr <- err.Error()
		return
	}
	inR.Close()
	outW.Close()
	d.mu.Lock()
	d.fsPid = cmd.Process.Pid
	d.mu.Unlock()
	os.WriteFile(filepath.Join(d.cfg.Run, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	go func() { io.Copy(inW, st); inW.Close() }()
	go func() { io.Copy(st, outR); outR.Close() }()
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		d.logf("sshfs exited: %v", err)
		close(exited)
		close(d.fsStopped)
		st.Close()
	}()
	// Mounted once the mount point answers inside the namespace.
	deadline := time.Now().Add(60 * time.Second)
	for {
		check := exec.Command("nsenter", "-t", strconv.Itoa(cmd.Process.Pid), "-m", "--", "mountpoint", "-q", d.cfg.Root)
		if check.Run() == nil {
			close(d.mounted)
			return
		}
		select {
		case <-exited:
			d.mountErr <- "the file mount on the server failed:\n" + strings.TrimSpace(errBuf.String())
			return
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			d.mountErr <- "timed out waiting for sshfs on the server"
			return
		}
	}
}

// terminal starts Claude Code in a pseudo-terminal on the stream.
func (d *daemon) terminal(st *yamux.Stream) {
	pc := proto.NewConn(st)
	f, err := pc.Read()
	var open Open
	if err != nil || f.Type != tOpen || json.Unmarshal(f.Payload, &open) != nil {
		st.Close()
		return
	}
	select {
	case <-d.mounted:
	case msg := <-d.mountErr:
		pc.Write(tError, []byte(msg))
		d.waitClosed(st)
		d.end("mount failed")
		return
	}
	master, slave, err := openPTY()
	if err != nil {
		pc.Write(tError, []byte("pseudo-terminal: "+err.Error()))
		d.waitClosed(st)
		d.end("no pseudo-terminal")
		return
	}
	setSize(master, open.Size)
	cmd := exec.Command(d.cfg.Launch[0], d.cfg.Launch[1:]...)
	cmd.Env = terminalEnv(os.Environ(), open.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		pc.Write(tError, []byte("starting Claude Code: "+err.Error()))
		d.waitClosed(st)
		d.end("launch failed")
		return
	}
	slave.Close()
	d.mu.Lock()
	d.term, d.claude = master, cmd
	d.mu.Unlock()

	output := make(chan struct{})
	go func() { // pseudo-terminal → rcc
		defer close(output)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if pc.Write(tData, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	closed := make(chan struct{})
	go func() { // rcc → pseudo-terminal
		defer close(closed)
		for {
			f, err := pc.Read()
			if err != nil {
				return
			}
			switch f.Type {
			case tData:
				master.Write(f.Payload)
			case tResize:
				var s Size
				if json.Unmarshal(f.Payload, &s) == nil {
					setSize(master, s)
				}
			case tHangup:
				syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
			case tRedraw:
				go d.redraw()
			}
		}
	}()

	err = cmd.Wait()
	close(d.claudeDone)
	code := exitCode(cmd.ProcessState, err)
	d.logf("Claude Code exited: %d", code)
	select { // what it printed last; background children may keep the terminal open
	case <-output:
	case <-time.After(500 * time.Millisecond):
	}
	pc.WriteJSON(tExit, Exit{Code: code})
	// rcc closes the stream once it has the exit code; it may first have
	// to reconnect, within the grace period.
	select {
	case <-closed:
	case <-d.finished:
	}
	d.end("Claude Code exited")
}

// waitClosed waits until rcc closes the stream (it has read the message).
func (d *daemon) waitClosed(st *yamux.Stream) {
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, st); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Minute):
	case <-d.finished:
	}
}

// redraw makes Claude Code repaint (rcc's status line took a line of the
// screen): a size change sends it SIGWINCH. Only the height changes, one
// row for a moment: Ink erases its last frame by counting lines, and a
// different width wraps them differently, leaving a copy of the frame on
// the screen.
func (d *daemon) redraw() {
	d.mu.Lock()
	m := d.term
	d.mu.Unlock()
	if m == nil {
		return
	}
	ws, err := unix.IoctlGetWinsize(int(m.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Row < 2 {
		return
	}
	setSize(m, Size{Rows: ws.Row - 1, Cols: ws.Col})
	time.Sleep(150 * time.Millisecond)
	// Unless rcc sent a new size meanwhile.
	if now, err := unix.IoctlGetWinsize(int(m.Fd()), unix.TIOCGWINSZ); err == nil && now.Row == ws.Row-1 && now.Col == ws.Col {
		setSize(m, Size{Rows: ws.Row, Cols: ws.Col})
	}
}

// end stops everything and removes the session's directory.
func (d *daemon) end(reason string) {
	d.ending.Do(func() {
		d.logf("ending: %s", reason)
		d.sessL.Close()
		d.cmdL.Close()
		d.mu.Lock()
		claude, fsPid, link, mux := d.claude, d.fsPid, d.link, d.mux
		d.mu.Unlock()
		if claude != nil {
			select {
			case <-d.claudeDone:
			default:
				syscall.Kill(-claude.Process.Pid, syscall.SIGHUP)
				select {
				case <-d.claudeDone:
				case <-time.After(5 * time.Second):
					syscall.Kill(-claude.Process.Pid, syscall.SIGKILL)
				}
			}
		}
		if mux != nil {
			mux.Close()
		}
		if link != nil {
			link.Close()
		}
		if fsPid != 0 {
			syscall.Kill(fsPid, syscall.SIGTERM) // sshfs; its namespace goes with it
			select {
			case <-d.fsStopped:
			case <-time.After(3 * time.Second):
				syscall.Kill(fsPid, syscall.SIGKILL)
			}
		}
		removeRun(d.cfg.Run, d.cfg.Root)
		close(d.finished)
	})
}

// removeRun removes the directories created for the mount point (recorded
// in RUN/created) and RUN itself.
func removeRun(run, root string) {
	if b, err := os.ReadFile(filepath.Join(run, "created")); err == nil && root != "" {
		first := strings.TrimSpace(string(b))
		for p := root; ; p = filepath.Dir(p) {
			if os.Remove(p) != nil || p == first || p == "/" {
				break
			}
		}
	}
	os.RemoveAll(run)
}

func terminalEnv(base []string, from map[string]string) []string {
	keep := map[string]bool{}
	for _, k := range TerminalEnv {
		keep[k] = true
	}
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !keep[k] && k != daemonChildEnv {
			env = append(env, kv)
		}
	}
	if from["TERM"] == "" {
		from = mapWith(from, "TERM", "xterm-256color")
	}
	for k, v := range from {
		if keep[k] && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func mapWith(m map[string]string, k, v string) map[string]string {
	n := map[string]string{k: v}
	for a, b := range m {
		if a != k {
			n[a] = b
		}
	}
	return n
}

func exitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err == nil {
		var n uint32
		if n, err = unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN); err == nil {
			slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
		}
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setSize(f *os.File, s Size) {
	if s.Rows == 0 || s.Cols == 0 {
		s = Size{Rows: 24, Cols: 80}
	}
	unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: s.Rows, Col: s.Cols})
}

// limitedBuffer keeps the first 8 KiB written (sshfs' error messages).
type limitedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := 8<<10 - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
