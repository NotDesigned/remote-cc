// Package remote runs scripts on servers over the system ssh client: one
// multiplexed connection where available (not on Windows), scripts wrapped so
// any login shell runs them, and sudo elevation for host scripts.
package remote

import (
	"bufio"
	"bytes"
	"crypto/rand"
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
		c.ctl = filepath.Join(tmp, "c")
	}
	return c, nil
}

// Tmp is a private directory removed by Close.
func (c *Conn) Tmp() string { return c.tmp }

func (c *Conn) Close() {
	if c.ctl != "" {
		if _, err := os.Stat(c.ctl); err == nil {
			args := append(append([]string{}, c.T.Args...), "-o", "ControlPath="+c.ctl, "-O", "exit", c.T.Dest)
			exec.Command(c.T.SSH, args...).Run()
		}
	}
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
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+c.ctl, "-o", "ControlPersist=30")
	}
	args = append(args, "-o", "ServerAliveInterval=15")
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

// Hint explains ssh errors that have a known remote-cc cause.
func Hint(sshErr string) string {
	if strings.Contains(sshErr, "stdio forwarding failed") || strings.Contains(sshErr, "open failed: connect failed") {
		return "\n     The server's gateway could not reach your container: it is stopped or missing." +
			"\n     The server's admin can start it: " + Prog + " update-server <docker host>" +
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
	inner := params + body
	run := func(password string) ([]byte, error) {
		cmd := c.Script(false, nil, params+Vars("SELF_B64", Base64(inner), "HAVE_PASS", boolStr(password != ""))+body)
		cmd.Stderr = os.Stderr
		if password != "" {
			cmd.Stdin = strings.NewReader(password + "\n")
		}
		return cmd.Output()
	}
	out, err := run("")
	if bytes.Contains(out, []byte("rcc-needs-sudo-password")) {
		pw, perr := ReadPassword(fmt.Sprintf("[sudo] password for %s: ", c.T.Dest))
		if perr != nil {
			return nil, perr
		}
		out, err = run(pw)
	}
	return out, err
}

// wrap makes a script runnable by any login shell (bash, zsh, fish, …).
func wrap(script string) string {
	return `sh -c 'eval "$(printf %s ` + Base64(script) + ` | base64 -d)"'`
}

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
