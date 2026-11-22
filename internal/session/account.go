package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"remote-cc/internal/procjob"
)

// newBareSession connects to B for a command that needs no mount or agent.
func newBareSession(cfg Config) (*session, func(), error) {
	s := &session{cfg: cfg, id: randHex(6)}
	s.logf = func(f string, a ...any) {
		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "rcc: "+f+"\n", a...)
		}
	}
	var err error
	if s.tmp, err = os.MkdirTemp("", "rcc-"); err != nil {
		return nil, nil, err
	}
	procjob.RemoveDirOnExit(s.tmp)
	if runtime.GOOS != "windows" {
		s.ctl = filepath.Join(s.tmp, "c")
	}
	cleanup := func() { s.closeMaster(); os.RemoveAll(s.tmp) }
	if err := s.fetchFacts(); err != nil {
		cleanup()
		return nil, nil, err
	}
	if s.facts["claude"] == "" {
		cleanup()
		return nil, nil, s.claudeMissing()
	}
	if s.facts["bin_base64"] == "" {
		cleanup()
		return nil, nil, errors.New("base64 is required on the remote machine")
	}
	return s, cleanup, nil
}

// claudeMissing explains a missing claude, spotting the common mistake of
// connecting to the Docker host instead of the remote-cc container.
func (s *session) claudeMissing() error {
	if s.facts["rccserver"] == "1" {
		return errors.New("claude is missing inside the remote-cc container; update it with rcc setup-server, or pass --claude PATH")
	}
	return fmt.Errorf("claude was not found on %s (host %q).\n"+
		"     If the server was deployed with `rcc setup-server`, this is the Docker host, not the remote-cc container:\n"+
		"     connect to the container's port instead (default 2222), e.g. in ~/.ssh/config:\n"+
		"       Host cc\n         HostName <server address>\n         Port 2222\n         User root\n"+
		"     Otherwise install Claude Code there or pass --claude PATH", s.cfg.Destination, s.facts["hostname"])
}

func (s *session) resolveAccount() error {
	s.cfg0 = s.facts["cfgdir"]
	s.ccfg = s.cfg0
	a := s.cfg.Account
	if a == "" || a == "default" {
		s.cfg.Account = ""
		return nil
	}
	if !validProfile(a) {
		return fmt.Errorf("invalid account name %q (use letters, digits, '.', '_', '-')", a)
	}
	s.acct = s.accountsDir() + "/" + a
	s.ccfg = s.acct
	s.logf("account: %s (%s)", a, s.acct)
	return nil
}

func (s *session) accountsDir() string { return s.facts["home"] + "/.local/share/rcc/accounts" }

// Command runs an account-management action on B without mounting anything:
//
//	login, logout, status   claude auth <action>
//	setup-token             claude setup-token
//	token                   store a setup-token for the account (read here)
//	token-clear             remove the stored token
//	accounts                list accounts and their login state
func Command(cfg Config, action string, args []string) (int, error) {
	if cfg.SSH == "" {
		cfg.SSH = "ssh"
	}
	s, cleanup, err := newBareSession(cfg)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	if err := s.resolveAccount(); err != nil {
		return 1, err
	}
	if (action == "token" || action == "token-clear") && s.acct == "" {
		return 1, errors.New("a stored token needs a named account: use --account NAME (or RCC_ACCOUNT)")
	}
	base := vars("ACCT_DIR", s.acct, "DEFCFG", s.cfg0, "CLAUDE", s.facts["claude"])

	switch action {
	case "login", "logout", "status":
		return s.interactive(base+setArgs(append([]string{"auth", action}, args...))+authScript, action == "login")
	case "setup-token":
		code, err := s.interactive(base+setArgs(append([]string{"setup-token"}, args...))+authScript, true)
		if err == nil && code == 0 && s.acct != "" {
			fmt.Fprintf(os.Stderr, "\nrcc: to use this token for account %q, run: rcc token --account %s %s\n", s.cfg.Account, s.cfg.Account, s.cfg.Destination)
		}
		return code, err
	case "token":
		tok, err := readSecret("Paste the token from `setup-token` (input hidden): ")
		if err != nil {
			return 1, err
		}
		c := s.sshCmd(false, nil, base+vars("ACTION", "set")+tokenScript)
		c.Stdin = strings.NewReader(tok + "\n")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return exitCode(c.Run())
	case "token-clear":
		c := s.sshCmd(false, nil, base+vars("ACTION", "clear")+tokenScript)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return exitCode(c.Run())
	case "accounts":
		return s.listAccounts(base)
	}
	return 2, fmt.Errorf("unknown action %q", action)
}

func (s *session) interactive(script string, tty bool) (int, error) {
	c := s.sshCmd(tty, nil, script)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(c.Run())
}

func (s *session) listAccounts(base string) (int, error) {
	c := s.sshCmd(false, nil, base+vars("ACCTS", s.accountsDir())+accountsScript)
	c.Stderr = os.Stderr
	out, err := c.Output()
	if err != nil {
		return exitCode(err)
	}
	type status struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		Email            string `json:"email"`
		OrgName          string `json:"orgName"`
		SubscriptionType string `json:"subscriptionType"`
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tLOGGED IN\tMETHOD\tEMAIL\tORG\tPLAN\tSTORED TOKEN")
	var name string
	var js bytes.Buffer
	var row []string
	flush := func() {
		if name == "" {
			return
		}
		var st status
		json.Unmarshal(js.Bytes(), &st)
		row = []string{name, map[bool]string{true: "yes", false: "no"}[st.LoggedIn], dash(st.AuthMethod), dash(st.Email), dash(st.OrgName), dash(st.SubscriptionType)}
		js.Reset()
	}
	emit := func(tok string) {
		if row != nil {
			fmt.Fprintln(tw, strings.Join(append(row, tok), "\t"))
			row = nil
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "@@token"):
			flush()
			emit(map[bool]string{true: "yes", false: "no"}[strings.TrimSpace(strings.TrimPrefix(l, "@@token")) == "1"])
			name = ""
		case strings.HasPrefix(l, "@@ "):
			flush()
			emit("-")
			name = strings.Fields(l)[1]
		default:
			js.WriteString(l + "\n")
		}
	}
	flush()
	emit("-")
	tw.Flush()
	return 0, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return validToken(string(b))
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	if err != nil {
		return "", err
	}
	return validToken(string(b))
}

// readPassword reads a password from the terminal (hidden), or one line
// from stdin when it is not a terminal.
func readPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("sudo needs a password, but none was given")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func validToken(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" || strings.ContainsAny(t, " \t\r\n'\"") {
		return "", errors.New("that does not look like a token")
	}
	return t, nil
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 1, err
}
