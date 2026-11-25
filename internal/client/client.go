// Package client runs remote-cc on the user's machine (A): a Claude Code
// session on the server with commands and files coming back to A, the
// account commands, and self-service device management.
//
// A session:
//
//  1. probe the server (the user's container), pick account and history
//     profile, install rcc-shim if needed
//  2. start the command agent on A
//  3. ssh #1: `sshfs -o slave` on the server ⇄ in-process SFTP server on A,
//     carrying a reverse forward of the server's command socket to the agent
//  4. ssh #2 (tty): Claude Code on the server, inside the mount
//  5. tear everything down when Claude exits
package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"remote-cc/internal/agent"
	"remote-cc/internal/embedded"
	"remote-cc/internal/fsserver"
	"remote-cc/internal/netexec"
	"remote-cc/internal/pathmap"
	"remote-cc/internal/procjob"
	"remote-cc/internal/rcb"
	"remote-cc/internal/remote"
	"remote-cc/internal/scripts"
)

// Config is a session (and, partly, an account command).
type Config struct {
	remote.Target          // the user's container, normally an alias like "server-cc"
	Dir           string   // project directory on A
	ClaudeArgs    []string // passed to claude
	ClaudePath    string   // override the detected claude path
	// Profile is the history store: "" = this machine's own, "none" = the
	// account's regular one, else a named profile.
	Profile string
	// Account is the Claude login: "" = this machine's own account, "default"
	// = the container's regular ~/.claude, else a named account.
	Account     string
	NoIsSandbox bool // do not set IS_SANDBOX=1 for claude as root
	RemoteGit   bool // Claude Code's own git calls run on the server, not here
	NoPrompt    bool // do not append remote-cc's system prompt
	// RCB runs claude as this remote-claude-browser profile: its account
	// (rcb-<id>) and, inside the egress host's namespace, its proxy node.
	RCB *rcb.Profile
	// RCBLink reaches the workbench RCB came from (grants, exit checks, logins).
	RCBLink *rcb.Link
}

// rcbVars are the run/auth script parameters for a profile ("" without one).
func rcbVars(p *rcb.Profile, checkEmail bool) string {
	if p == nil || p.Node == nil {
		return ""
	}
	ip, tz, email := "", "", ""
	if p.Exit != nil {
		ip, tz = p.Exit.IP, p.Exit.TimeZone
	}
	if checkEmail && p.SignedIn {
		email = p.Email
	}
	return remote.Vars("RCB_NODE", p.Node.ID, "RCB_EXPECT_IP", ip, "RCB_TZ", tz, "RCB_EMAIL", email, "RCB_LABEL", p.Name)
}

type session struct {
	cfg   Config
	conn  *remote.Conn
	id    string
	grant string // the session's grant from remote-claude-browser (--rcb)

	local, remote string // project dir on A (native) and on the server
	facts         map[string]string
	acct          string // account config dir, "" = default
	autoAccount   bool   // acct is this machine's own account
	cfg0, ccfg    string // default config dir; the one claude uses
	prof          string // history profile dir, "" = none
	run, cache    string // per-session dir and rcc cache on the server
	sock          string // command socket on the server

	mu      sync.Mutex
	stopped bool        // SIGTERM/SIGHUP received
	runProc *os.Process // interactive ssh, killed on stop
}

func newSession(cfg Config) (*session, error) {
	conn, err := remote.Open(cfg.Target)
	if err != nil {
		return nil, err
	}
	s := &session{cfg: cfg, conn: conn, id: remote.RandHex(6)}
	f, err := conn.Facts(scripts.Get("client/probe.sh"))
	if err != nil {
		conn.Close()
		return nil, err
	}
	if cfg.ClaudePath != "" {
		f["claude"] = cfg.ClaudePath
	}
	s.facts = f
	conn.Logf("server: %s/%s uid=%s claude=%s", f["os"], f["arch"], f["uid"], f["claude"])
	if f["claude"] == "" {
		conn.Close()
		return nil, s.claudeMissing()
	}
	if err := s.resolveAccount(); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// Run runs an interactive Claude Code session.
func Run(cfg Config) (int, error) {
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return 1, err
	}
	local, err := filepath.EvalSymlinks(abs) // claude tracks cwd with `pwd -P`
	if err != nil {
		return 1, err
	}
	s, err := newSession(cfg)
	if err != nil {
		return 1, err
	}
	defer s.conn.Close()
	s.local, s.remote = local, pathmap.ToRemote(runtime.GOOS, local)

	// SIGTERM/SIGHUP (terminal closed, kill): end the interactive ssh and fall
	// through to the normal teardown instead of leaving server state behind.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			s.mu.Lock()
			s.stopped = true
			if s.runProc != nil {
				s.runProc.Kill()
			}
			s.mu.Unlock()
		}
	}()
	if err := s.check(); err != nil {
		return 1, err
	}
	return s.startRCB()
}

