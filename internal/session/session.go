// Package session orchestrates a full rcc session from machine A:
//
//  1. probe B (arch, tools, claude path) and install rcc-shim if needed
//  2. start the command agent on A
//  3. ssh session #1: `sshfs -o slave` on B ⇄ in-process SFTP server on A,
//     carrying a reverse forward of B's command socket to the agent
//  4. ssh session #2 (with a tty): Claude Code on B, inside the mount
//  5. tear everything down when Claude exits
package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
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
	"remote-cc/internal/pathmap"
	"remote-cc/internal/procjob"
)

type Config struct {
	Destination string   // [user@]host, as for ssh
	SSHArgs     []string // extra ssh options placed before the destination
	Dir         string   // project directory on A
	ClaudeArgs  []string // passed through to claude on B
	ClaudePath  string   // override auto-detected claude path on B
	Mode        string   // auto | ns | global
	Verbose     bool
	// Profile selects the conversation history store on B. "" means this A
	// machine's own profile; "none" means B's regular ~/.claude (shared).
	Profile string
	// NoIsSandbox disables exporting IS_SANDBOX=1 for claude when B's user
	// is root (needed there for --dangerously-skip-permissions).
	NoIsSandbox bool
	// NoPrompt disables appending remote-cc's system prompt (which tells
	// Claude that commands run on this machine, not on the remote host).
	NoPrompt bool
	// Account selects a Claude account on B: each has its own config dir
	// (credentials, .claude.json, org policy) via CLAUDE_CONFIG_DIR. ""
	// means this machine's own account (named like its history profile);
	// "default" or "shared" means B's regular config dir.
	Account string
	SSH         string // ssh binary, default "ssh"
}

type session struct {
	cfg    Config
	id     string
	local  string // project dir on A (native, symlinks resolved)
	remote string // same dir in B's form
	tmp    string // private temp dir on A
	ctl    string // ControlPath, empty when multiplexing is unavailable
	logf   func(string, ...any)

	facts map[string]string
	mode  string
	run   string // $HOME/.cache/rcc/run/<id> on B
	cache string
	sock  string // command socket on B
	prof  string // per-A history dir on B, "" = shared
	acct  string // account config dir on B, "" = shared default
	autoAccount bool // acct is this machine's own default account
	cfg0  string // B's default config dir
	ccfg  string // config dir claude will use (acct or cfg0)

	mu      sync.Mutex
	stopped bool        // SIGTERM/SIGHUP received
	runProc *os.Process // interactive ssh, killed on stop
}

func Run(cfg Config) (int, error) {
	if cfg.SSH == "" {
		cfg.SSH = "ssh"
	}
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	s := &session{cfg: cfg, id: randHex(6)}
	s.logf = func(f string, a ...any) {
		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "rcc: "+f+"\n", a...)
		}
	}

	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return 1, err
	}
	// Resolve symlinks: Claude tracks cwd with `pwd -P`, which must stay
	// inside the mounted path.
	if s.local, err = filepath.EvalSymlinks(abs); err != nil {
		return 1, err
	}
	s.remote = pathmap.ToRemote(runtime.GOOS, s.local)

	if s.tmp, err = os.MkdirTemp("", "rcc-"); err != nil {
		return 1, err
	}
	defer os.RemoveAll(s.tmp)
	procjob.RemoveDirOnExit(s.tmp)
	if runtime.GOOS != "windows" {
		// Windows OpenSSH has no connection multiplexing.
		s.ctl = filepath.Join(s.tmp, "c")
	}
	defer s.closeMaster()

	// On SIGTERM/SIGHUP (terminal closed, kill), end the interactive ssh and
	// fall through to the normal teardown instead of leaving remote state.
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

	if err := s.probe(); err != nil {
		return 1, err
	}
	return s.start()
}

// sshCmd builds an ssh command line. opts go before the destination.
func (s *session) sshCmd(tty bool, opts []string, script string) *exec.Cmd {
	args := append([]string{}, s.cfg.SSHArgs...)
	if s.ctl != "" {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+s.ctl, "-o", "ControlPersist=30")
	}
	args = append(args, "-o", "ServerAliveInterval=15")
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, opts...)
	args = append(args, s.cfg.Destination, remoteCommand(script))
	return exec.Command(s.cfg.SSH, args...)
}

func (s *session) closeMaster() {
	if s.ctl == "" {
		return
	}
	if _, err := os.Stat(s.ctl); err != nil {
		return
	}
	args := append(append([]string{}, s.cfg.SSHArgs...), "-o", "ControlPath="+s.ctl, "-O", "exit", s.cfg.Destination)
	c := exec.Command(s.cfg.SSH, args...)
	c.Run()
}

