// Package client runs remote-cc on the user's computer: a Claude Code
// session on the server with commands and files coming back here, the
// account commands, and self-service device management.
//
// A session:
//
//  1. probe the server (the user's container) and pick account and history
//     profile, while the local shell check and, with --rcb, the grant from
//     remote-claude-browser run alongside; upload rcc-shim if needed
//  2. one ssh channel: client/session.sh starts the session daemon on the
//     server (internal/session) and connects to it; Claude Code's terminal,
//     the sshfs file channel and the commands all run over it
//  3. if the connection drops, reconnect (client/dial.sh) and resume
//  4. when Claude Code exits, the daemon ends the server side
package client

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
	"remote-cc/internal/resume"
	"remote-cc/internal/scripts"
	live "remote-cc/internal/session"
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

	hangup   chan struct{}      // closed on SIGTERM/SIGHUP: end Claude Code
	prefetch <-chan grantResult // --rcb: the first grant, fetched during the probe
	// held is the probe's ssh session, waiting to start the session on (nil
	// once used, and for account commands).
	held *remote.Held
}

// newSession probes the server. With hold, the probe's ssh session stays
// open to start the session on (s.held; close ends it if unused).
func newSession(cfg Config, hold bool) (*session, error) {
	conn, err := remote.Open(cfg.Target)
	if err != nil {
		return nil, err
	}
	s := &session{cfg: cfg, conn: conn, id: remote.RandHex(6)}
	var f map[string]string
	if hold {
		f, s.held, err = conn.FactsHeld(scripts.Get("client/probe.sh"))
	} else {
		f, err = conn.Facts(scripts.Get("client/probe.sh"))
	}
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
		s.close()
		return nil, s.claudeMissing()
	}
	if err := s.resolveAccount(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// close ends the held probe session, if unused, and the connection.
func (s *session) close() {
	if s.held != nil {
		s.held.Close()
	}
	s.conn.Close()
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
	logf := func(f string, a ...any) {
		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "rcc: "+f+"\n", a...)
		}
	}
	logFile := openLog(logf)
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)

	// While ssh connects and probes: check the local shell, and with --rcb
	// get the grant from remote-claude-browser.
	agentTmp, err := os.MkdirTemp("", "rcc-agent-")
	if err != nil {
		return 1, err
	}
	procjob.RemoveDirOnExit(agentTmp)
	defer os.RemoveAll(agentTmp)
	// One token: rcc-shim on the server presents it to the agent here.
	token := remote.RandHex(16)
	ag, err := agent.New(agent.Options{Token: token, Logger: logger, TempDir: agentTmp})
	if err != nil {
		return 1, err
	}
	logger.Printf("shell: %s", ag.Shell())
	logf("local shell: %s", ag.Shell())
	shellOK := make(chan error, 1)
	go func() { shellOK <- ag.Check(local) }()
	var prefetch <-chan grantResult
	if cfg.RCB != nil && cfg.RCBLink != nil {
		prefetch = firstGrant(cfg)
	}

	s, err := newSession(cfg, true)
	if err != nil {
		return 1, err
	}
	defer s.close()
	s.local, s.remote = local, pathmap.ToRemote(runtime.GOOS, local)
	s.prefetch = prefetch
	if err := <-shellOK; err != nil {
		logger.Printf("shell check failed: %v", err)
		return 1, fmt.Errorf("Claude's commands could not run on this machine:\n     %v\n     Set RCC_SHELL to a working bash (on Windows: Git for Windows' bash.exe, e.g. C:\\Program Files\\Git\\bin\\bash.exe)", err)
	}

	// SIGTERM/SIGHUP (terminal closed, kill): end Claude Code on the server
	// and fall through to the normal teardown.
	s.hangup = make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		close(s.hangup)
	}()
	if err := s.check(); err != nil {
		return 1, err
	}
	return s.startRCB(func(grant string) (int, error) {
		s.grant = grant
		return s.start(ag, token, logger, logFile)
	})
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