func (s *session) check() error {
	f := s.facts
	var problems []string
	if f["os"] != "Linux" {
		problems = append(problems, fmt.Sprintf("the server is %q; only Linux is supported", f["os"]))
	}
	if f["uid"] != "0" || f["bin_unshare"] == "" || f["bin_nsenter"] == "" || f["bin_mountpoint"] == "" {
		problems = append(problems, "the server must be root with unshare, nsenter and mountpoint (the remote-cc container provides this)")
	}
	if f["bin_sshfs"] == "" || f["fuse"] == "" {
		problems = append(problems, "the server needs sshfs and /dev/fuse (the remote-cc container provides this)")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n     "))
	}
	profile := s.cfg.Profile
	if profile == "" {
		var err error
		if profile, err = MachineProfile(); err != nil {
			return err
		}
	}
	if profile != "none" {
		if !validProfile(profile) {
			return fmt.Errorf("invalid profile name %q (use letters, digits, '.', '_', '-')", profile)
		}
		// A named account keeps its history apart from this machine's own.
		if s.acct != "" && !s.autoAccount {
			profile += "@" + s.cfg.Account
		}
		s.prof = f["home"] + "/.local/share/rcc/profiles/" + profile
	}
	s.conn.Logf("history profile: %s", map[bool]string{true: s.prof, false: "the account's own"}[s.prof != ""])
	s.cache = f["home"] + "/.cache/rcc"
	s.run = s.cache + "/run/" + s.id
	s.sock = s.run + "/s"
	return nil
}

