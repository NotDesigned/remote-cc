// Package remote runs scripts on servers over the system ssh client: one
// multiplexed connection where available (not on Windows), scripts wrapped so
// any login shell runs them, and sudo elevation for host scripts.
package remote

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"remote-cc/internal/procjob"
)

// Prog is the name rcc was invoked as (e.g. "remote-cc"), used in hints so
// they can be pasted as-is; "rcc" may be another program on the user's PATH.
var Prog = "rcc"

// Target is an ssh destination plus options.
type Target struct {
	SSH     string   // ssh binary, default "ssh"
	Args    []string // ssh options placed before the destination
	Dest    string   // [user@]host or an ssh config alias
	Verbose bool
}

// Conn is a (possibly multiplexed) series of ssh invocations to one Target.
type Conn struct {
	T   Target
	tmp string // private temp dir; also holds the control socket
	ctl string // ControlPath, "" without multiplexing
}

// Open prepares a Conn. Close it to end the multiplexed master connection.
func Open(t Target) (*Conn, error) {
	if t.SSH == "" {
		t.SSH = "ssh"
	}
	tmp, err := os.MkdirTemp("", "rcc-")
	if err != nil {
		return nil, err
	}
	procjob.RemoveDirOnExit(tmp)
	c := &Conn{T: t, tmp: tmp}
	if runtime.GOOS != "windows" { // Windows OpenSSH has no multiplexing
		c.ctl = controlPath(t)
		if c.ctl == "" { // no usable shared path: this run's own connection
			c.ctl = filepath.Join(tmp, "c")
		}
	}
	return c, nil
}

// MasterPersist is how long the shared ssh connection stays up after its
// last use, so the next rcc (session or command) skips the ssh handshake.
const MasterPersist = 10 * time.Minute

// controlPath is the shared connection's socket for t, in a private
// directory under the user cache. A socket left by a master that died is
// removed first: ssh would otherwise not multiplex at all.
func controlPath(t Target) string {
	p := controlPathName(t)
	if p == "" {
		return ""
	}
	dir := filepath.Dir(p)
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	os.Chmod(dir, 0o700)
	if _, err := os.Stat(p); err == nil {
		args := append(append([]string{}, t.Args...), "-o", "ControlPath="+p, "-O", "check", t.Dest)
		if exec.Command(t.SSH, args...).Run() != nil {
			os.Remove(p)
		}
	}
	return p
}

// StopMaster ends the shared connection to t, if one is up. A connection
// made before t's ssh config changed (device join again, server setup) or
// before its key was removed (device remove) would otherwise go on being
// used, with the old settings and the old authentication, for up to
// MasterPersist.
func StopMaster(t Target) {
	if runtime.GOOS == "windows" {
		return
	}
	if t.SSH == "" {
		t.SSH = "ssh"
	}
	p := controlPathName(t)
	if p == "" {
		return
	}
	if _, err := os.Stat(p); err != nil {
		return
	}
	args := append(append([]string{}, t.Args...), "-o", "ControlPath="+p, "-O", "exit", t.Dest)
	exec.Command(t.SSH, args...).Run()
	os.Remove(p)
}

// controlPathName is where t's shared connection's socket goes ("" if no
// usable path).
func controlPathName(t Target) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(append([]string{t.SSH, t.Dest}, t.Args...), "\x00")))
	p := filepath.Join(dir, "rcc", "cm", hex.EncodeToString(sum[:8]))
	if len(p) > 100 { // unix socket paths are limited to 104-108 bytes
		return ""
	}
	return p
}

// Tmp is a private directory removed by Close.
func (c *Conn) Tmp() string { return c.tmp }

// Close removes the private temp directory. The shared connection stays up
// for MasterPersist.
func (c *Conn) Close() {
	os.RemoveAll(c.tmp)
}

// Logf prints when verbose.
func (c *Conn) Logf(f string, a ...any) {
	if c.T.Verbose {
		fmt.Fprintf(os.Stderr, "rcc: "+f+"\n", a...)
	}
}