// ensureShim puts this rcc's rcc-shim on the server, if it is not there
// yet (gzip-compressed when the server has gzip), and returns its sha256.
func (s *session) ensureShim() (string, error) {
	shim, err := s.shimBinary()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(shim)
	sha := hex.EncodeToString(sum[:])
	if strings.Contains(","+s.facts["shims"], ","+sha+",") {
		s.conn.Logf("rcc-shim: already on the server")
		return sha, nil
	}
	gz := s.facts["bin_gzip"] != ""
	body := shim
	if gz {
		var b bytes.Buffer
		w, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
		w.Write(shim)
		w.Close()
		body = b.Bytes()
	}
	s.conn.Logf("rcc-shim: uploading %d KiB", len(body)>>10)
	c := s.conn.Script(false, nil, remote.Vars("CACHE", s.facts["home"]+"/.cache/rcc", "SHIM_SHA", sha, "GZ", remote.Bool(gz))+scripts.Get("client/upload.sh"))
	c.Stdin = bytes.NewReader(body)
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("installing rcc-shim on the server: %w", err)
	}
	s.facts["shims"] += sha + ","
	return sha, nil
}

func (s *session) start(ag *agent.Agent, token string, logger *log.Logger, logFile *os.File) (int, error) {
	shimSHA, err := s.ensureShim()
	if err != nil {
		return 1, err
	}
	common := remote.Vars("RUN", s.run, "CACHE", s.cache, "R", s.remote, "PROF", s.prof, "CFG", s.ccfg,
		"ACCT_DIR", s.acct, "DEFCFG", s.cfg0, "SHIM_SHA", shimSHA)
	// Normally the daemon has already removed everything; this covers a
	// session that never started.
	defer func() {
		c := s.conn.Script(false, []string{"-o", "ConnectTimeout=10"}, common+scripts.Get("client/cleanup.sh"))
		c.Stdout, c.Stderr = logFile, logFile
		c.Run()
	}()

	claudeArgs := s.cfg.ClaudeArgs
	if !s.cfg.NoPrompt {
		var added bool
		claudeArgs, added = withSystemPrompt(claudeArgs, systemPrompt(s.remote, ag.Shell()))
		s.conn.Logf("system prompt appended: %v", added)
	}
	sandbox := s.facts["uid"] == "0" && !s.cfg.NoIsSandbox
	launch := common + remote.Vars("CLAUDE", s.facts["claude"], "SANDBOX", remote.Bool(sandbox), "LOCAL_GIT", remote.Bool(!s.cfg.RemoteGit)) +
		rcbVars(s.cfg.RCB, true) + remote.Vars("RCB_GRANT", s.grant) + remote.SetArgs(claudeArgs) + scripts.Client("client/launch.sh")

	var startErr bytes.Buffer
	connect := func(first bool) (io.ReadWriteCloser, error) {
		script := common + scripts.Get("client/dial.sh")
		var stderr io.Writer = logFile
		if first {
			script = common + remote.Vars("LAUNCH_B64", remote.Base64(launch)) + scripts.Client("client/session.sh")
			stderr = io.MultiWriter(&startErr, logFile)
		} else {
			logger.Printf("reconnecting")
		}
		if first && s.held != nil { // the probe's ssh session waits for it
			h := s.held
			s.held = nil
			return h.Start(script, stderr)
		}
		var opts []string
		if !first { // the network may still be half there
			opts = []string{"-o", "ConnectTimeout=15"}
		}
		return remote.StartPipe(s.conn.Script(false, opts, script), stderr)
	}

	// Raw mode carries Ctrl-C to Claude Code; without a terminal, keep it
	// from killing rcc before it can clean up.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	s.conn.Logf("mount=%s", s.remote)
	code, err := live.Run(live.ClientConfig{
		Token:    token,
		Connect:  connect,
		Files:    func(rw io.ReadWriteCloser) error { return fsserver.Serve(rw, s.remote) },
		Commands: ag.Serve,
		Stdin:    os.Stdin,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Terminal: live.StdTerminal(),
		Env:      live.LocalEnv(),
		Hangup:   s.hangup,
		Logf:     logger.Printf,
		Reconnecting: func(reason error) {
			// Nothing heard: the shared ssh connection most likely went
			// down with the session's (or is stalled with it), and new
			// ssh calls through it would wait for it until ssh gives up
			// on it. While the session's connection may yet carry on,
			// the shared one is only retired: ending it would end that
			// too. Other reasons (the server side closed the connection)
			// leave it alone.
			switch {
			case errors.Is(reason, live.ErrQuiet):
				s.conn.RetireMaster()
			case errors.Is(reason, resume.ErrSilent):
				s.conn.DropMaster()
			}
		},
	})
	var se *live.StartError
	if errors.As(err, &se) && strings.TrimSpace(startErr.String()) != "" {
		err = fmt.Errorf("starting the session on the server failed:\n%s", strings.TrimSpace(startErr.String()))
	}
	return code, err
}