// shimBinary picks the rcc-shim build for the server's architecture.
func (s *session) shimBinary() ([]byte, error) {
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[s.facts["arch"]]
	if goarch == "" {
		return nil, fmt.Errorf("unsupported server architecture %q", s.facts["arch"])
	}
	if b := embedded.Shim(goarch); b != nil {
		return b, nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == goarch {
		// Same OS/arch: upload ourselves; installed as rcc-shim, rcc runs as the
		// shim when invoked under that name.
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return os.ReadFile(exe)
	}
	return nil, fmt.Errorf("this rcc build has no rcc-shim for linux/%s; build with scripts/build.sh", goarch)
}

func (s *session) start() (int, error) {
	token := remote.RandHex(16)
	shim, err := s.shimBinary()
	if err != nil {
		return 1, err
	}
	sum := sha256.Sum256(shim)
	shimSHA := hex.EncodeToString(sum[:])
	need := !strings.Contains(","+s.facts["shims"], ","+shimSHA+",")
	s.conn.Logf("mount=%s shim upload=%v", s.remote, need)

	common := remote.Vars("RUN", s.run, "CACHE", s.cache, "SOCK", s.sock, "R", s.remote,
		"PROF", s.prof, "CFG", s.ccfg, "ACCT_DIR", s.acct, "DEFCFG", s.cfg0, "SHIM_SHA", shimSHA)
	// Registered before prepare, so a failed prepare leaves nothing behind.
	defer s.conn.Script(false, nil, common+scripts.Get("client/cleanup.sh")).Run()

	prep := s.conn.Script(false, nil, common+remote.Vars("NEED_SHIM", remote.Bool(need))+scripts.Client("client/prepare.sh"))
	prep.Stdin = io.MultiReader(strings.NewReader(token+"\n"), bytes.NewReader(map[bool][]byte{true: shim}[need]))
	prep.Stderr = os.Stderr
	if err := prep.Run(); err != nil {
		return 1, fmt.Errorf("preparing the server: %w", err)
	}

	// Command agent.
	logFile := s.openLog()
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)
	ag, err := agent.New(agent.Options{Token: token, Logger: logger, TempDir: s.conn.Tmp()})
	if err != nil {
		return 1, err
	}
	logger.Printf("shell: %s", ag.Shell())
	s.conn.Logf("local shell: %s", ag.Shell())
	if err := ag.Check(s.local); err != nil {
		logger.Printf("shell check failed: %v", err)
		return 1, fmt.Errorf("Claude's commands could not run on this machine:\n     %v\n     Set RCC_SHELL to a working bash (on Windows: Git for Windows' bash.exe, e.g. C:\\Program Files\\Git\\bin\\bash.exe)", err)
	}
	claudeArgs := s.cfg.ClaudeArgs
	if !s.cfg.NoPrompt {
		var added bool
		claudeArgs, added = withSystemPrompt(claudeArgs, systemPrompt(s.remote, ag.Shell()))
		s.conn.Logf("system prompt appended: %v", added)
	}
	var l net.Listener
	var target string
	if runtime.GOOS == "windows" {
		l, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			target = l.Addr().String()
		}
	} else {
		target = filepath.Join(s.conn.Tmp(), "a.sock")
		l, err = net.Listen("unix", target)
	}
	if err != nil {
		return 1, err
	}
	defer l.Close()
	go ag.Serve(l)

	// File channel, carrying the reverse forward of the command socket.
	fs := s.conn.Script(false, []string{"-o", "ExitOnForwardFailure=yes", "-R", s.sock + ":" + target}, common+scripts.Get("client/mount.sh"))
	fsIn, _ := fs.StdinPipe()
	fsOut, _ := fs.StdoutPipe()
	var fsErr bytes.Buffer
	fs.Stderr = io.MultiWriter(&fsErr, logFile)
	if err := fs.Start(); err != nil {
		return 1, err
	}
	procjob.TrackProcess(fs.Process.Pid)
	ready := make(chan struct{})
	go fsserver.Serve(rwc{&firstRead{r: fsOut, ready: ready}, fsIn}, s.remote)
	fsExited := make(chan struct{})
	go func() { fs.Wait(); close(fsExited) }()
	select {
	case <-ready:
	case <-fsExited:
		return 1, fmt.Errorf("the file mount on the server failed:\n%s", strings.TrimSpace(fsErr.String()))
	case <-time.After(60 * time.Second):
		return 1, errors.New("timed out waiting for sshfs on the server")
	}

	// Claude Code, interactive.
	sandbox := s.facts["uid"] == "0" && !s.cfg.NoIsSandbox
	run := s.conn.Script(true, nil, common+remote.Vars("CLAUDE", s.facts["claude"], "SANDBOX", remote.Bool(sandbox), "LOCAL_GIT", remote.Bool(!s.cfg.RemoteGit))+
		rcbVars(s.cfg.RCB, true)+remote.Vars("RCB_GRANT", s.grant)+remote.SetArgs(claudeArgs)+scripts.Client("client/run.sh"))
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The tty is in raw mode while ssh runs; keep stray signals from killing
	// rcc before it can clean up.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return 1, errors.New("interrupted")
	}
	err = run.Start()
	if err == nil {
		s.runProc = run.Process
		procjob.TrackProcess(run.Process.Pid) // TERM lets ssh restore the terminal if rcc dies
	}
	s.mu.Unlock()
	if err != nil {
		return 1, err
	}
	code, err := remote.ExitCode(run.Wait())
	if err != nil {
		return 1, err
	}
	select {
	case <-fsExited:
		if code != 0 && fsErr.Len() > 0 {
			fmt.Fprintf(os.Stderr, "rcc: file channel output:\n%s\n", strings.TrimSpace(fsErr.String()))
		}
	default:
	}
	// Teardown: closing sshfs' stdin makes it exit and unmount.
	fsIn.Close()
	select {
	case <-fsExited:
	case <-time.After(3 * time.Second):
		fs.Process.Kill()
	}
	return code, nil
}

