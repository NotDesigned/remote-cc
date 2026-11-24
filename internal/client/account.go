package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"remote-cc/internal/remote"
	"remote-cc/internal/scripts"
)

func (s *session) accountsDir() string { return s.facts["home"] + "/.local/share/rcc/accounts" }

// resolveAccount picks the Claude config dir: this machine's own account by
// default (machines never share Claude credentials unless asked to),
// "default"/"shared" for the container's regular one, or a named account.
func (s *session) resolveAccount() error {
	s.cfg0 = s.facts["cfgdir"]
	s.ccfg = s.cfg0
	a := s.cfg.Account
	switch a {
	case "default", "shared":
		s.cfg.Account = ""
		s.conn.Logf("account: default (%s)", s.cfg0)
		return nil
	case "":
		p, err := MachineProfile()
		if err != nil {
			return err
		}
		a, s.cfg.Account, s.autoAccount = p, p, true
	}
	if !validProfile(a) {
		return fmt.Errorf("invalid account name %q (use letters, digits, '.', '_', '-')", a)
	}
	s.acct = s.accountsDir() + "/" + a
	s.ccfg = s.acct
	s.conn.Logf("account: %s (%s)", a, s.acct)
	return nil
}

// Account runs an account command on the user's container:
//
//	login, logout, status, setup-token   claude auth … / claude setup-token
//	token        store a setup-token for the account (read here, hidden)
//	token-clear  remove it
//	accounts     list the accounts and their login state
func Account(cfg Config, action string, args []string) (int, error) {
	s, err := newSession(cfg)
	if err != nil {
		return 1, err
	}
	defer s.conn.Close()
	if (action == "token" || action == "token-clear") && s.acct == "" {
		return 1, errors.New("a stored token needs an account of its own: drop --account default, or name one with --account NAME")
	}
	base := remote.Vars("ACCT_DIR", s.acct, "DEFCFG", s.cfg0, "CLAUDE", s.facts["claude"])
	interactive := func(script string) (int, error) {
		c := s.conn.Script(action == "login" || action == "setup-token", nil, script)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return remote.ExitCode(c.Run())
	}
	switch action {
	case "login", "logout", "status":
		return interactive(base + remote.SetArgs(append([]string{"auth", action}, args...)) + scripts.Client("client/auth.sh"))
	case "setup-token":
		code, err := interactive(base + remote.SetArgs(append([]string{"setup-token"}, args...)) + scripts.Client("client/auth.sh"))
		if err == nil && code == 0 && s.acct != "" {
			fmt.Fprintf(os.Stderr, "\nrcc: to use this token for this account, run: %s token --account %s %s\n", remote.Prog, s.cfg.Account, s.cfg.Dest)
		}
		return code, err
	case "token", "token-clear":
		c := s.conn.Script(false, nil, base+remote.Vars("ACTION", map[bool]string{true: "set", false: "clear"}[action == "token"])+scripts.Client("client/token.sh"))
		if action == "token" {
			tok, err := readToken()
			if err != nil {
				return 1, err
			}
			c.Stdin = strings.NewReader(tok + "\n")
		}
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return remote.ExitCode(c.Run())
	case "accounts":
		return s.listAccounts(base)
	}
	return 2, fmt.Errorf("unknown action %q", action)
}

func (s *session) listAccounts(base string) (int, error) {
	c := s.conn.Script(false, nil, base+remote.Vars("ACCTS", s.accountsDir())+scripts.Get("client/accounts.sh"))
	c.Stderr = os.Stderr
	out, err := c.Output()
	if err != nil {
		return remote.ExitCode(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tLOGGED IN\tMETHOD\tEMAIL\tORG\tPLAN\tSTORED TOKEN")
	for _, l := range strings.Split(string(out), "\n") {
		v, ok := strings.CutPrefix(l, "acct=")
		if !ok {
			continue
		}
		parts := strings.SplitN(v, "|", 3)
		if len(parts) < 3 {
			continue
		}
		var st struct {
			LoggedIn                                     bool
			AuthMethod, Email, OrgName, SubscriptionType string
		}
		json.Unmarshal([]byte(parts[2]), &st)
		fmt.Fprintln(tw, strings.Join([]string{parts[0], yesNo(st.LoggedIn), dash(st.AuthMethod), dash(st.Email),
			dash(st.OrgName), dash(st.SubscriptionType), dash(parts[1])}, "\t"))
	}
	tw.Flush()
	return 0, nil
}

func readToken() (string, error) {
	var b []byte
	var err error
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Paste the token from `setup-token` (input hidden): ")
		b, err = term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
	} else {
		b, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	}
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" || strings.ContainsAny(t, " \t\r\n'\"") {
		return "", errors.New("that does not look like a token")
	}
	return t, nil
}

func yesNo(b bool) string { return map[bool]string{true: "yes", false: "no"}[b] }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