func (c *Conn) args(tty bool, opts []string) []string {
	args := append([]string{}, c.T.Args...)
	if c.ctl != "" {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+c.ctl,
			"-o", fmt.Sprintf("ControlPersist=%d", int(MasterPersist.Seconds())))
	}
	// A dropped network is noticed within 15s (sessions also have their own
	// heartbeat, see internal/resume).
	args = append(args, "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3")
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	return append(args, opts...)
}

// Script returns an ssh command that runs a POSIX sh script on the server.
func (c *Conn) Script(tty bool, opts []string, script string) *exec.Cmd {
	return exec.Command(c.T.SSH, append(c.args(tty, opts), c.T.Dest, wrap(script))...)
}

// Raw returns an ssh command that sends command as is (for forced commands).
func (c *Conn) Raw(command string) *exec.Cmd {
	return exec.Command(c.T.SSH, append(c.args(false, nil), c.T.Dest, command)...)
}

// Facts runs a script and parses its key=value output.
func (c *Conn) Facts(script string) (map[string]string, error) {
	cmd := c.Script(false, nil, script)
	var errBuf bytes.Buffer
	// Auth prompts and host key questions reach the user; a copy is kept to
	// explain known failures.
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh %s failed: %w%s", c.T.Dest, err, Hint(errBuf.String()))
	}
	return ParseFacts(out), nil
}

// factsEnd ends the facts of a held probe (FactsHeld).
const factsEnd = "rcc-facts-end"

// heldLoader runs the script sent as one base64 line on stdin (gzipped
// after a "z"), once the facts are out; the rest of stdin is the script's
// (sh's read takes no more than that line). No line (rcc gave up): nothing
// more runs.
const heldLoader = `IFS= read -r rcc_s || exit 0
case $rcc_s in
z*) eval "$(printf %s "${rcc_s#z}" | base64 -d | gzip -dc)" ;;
*) eval "$(printf %s "$rcc_s" | base64 -d)" ;;
esac`

// FactsHeld is Facts on an ssh session that then waits for the next script
// (Held.Start). The script that follows the facts, such as a session start,
// skips a new ssh channel (two round trips) or, without multiplexing
// (Windows), a new ssh connection.
func (c *Conn) FactsHeld(script string) (map[string]string, *Held, error) {
	cmd := c.Script(false, nil, script+"\necho "+factsEnd+"\n"+heldLoader)
	var errBuf bytes.Buffer
	sw := &switchWriter{w: io.MultiWriter(os.Stderr, &errBuf)}
	cmd.Stderr = sw
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	procjob.TrackProcess(cmd.Process.Pid)
	h := &Held{p: newPipe(cmd, bufio.NewReader(out), in), stderr: sw}
	var facts bytes.Buffer
	for {
		line, rerr := h.p.r.ReadString('\n')
		if strings.TrimRight(line, "\r\n") == factsEnd {
			break
		}
		facts.WriteString(line)
		if rerr != nil {
			h.p.Close()
			<-h.p.done
			err := h.p.err
			if err == nil {
				err = errors.New("the server closed the connection")
			}
			sw.set(io.Discard)
			return nil, nil, fmt.Errorf("ssh %s failed: %w%s", c.T.Dest, err, Hint(errBuf.String()))
		}
	}
	f := ParseFacts(facts.Bytes())
	h.gzip = f["bin_gzip"] != ""
	return f, h, nil
}

// Held is an ssh session waiting for its next script (FactsHeld).
type Held struct {
	p      *Pipe
	stderr *switchWriter
	once   sync.Once
	gzip   bool // the server has gzip (fact bin_gzip)
}

