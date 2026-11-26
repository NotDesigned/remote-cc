// Package agent is the A-side executor: it accepts connections from rcc-shim
// (forwarded from B over ssh) and runs each requested command locally.
package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"remote-cc/internal/pathmap"
	"remote-cc/internal/procjob"
	"remote-cc/internal/proto"
)

type Options struct {
	Token string
	// Shell is the bash-compatible shell used to run commands. Empty means
	// auto-detect (see FindShell).
	Shell string
	// Logger receives one line per command; nil disables logging.
	Logger *log.Logger
	// TempDir holds per-command temp files; empty means the system default.
	// rcc passes its private dir, which is removed even if rcc is killed.
	TempDir string
}

type Agent struct {
	opt   Options
	shell string
}

func New(opt Options) (*Agent, error) {
	sh := opt.Shell
	if sh == "" {
		var err error
		if sh, err = FindShell(); err != nil {
			return nil, err
		}
	}
	return &Agent{opt: opt, shell: sh}, nil
}

func (a *Agent) Shell() string { return a.shell }

// Check runs a trivial command the way Claude's commands will run, so that a
// broken shell is reported before Claude starts instead of as silent failures.
func (a *Agent) Check(dir string) error {
	cmd := exec.Command(a.shell, "-c", wrapScript("echo rcc-shell-ok"))
	cmd.Dir = dir
	setProcessGroup(cmd)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Errorf("%s did not answer within 20s", a.shell)
	}
	if strings.Contains(string(out), "rcc-shell-ok") {
		return nil
	}
	msg := strings.TrimSpace(strings.ReplaceAll(string(out), "\x00", "")) // UTF-16 noise
	if msg == "" {
		msg = "(no output)"
	}
	return fmt.Errorf("%s -c 'echo rcc-shell-ok' failed (%v): %s", a.shell, err, msg)
}

// Serve accepts connections until l is closed.
func (a *Agent) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go a.handle(c)
	}
}

func (a *Agent) logf(format string, args ...any) {
	if a.opt.Logger != nil {
		a.opt.Logger.Printf(format, args...)
	}
}

func (a *Agent) handle(nc net.Conn) {
	defer nc.Close()
	c := proto.NewConn(nc)

	nc.SetReadDeadline(time.Now().Add(15 * time.Second))
	f, err := c.Read()
	if err != nil || f.Type != proto.TStart {
		return
	}
	nc.SetReadDeadline(time.Time{})

	var st proto.Start
	if err := json.Unmarshal(f.Payload, &st); err != nil {
		c.Write(proto.TError, []byte("rcc: bad start frame"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(st.Token), []byte(a.opt.Token)) != 1 {
		c.Write(proto.TError, []byte("rcc: bad token"))
		return
	}
	if st.Version != proto.Version {
		c.Write(proto.TError, []byte(fmt.Sprintf("rcc: protocol version mismatch (shim %d, agent %d); update rcc on both machines", st.Version, proto.Version)))
		return
	}

	dir := pathmap.ToLocal(runtime.GOOS, st.Cwd)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		c.Write(proto.TError, []byte(fmt.Sprintf("rcc: working directory %q does not exist on the local machine", dir)))
		return
	}

	exit, err := a.run(c, &st, dir)
	if err != nil {
		c.Write(proto.TError, []byte("rcc: "+err.Error()))
		return
	}
	c.WriteJSON(proto.TExit, exit)
}

func (a *Agent) run(c *proto.Conn, st *proto.Start, dir string) (*proto.Exit, error) {
	var cmd *exec.Cmd
	var flush func() error
	stdout := io.Writer(proto.ChunkWriter{C: c, T: proto.TStdout})
	if len(st.Argv) > 0 && st.Argv[0] == "rg" {
		r, err := rgCommand(st.Argv[1:], st.Paths)
		if err != nil {
			// The shim then searches on the server instead.
			return nil, errors.New(proto.RgMissing)
		}
		cmd = r
		if runtime.GOOS == "windows" {
			dw := &driveWriter{w: stdout}
			stdout, flush = dw, dw.Flush
		}
		st.Cmd = strings.Join(st.Argv, " ") // for the log
	} else if len(st.Argv) > 0 {
		if st.Argv[0] != "git" {
			return nil, fmt.Errorf("cannot run %q directly", st.Argv[0])
		}
		g, err := gitCommand(st.Argv[1:])
		if err != nil {
			c.Write(proto.TStderr, []byte("git: not installed on the local machine (rcc runs Claude Code's git commands there)\n"))
			return &proto.Exit{Code: 127}, nil
		}
		cmd = g
		if runtime.GOOS == "windows" {
			dw := &driveWriter{w: stdout}
			stdout, flush = dw, dw.Flush
		}
		st.Cmd = strings.Join(st.Argv, " ") // for the log
	} else {
		cmd = exec.Command(a.shell, "-c", wrapScript(st.Cmd))
	}
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), st.Env)
	cmd.Stdout = stdout
	cmd.Stderr = proto.ChunkWriter{C: c, T: proto.TStderr}
	// Background jobs may keep stdout open forever; stop waiting for them
	// shortly after the shell itself exits.
	cmd.WaitDelay = time.Second
	setProcessGroup(cmd)

	var cwdFile string
	if st.CaptureCwd {
		tf, err := os.CreateTemp(a.opt.TempDir, "rcc-cwd-")
		if err != nil {
			return nil, err
		}
		tf.Close()
		cwdFile = tf.Name()
		defer os.Remove(cwdFile)
		cmd.Env = append(cmd.Env, "RCC_CWD_FILE="+filepath.ToSlash(cwdFile))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// The command leads its own session/process group (pgid == pid).
	procjob.TrackGroup(cmd.Process.Pid)
	a.logf("run pid=%d cwd=%s cmd=%s", cmd.Process.Pid, dir, summarize(st.Cmd))

	// Pump frames from the shim: stdin, signals, and disconnects.
	go func() {
		for {
			f, err := c.Read()
			if err != nil {
				// Shim went away (killed, or ssh dropped): take the job down.
				signalGroup(cmd, "KILL")
				stdin.Close()
				return
			}
			switch f.Type {
			case proto.TStdin:
				stdin.Write(f.Payload)
			case proto.TStdinEOF:
				stdin.Close()
			case proto.TSignal:
				var s proto.Signal
				if json.Unmarshal(f.Payload, &s) == nil {
					a.logf("signal %s -> pid=%d", s.Name, cmd.Process.Pid)
					signalGroup(cmd, s.Name)
				}
			}
		}
	}()

	werr := cmd.Wait()
	if flush != nil {
		flush()
	}
	ex := exitInfo(cmd.ProcessState)
	if werr != nil && cmd.ProcessState == nil {
		return nil, werr
	}
	if cwdFile != "" {
		if b, err := os.ReadFile(cwdFile); err == nil {
			ex.Cwd = strings.TrimSpace(string(b))
		}
	}
	a.logf("exit pid=%d code=%d %s", cmd.Process.Pid, ex.Code, time.Since(start).Round(time.Millisecond))
	return ex, nil
}