func (s *session) openLog() *os.File {
	if dir, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(dir, "rcc")
		if os.MkdirAll(dir, 0o700) == nil {
			// Keep the previous session's log too: problems are often
			// reported after another session has started.
			last := filepath.Join(dir, "last.log")
			os.Rename(last, filepath.Join(dir, "prev.log"))
			if f, err := os.Create(last); err == nil {
				s.conn.Logf("log file: %s", f.Name())
				return f
			}
		}
	}
	f, _ := os.Create(filepath.Join(s.conn.Tmp(), "log"))
	return f
}

// claudeMissing explains a missing claude, spotting the usual mistake of
// connecting to the Docker host instead of the user's container.
func (s *session) claudeMissing() error {
	if s.facts["rccserver"] == "1" {
		return errors.New("claude is missing inside the remote-cc container; update the server (update-server), or pass --claude PATH")
	}
	return fmt.Errorf("claude was not found on %s (host %q).\n"+
		"     If this is the Docker host of a remote-cc server, connect to your container instead:\n"+
		"     use the alias that setup-server or join configured (e.g. <server>-cc).\n"+
		"     Otherwise install Claude Code there or pass --claude PATH", s.cfg.Dest, s.facts["hostname"])
}

// firstRead closes ready once the first byte arrives (ssh is authenticated
// and sshfs is talking).
type firstRead struct {
	r     io.Reader
	ready chan struct{}
	once  sync.Once
}

func (f *firstRead) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.once.Do(func() { close(f.ready) })
	}
	return n, err
}

type rwc struct {
	io.Reader
	w io.WriteCloser
}

func (x rwc) Write(p []byte) (int, error) { return x.w.Write(p) }
func (x rwc) Close() error                { return x.w.Close() }

// startRCB starts the session; with --rcb it does so through rcbRun.
func (s *session) startRCB() (int, error) {
	if s.cfg.RCB == nil {
		return s.start()
	}
	return s.rcbRun(func(grant string) (int, error) {
		s.grant = grant
		return s.start()
	})
}

// rcbRun runs fn (a session or an account command) as the --rcb profile with
// a fresh grant. Without a recorded exit any exit would pass the node check,
// so it has the workbench check the exit first and refuses if it cannot.
// When the node exits elsewhere than recorded, it has the exit re-checked
// and runs fn once more with the fresh one.
func (s *session) rcbRun(fn func(grant string) (int, error)) (int, error) {
	p := s.cfg.RCB
	if s.cfg.RCBLink == nil {
		return 1, errors.New("--rcb needs this machine linked to remote-claude-browser")
	}
	if p.Exit == nil || p.Exit.IP == "" || p.Exit.TimeZone == "" {
		fmt.Fprintln(os.Stderr, "rcc: remote-claude-browser has no checked exit for this profile yet; checking it now …")
		exit, err := rcb.Check(*s.cfg.RCBLink, p.ID)
		if err == nil && exit.IP == "" {
			err = errors.New("no exit address reported")
		}
		if err != nil {
			return 1, fmt.Errorf("could not check profile %s's exit through node %s (%v); refusing to run without a known exit", p.Label(), p.Node.Name, err)
		}
		fmt.Fprintf(os.Stderr, "rcc: the profile's exit is %s (%s)\n", exit.IP, exit.TimeZone)
		p.Exit = &exit
	}
	for attempt := 0; ; attempt++ {
		g, err := rcb.Grant(*s.cfg.RCBLink, p.ID)
		if err != nil {
			return 1, err
		}
		code, err := fn(g)
		if err != nil || code != netexec.ExitMismatch || attempt > 0 {
			return code, err
		}
		fmt.Fprintln(os.Stderr, "rcc: re-checking the profile's exit with remote-claude-browser …")
		exit, err := rcb.Check(*s.cfg.RCBLink, p.ID)
		if err != nil {
			return code, err
		}
		if exit.IP == p.Exit.IP {
			return code, fmt.Errorf("remote-claude-browser still sees exit %s for node %s; the egress host reaches the node differently", exit.IP, p.Node.Name)
		}
		fmt.Fprintf(os.Stderr, "rcc: the profile's exit is now %s (%s); starting again\n", exit.IP, exit.TimeZone)
		p.Exit = &exit
	}
}