// Start runs script on the held session; its stdio is then the returned
// pipe, its stderr goes to stderr. A Held starts one script.
func (h *Held) Start(script string, stderr io.Writer) (*Pipe, error) {
	err := errors.New("remote: held session already used")
	h.once.Do(func() {
		h.stderr.set(stderr)
		line := Base64(script)
		if h.gzip && len(script) > 4<<10 { // a session start: about a third
			line = "z" + gzipBase64(script)
		}
		_, err = io.WriteString(h.p.in, line+"\n")
	})
	if err != nil {
		return nil, err
	}
	return h.p, nil
}

// Close ends the held session if no script was started.
func (h *Held) Close() {
	h.once.Do(func() { h.p.Close() })
}

// StartPipe starts cmd (an ssh command) with its stdio as the returned
// pipe; closing the pipe ends it.
func StartPipe(cmd *exec.Cmd, stderr io.Writer) (*Pipe, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	procjob.TrackProcess(cmd.Process.Pid)
	return newPipe(cmd, out, in), nil
}

// Pipe is a running command's stdout and stdin.
type Pipe struct {
	r    *bufio.Reader
	in   io.WriteCloser
	cmd  *exec.Cmd
	done chan struct{} // closed when the command has ended
	err  error         // its Wait error, once done
	once sync.Once
}

func newPipe(cmd *exec.Cmd, out io.Reader, in io.WriteCloser) *Pipe {
	br, ok := out.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(out, 64<<10)
	}
	p := &Pipe{r: br, in: in, cmd: cmd, done: make(chan struct{})}
	go func() {
		// What matters is that the command ended (a transport that
		// ended); output still unread then may be lost.
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *Pipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *Pipe) Write(b []byte) (int, error) { return p.in.Write(b) }

// Close closes stdin; the command gets a second to end before it is killed.
func (p *Pipe) Close() error {
	p.once.Do(func() {
		p.in.Close()
		// Not waited for: the resumable stream closes its transport
		// while it holds its lock.
		go func() {
			select {
			case <-p.done:
			case <-time.After(time.Second):
				p.cmd.Process.Kill()
			}
		}()
	})
	return nil
}

// DropMaster ends this Conn's shared ssh connection, if one is up. Once a
// session's connection has been lost, the shared one is most likely dead
// too, and new ssh calls through it would hang until ssh notices (up to
// 15s); without it they connect afresh (and become the new shared one).
func (c *Conn) DropMaster() {
	if c.ctl == "" {
		return
	}
	if _, err := os.Stat(c.ctl); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := append(append([]string{}, c.T.Args...), "-o", "ControlPath="+c.ctl, "-O", "exit", c.T.Dest)
	exec.CommandContext(ctx, c.T.SSH, args...).Run()
}

// switchWriter is a writer whose destination can change.
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w.Write(p)
	return len(p), nil
}

func (s *switchWriter) set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

// Hint explains ssh errors that have a known remote-cc cause.
func Hint(sshErr string) string {
	// Through the gateway (ProxyJump), a container that is down shows as the
	// forwarded connection closing before any ssh banner ("UNKNOWN port 65535").
	if strings.Contains(sshErr, "stdio forwarding failed") || strings.Contains(sshErr, "open failed: connect failed") ||
		strings.Contains(sshErr, "Connection closed by UNKNOWN port 65535") {
		return "\n     The server's gateway could not reach your container: it is probably stopped or missing." +
			"\n     The server's admin can start it: " + Prog + " server update <docker host>" +
			"\n     (or on that host: docker start rcc-<user>)"
	}
	return ""
}

// Elevated runs params+body (a scripts.Host script) as root: as is when the
// login user is root, else via sudo, asking here for a sudo password if one
// is needed (hidden, passed on stdin, never on a command line).
func (c *Conn) Elevated(params, body string) (map[string]string, error) {
	out, err := c.ElevatedOutput(params, body)
	return ParseFacts(out), err
}

// ElevatedOutput is Elevated returning the raw output.
func (c *Conn) ElevatedOutput(params, body string) ([]byte, error) {
	return c.ElevatedInput(params, body, "")
}