// fetchFacts asks B for the key=value facts printed by probeScript.
func (s *session) fetchFacts() error {
	c := s.sshCmd(false, nil, probeScript)
	c.Stdin = nil
	c.Stderr = os.Stderr // auth prompts / host key questions go to the user
	out, err := c.Output()
	if err != nil {
		return fmt.Errorf("ssh %s failed: %w", s.cfg.Destination, err)
	}
	s.facts = map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			s.facts[k] = v
		}
	}
	if s.cfg.ClaudePath != "" {
		s.facts["claude"] = s.cfg.ClaudePath
	}
	f := s.facts
	s.logf("remote: %s/%s uid=%s claude=%s", f["os"], f["arch"], f["uid"], f["claude"])
	return nil
}

func (s *session) probe() (err error) {
	if err := s.fetchFacts(); err != nil {
		return err
	}
	f := s.facts
	var problems []string
	if f["os"] != "Linux" {
		problems = append(problems, fmt.Sprintf("remote OS is %q; only Linux is supported as the Claude Code host", f["os"]))
	}
	if f["bin_sshfs"] == "" {
		problems = append(problems, "sshfs is not installed on the remote machine (e.g. `sudo apt install sshfs`)")
	}
	if f["fuse"] == "" {
		problems = append(problems, "/dev/fuse is missing on the remote machine")
	}
	if f["bin_base64"] == "" || f["bin_mountpoint"] == "" {
		problems = append(problems, "base64 and mountpoint (coreutils/util-linux) are required on the remote machine")
	}
	if f["claude"] == "" {
		problems = append(problems, s.claudeMissing().Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n     "))
	}

	nsOK := f["uid"] == "0" && f["bin_unshare"] != "" && f["bin_nsenter"] != ""
	switch s.cfg.Mode {
	case "auto":
		s.mode = map[bool]string{true: "ns", false: "global"}[nsOK]
	case "ns":
		if !nsOK {
			return errors.New("--mode ns needs root plus unshare/nsenter on the remote machine")
		}
		s.mode = "ns"
	case "global":
		s.mode = "global"
	default:
		return fmt.Errorf("unknown mode %q", s.cfg.Mode)
	}

	if err := s.resolveAccount(); err != nil {
		return err
	}

	profile := s.cfg.Profile
	if profile == "" {
		if profile, err = MachineProfile(); err != nil {
			return err
		}
	}
	if profile != "none" {
		if !validProfile(profile) {
			return fmt.Errorf("invalid profile name %q (use letters, digits, '.', '_', '-')", profile)
		}
		// History of a named account is kept apart from this machine's own
		// (the machine's own account uses the plain profile, as before).
		if s.acct != "" && !s.autoAccount {
			profile += "@" + s.cfg.Account
		}
		if s.mode == "ns" {
			s.prof = f["home"] + "/.local/share/rcc/profiles/" + profile
		} else {
			fmt.Fprintf(os.Stderr, "rcc: warning: per-machine conversation history needs root on the remote machine (mount namespace); "+
				"this session uses the remote's shared ~/.claude history\n")
		}
	}
	s.logf("history profile: %s", map[bool]string{true: s.prof, false: "shared (" + f["cfgdir"] + ")"}[s.prof != ""])

	s.cache = f["home"] + "/.cache/rcc"
	s.run = s.cache + "/run/" + s.id
	s.sock = s.run + "/s"
	return nil
}

// shimBinary picks the rcc-shim build for B's architecture.
func (s *session) shimBinary() ([]byte, error) {
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[s.facts["arch"]]
	if goarch == "" {
		return nil, fmt.Errorf("unsupported remote architecture %q", s.facts["arch"])
	}
	if b := embedded.Shim(goarch); b != nil {
		return b, nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == goarch {
		// Same OS/arch: upload ourselves. The file is installed as rcc-shim,
		// and rcc dispatches to shim mode on that argv[0].
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return os.ReadFile(exe)
	}
	return nil, fmt.Errorf("this rcc build has no rcc-shim for linux/%s; build with scripts/build.sh", goarch)
}

func (s *session) start() (int, error) {
	token := randHex(16)

	// --- install shim + token
	shimBin, err := s.shimBinary()
	if err != nil {
		return 1, err
	}
	sum := sha256.Sum256(shimBin)
	shimSHA := hex.EncodeToString(sum[:])
	need := !strings.Contains(","+s.facts["shims"], ","+shimSHA+",")
	s.logf("mode=%s mount=%s shim upload=%v", s.mode, s.remote, need)

	common := vars("RUN", s.run, "CACHE", s.cache, "SOCK", s.sock, "R", s.remote, "MODE", s.mode,
		"PROF", s.prof, "CFG", s.ccfg, "ACCT_DIR", s.acct, "SHIM_SHA", shimSHA)
	// Registered before prepare, so a failed prepare leaves nothing behind.
	defer func() {
		c := s.sshCmd(false, nil, common+cleanupScript)
		c.Run()
	}()

	prep := vars("RUN", s.run, "CACHE", s.cache, "NEED_SHIM", map[bool]string{true: "1", false: "0"}[need],
		"SHIM_SHA", shimSHA, "ACCT_DIR", s.acct, "DEFCFG", s.cfg0) + prepareScript
	c := s.sshCmd(false, nil, prep)
	in := io.MultiReader(strings.NewReader(token+"\n"), bytes.NewReader(map[bool][]byte{true: shimBin}[need]))
	c.Stdin, c.Stderr = in, os.Stderr
	if err := c.Run(); err != nil {
		return 1, fmt.Errorf("preparing remote machine: %w", err)
	}

	// --- command agent
	logFile := s.openLog()
	defer logFile.Close()
	ag, err := agent.New(agent.Options{Token: token, Logger: log.New(logFile, "", log.LstdFlags), TempDir: s.tmp})
	if err != nil {
		return 1, err
	}
	claudeArgs := s.cfg.ClaudeArgs
	if !s.cfg.NoPrompt {
		var added bool
		claudeArgs, added = withSystemPrompt(claudeArgs, systemPrompt(s.remote, ag.Shell()))
		s.logf("system prompt appended: %v", added)
	}
	var l net.Listener
	var target string
	if runtime.GOOS == "windows" {
		l, err = net.Listen("tcp", "127.0.0.1:0")
		target = l.Addr().String()
	} else {
		target = filepath.Join(s.tmp, "a.sock")
		l, err = net.Listen("unix", target)
	}
	if err != nil {
		return 1, err
	}
	defer l.Close()
	go ag.Serve(l)

	// --- file channel (+ reverse forward of the command socket)
	fsCmd := s.sshCmd(false, []string{"-o", "ExitOnForwardFailure=yes", "-R", s.sock + ":" + target}, common+mountScript)
	fsIn, _ := fsCmd.StdinPipe()
	fsOut, _ := fsCmd.StdoutPipe()
	var fsErr bytes.Buffer
	fsCmd.Stderr = io.MultiWriter(&fsErr, logFile)
	if err := fsCmd.Start(); err != nil {
		return 1, err
	}
	procjob.TrackProcess(fsCmd.Process.Pid)
	ready := make(chan struct{})
	fsDone := make(chan error, 1)
	go func() {
		fsDone <- fsserver.Serve(rwc{&firstRead{r: fsOut, ready: ready}, fsIn}, s.remote)
	}()
	fsExited := make(chan struct{})
	go func() { fsCmd.Wait(); close(fsExited) }()

	select {
	case <-ready:
	case <-fsExited:
		return 1, fmt.Errorf("file mount on the remote machine failed:\n%s", strings.TrimSpace(fsErr.String()))
	case <-time.After(60 * time.Second):
		return 1, errors.New("timed out waiting for sshfs on the remote machine")
	}

	// --- Claude Code, interactive
	sandbox := "0"
	if s.facts["uid"] == "0" && !s.cfg.NoIsSandbox {
		sandbox = "1"
	}
	runCmd := s.sshCmd(true, nil, common+vars("CLAUDE", s.facts["claude"], "SANDBOX", sandbox)+setArgs(claudeArgs)+runScript)
	runCmd.Stdin, runCmd.Stdout, runCmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The tty is in raw mode while ssh runs; keep stray signals from killing
	// rcc before it can clean up.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return 1, errors.New("interrupted")
	}
	err = runCmd.Start()
	if err == nil {
		s.runProc = runCmd.Process
		// TERM lets ssh restore the terminal from raw mode if rcc dies.
		procjob.TrackProcess(runCmd.Process.Pid)
	}
	s.mu.Unlock()
	if err != nil {
		return 1, err
	}
	code := 0
	if err := runCmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return 1, err
		}
	}
	select {
	case <-fsExited:
		if code != 0 && fsErr.Len() > 0 {
			fmt.Fprintf(os.Stderr, "rcc: file channel output:\n%s\n", strings.TrimSpace(fsErr.String()))
		}
	default:
	}

	// --- teardown: closing sshfs' stdin makes it exit and unmount.
	fsIn.Close()
	select {
	case <-fsExited:
	case <-time.After(3 * time.Second):
		fsCmd.Process.Kill()
	}
	return code, nil
}

func (s *session) openLog() *os.File {
	dir, err := os.UserCacheDir()
	if err == nil {
		dir = filepath.Join(dir, "rcc")
		if os.MkdirAll(dir, 0o700) == nil {
			if f, err := os.Create(filepath.Join(dir, "last.log")); err == nil {
				return f
			}
		}
	}
	f, _ := os.Create(filepath.Join(s.tmp, "log"))
	return f
}

func setArgs(args []string) string {
	if len(args) == 0 {
		return "set --\n"
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shQuote(a)
	}
	return "set -- " + strings.Join(q, " ") + "\n"
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

func isWindows() bool { return runtime.GOOS == "windows" }

// Prog is the name rcc was invoked as (e.g. "remote-cc"), used in hints so
// they can be pasted as-is; "rcc" may be another program on the user's PATH.
var Prog = "rcc"

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