func openLog(logf func(string, ...any)) *os.File {
	if dir, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(dir, "rcc")
		if os.MkdirAll(dir, 0o700) == nil {
			// Keep the previous session's log too: problems are often
			// reported after another session has started.
			// The log names the commands Claude Code ran here: only this
			// user may read it.
			last := filepath.Join(dir, "last.log")
			err := os.Rename(last, filepath.Join(dir, "prev.log"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				// Windows: another session still writes it. Never
				// truncate that one; this session logs on its own.
				last = filepath.Join(dir, fmt.Sprintf("session-%d.log", os.Getpid()))
			}
			if old, _ := filepath.Glob(filepath.Join(dir, "session-*.log")); len(old) > 0 {
				for _, p := range old {
					if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
						os.Remove(p)
					}
				}
			}
			if f, err := os.OpenFile(last, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
				f.Chmod(0o600) // an older rcc made it 0644
				logf("log file: %s", f.Name())
				return f
			}
		}
	}
	f, _ := os.CreateTemp("", "rcc-log-")
	return f
}

// claudeMissing explains a missing claude, spotting the usual mistake of
// connecting to the Docker host instead of the user's container.
func (s *session) claudeMissing() error {
	if s.facts["rccserver"] == "1" {
		return errors.New("claude is missing inside the remote-cc container; update the server (server update), or pass --claude PATH")
	}
	return fmt.Errorf("claude was not found on %s (host %q).\n"+
		"     If this is the Docker host of a remote-cc server, connect to your container instead:\n"+
		"     use the alias that server setup or device join configured (e.g. <server>-cc).\n"+
		"     Otherwise install Claude Code there or pass --claude PATH", s.cfg.Dest, s.facts["hostname"])
}

// startRCB runs fn (the session), through rcbRun with --rcb.
func (s *session) startRCB(fn func(grant string) (int, error)) (int, error) {
	if s.cfg.RCB == nil {
		return fn("")
	}
	return s.rcbRun(fn)
}

type grantResult struct {
	grant string
	err   error
}

// firstGrant gets a profile's first grant in the background: without a
// recorded exit any exit would pass the node check, so it first has the
// workbench check the exit, and refuses if it cannot.
func firstGrant(cfg Config) <-chan grantResult {
	ch := make(chan grantResult, 1)
	go func() {
		p := cfg.RCB
		if p.Exit == nil || p.Exit.IP == "" || p.Exit.TimeZone == "" {
			fmt.Fprintln(os.Stderr, "rcc: remote-claude-browser has no checked exit for this profile yet; checking it now …")
			exit, err := rcb.Check(*cfg.RCBLink, p.ID)
			if err == nil && exit.IP == "" {
				err = errors.New("no exit address reported")
			}
			if err != nil {
				ch <- grantResult{err: fmt.Errorf("could not check profile %s's exit through node %s (%v); refusing to run without a known exit", p.Label(), p.Node.Name, err)}
				return
			}
			fmt.Fprintf(os.Stderr, "rcc: the profile's exit is %s (%s)\n", exit.IP, exit.TimeZone)
			p.Exit = &exit
		}
		g, err := rcb.Grant(*cfg.RCBLink, p.ID)
		ch <- grantResult{g, err}
	}()
	return ch
}

// rcbRun runs fn (a session or an account command) as the --rcb profile with
// a fresh grant. When the node exits elsewhere than recorded, it has the exit
// re-checked and runs fn once more with the fresh one.
func (s *session) rcbRun(fn func(grant string) (int, error)) (int, error) {
	p := s.cfg.RCB
	if s.cfg.RCBLink == nil {
		return 1, errors.New("--rcb needs this machine linked to remote-claude-browser")
	}
	first := s.prefetch
	if first == nil {
		first = firstGrant(s.cfg)
	}
	r := <-first
	if r.err != nil {
		return 1, r.err
	}
	g := r.grant
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			var err error
			if g, err = rcb.Grant(*s.cfg.RCBLink, p.ID); err != nil {
				return 1, err
			}
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