// ElevatedInput is ElevatedOutput with input on the script's stdin, for
// secrets that must not appear in a command line. lib/root.sh takes the
// password line itself (sudo gets it through SUDO_ASKPASS, so a wrong one
// never makes sudo read input), and the script sees just input.
func (c *Conn) ElevatedInput(params, body, input string) ([]byte, error) {
	inner := params + body
	run := func(password string) ([]byte, error) {
		// The script (it carries compose files, and itself again for
		// sudo) goes as the first stdin line, not on the command line:
		// Windows limits a command line to 32767 characters.
		script := params + Vars("SELF_B64", Base64(inner), "HAVE_PASS", boolStr(password != "")) + body
		cmd := exec.Command(c.T.SSH, append(c.args(false, nil), c.T.Dest, stdinLoader)...)
		cmd.Stderr = os.Stderr
		stdin := gzipBase64(script) + "\n"
		if password != "" {
			stdin += password + "\n"
		}
		cmd.Stdin = strings.NewReader(stdin + input)
		return cmd.Output()
	}
	out, err := run("")
	if bytes.Contains(out, []byte("rcc-needs-sudo-password")) {
		pw, perr := ReadPassword(fmt.Sprintf("[sudo] password for %s: ", c.T.Dest))
		if perr != nil {
			return nil, perr
		}
		out, err = run(pw)
		if bytes.Contains(out, []byte("rcc-wrong-sudo-password")) {
			return nil, fmt.Errorf("wrong sudo password for %s (nothing was run)", c.T.Dest)
		}
	}
	return out, err
}

// wrap makes a script runnable by any login shell (bash, zsh, fish, …).
// Long scripts (server admin scripts carry compose files) are compressed:
// Windows refuses command lines over 32767 characters.
func wrap(script string) string {
	plain := `sh -c 'eval "$(printf %s ` + Base64(script) + ` | base64 -d)"'`
	if len(plain) <= maxPlainCommand {
		return plain
	}
	return `sh -c 'eval "$(printf %s ` + gzipBase64(script) + ` | base64 -d | gzip -dc)"'`
}

// stdinLoader runs the script sent as the first stdin line (gzipBase64);
// the rest of stdin is the script's. sh's read takes no more than that line.
const stdinLoader = `sh -c 'IFS= read -r rcc_s && eval "$(printf %s "$rcc_s" | base64 -d | gzip -dc)"'`

func gzipBase64(script string) string {
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	w.Write([]byte(script))
	w.Close()
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// maxPlainCommand is the longest command sent uncompressed.
const maxPlainCommand = 16 << 10

// Vars renders shell assignments KEY='value' for a script prologue.
func Vars(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "%s=%s\n", kv[i], Quote(kv[i+1]))
	}
	return b.String()
}

// SetArgs renders `set -- 'a' 'b'` (positional parameters for a script).
func SetArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.TrimSpace("set -- "+strings.Join(q, " ")) + "\n"
}

// Quote quotes s for POSIX sh.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func Base64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Bool renders b as "1"/"0" for script parameters.
func Bool(b bool) string { return boolStr(b) }

// ParseFacts parses key=value lines (other lines are ignored).
func ParseFacts(out []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && k != "" && !strings.ContainsAny(k, " \t") {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

// Hostname asks ssh which host name a destination really uses.
func Hostname(t Target) string {
	ssh := t.SSH
	if ssh == "" {
		ssh = "ssh"
	}
	out, err := exec.Command(ssh, append(append([]string{}, t.Args...), "-G", t.Dest)...).Output()
	if err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(l, "hostname "); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	if i := strings.LastIndexByte(t.Dest, '@'); i >= 0 {
		return t.Dest[i+1:]
	}
	return t.Dest
}

// ExitCode extracts a process exit code; non-exit errors are returned.
func ExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 1, err
}

// ReadPassword reads a secret from the terminal without echo, or one line
// from stdin when it is not a terminal.
func ReadPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("a password is needed, but none was given")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// RandHex returns n random bytes as hex.
func RandHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