func mergeEnv(base []string, over map[string]string) []string {
	if len(over) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := over[k]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}

// secretRe finds what looks like a secret in a command line (a value after
// token=, password: …, a bearer token, or an Anthropic or RCB key), so the
// log keeps only its name.
var secretRe = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|api[_-]?key|authorization)["']?\s*[=:]\s*["']?(?:bearer\s+)?|bearer\s+|\b(?:sk-ant-|rcc[a-z]_))[^\s"'⏎]+`)

func summarize(s string) string {
	// Show just the user's command, not Claude's setup preamble.
	if i := strings.LastIndex(s, "eval "); i >= 0 {
		s = s[i+5:]
	}
	s = strings.ReplaceAll(s, "\n", "⏎")
	s = secretRe.ReplaceAllString(s, "${1}…")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// FindShell locates a bash-compatible shell. $RCC_SHELL wins.
func FindShell() (string, error) {
	if s := os.Getenv("RCC_SHELL"); s != "" {
		return s, nil
	}
	if runtime.GOOS == "windows" {
		return findGitBash()
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p, nil
	}
	return "", errors.New("no bash or sh found; set RCC_SHELL")
}

// findGitBash avoids C:\Windows\System32\bash.exe, which is the WSL launcher
// and would run commands inside a different filesystem.
func findGitBash() (string, error) {
	// Standard installs first: the git found in PATH may belong to Anaconda,
	// MSYS2 or Cygwin, whose bash behaves differently or not at all here.
	var cands []string
	for _, env := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
		if v := os.Getenv(env); v != "" {
			cands = append(cands, filepath.Join(v, "Git", "bin", "bash.exe"))
		}
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		cands = append(cands, filepath.Join(v, "Programs", "Git", "bin", "bash.exe"))
	}
	if g, err := exec.LookPath("git"); err == nil {
		// ...\Git\cmd\git.exe -> ...\Git\bin\bash.exe (a portable Git for Windows)
		root := filepath.Dir(filepath.Dir(g))
		if _, err := os.Stat(filepath.Join(root, "cmd", "git.exe")); err == nil {
			cands = append(cands, filepath.Join(root, "bin", "bash.exe"))
		}
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("Git Bash not found; install Git for Windows or set RCC_SHELL to a bash.exe")
}

var _ io.Writer = proto.ChunkWriter{}
