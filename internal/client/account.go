package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"remote-cc/internal/rcb"
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
	// --rcb is its profile's account, whatever else asks for one.
	if p := s.cfg.RCB; p != nil {
		if a := s.cfg.Account; a != "" && a != p.Account() {
			return fmt.Errorf("--rcb %s uses account %s; drop --account %s", p.ID, p.Account(), a)
		}
		s.cfg.Account = p.Account()
	}
	a := s.cfg.Account
	// A profile's account exists to be used through its node only.
	if strings.HasPrefix(a, "rcb-") && s.cfg.RCB == nil {
		return fmt.Errorf("account %s belongs to a remote-claude-browser profile; use --rcb %s instead of --account", a, strings.TrimPrefix(a, "rcb-"))
	}
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
	// With --rcb, claude commands run through the profile's node (rcbRun);
	// the others only touch the account's files.
	p := s.cfg.RCB
	if action == "accounts" {
		p = nil
	}
	var shim string
	if p != nil && action != "token" && action != "token-clear" {
		if s.cfg.RCBLink == nil {
			return 1, errors.New("--rcb needs this machine linked to remote-claude-browser")
		}
		if shim, err = s.installShim(); err != nil {
			return 1, err
		}
	}
	// claude runs one claude command for the account: through the profile's
	// node with --rcb (fresh grant, exit re-check), else directly.
	claude := func(args []string, run func(script string) (int, error)) (int, error) {
		script := remote.SetArgs(args) + scripts.Client("client/auth.sh")
		if p == nil {
			return run(base + script)
		}
		return s.rcbRun(func(grant string) (int, error) {
			return run(base + remote.Vars("SHIM", shim, "RCB_GRANT", grant) + rcbVars(p, false) + script)
		})
	}
	interactive := func(script string) (int, error) {
		c := s.conn.Script(action == "login" || action == "setup-token", nil, script)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return remote.ExitCode(c.Run())
	}
	switch action {
	case "login", "logout", "status":
		run := interactive
		if p != nil && action == "login" {
			run = s.rcbLogin
		}
		code, err := claude(append([]string{"auth", action}, args...), run)
		if err == nil && code == 0 && p != nil && action == "login" && p.Email != "" {
			s.checkEmail(base, shim, p.Email)
		}
		return code, err
	case "setup-token":
		code, err := claude(append([]string{"setup-token"}, args...), interactive)
		if err == nil && code == 0 && s.acct != "" {
			hint := "--account " + s.cfg.Account
			if p != nil {
				hint = "--rcb " + p.ID // rcb-* accounts are only reachable through --rcb
			}
			fmt.Fprintf(os.Stderr, "\nrcc: to use this token for this account, run: %s account token %s %s\n", remote.Prog, hint, s.cfg.Dest)
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
		if parts[2] == "rcb" {
			fmt.Fprintln(tw, strings.Join([]string{parts[0], "(use --rcb)", "-", "-", "-", "-", dash(parts[1])}, "\t"))
			continue
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
		fmt.Fprint(os.Stderr, "Paste the token from `account setup-token` (input hidden): ")
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

// installShim puts this rcc's shim on the server (account commands need it
// for --rcb; sessions install it in prepare) and returns its path.
func (s *session) installShim() (string, error) {
	shim, err := s.shimBinary()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(shim)
	sha := hex.EncodeToString(sum[:])
	cache := s.facts["home"] + "/.cache/rcc"
	need := !strings.Contains(","+s.facts["shims"], ","+sha+",")
	c := s.conn.Script(false, nil, remote.Vars("CACHE", cache, "SHIM_SHA", sha, "NEED_SHIM", remote.Bool(need))+scripts.Get("client/shim.sh"))
	if need {
		c.Stdin = bytes.NewReader(shim)
	}
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("installing rcc-shim on the server: %w", err)
	}
	return cache + "/shims/" + sha + "/rcc-shim", nil
}

// checkEmail warns when the new login is not the profile's Claude account.
// The sign-in may have taken longer than the login's grant lasts, so the
// check gets a fresh one.
func (s *session) checkEmail(base, shim, want string) {
	unchecked := func(why any) {
		fmt.Fprintf(os.Stderr, "rcc: could not check which Claude account is logged in (%v); sessions check it when they start.\n", why)
	}
	grant, err := rcb.Grant(*s.cfg.RCBLink, s.cfg.RCB.ID)
	if err != nil {
		unchecked(err)
		return
	}
	c := s.conn.Script(false, nil, base+remote.Vars("SHIM", shim, "RCB_GRANT", grant)+rcbVars(s.cfg.RCB, false)+
		remote.SetArgs([]string{"auth", "status", "--json"})+scripts.Client("client/auth.sh"))
	out, err := c.Output()
	if err != nil {
		unchecked(err)
		return
	}
	email, ok := statusEmail(out)
	switch {
	case !ok:
		unchecked("no account status from Claude Code")
	case email == "":
		fmt.Fprintln(os.Stderr, "rcc: Claude Code reports no account email; not compared with the profile's.")
	case !strings.EqualFold(email, want):
		fmt.Fprintf(os.Stderr, "\nrcc: warning: Claude Code is now logged in as %s, but the profile's browser is signed in as %s.\n     Sessions will refuse to start; log out and log in again with the right account.\n", email, want)
	default:
		fmt.Fprintf(os.Stderr, "rcc: logged in as %s, matching the profile.\n", email)
	}
}

// statusEmail reads the email from `claude auth status --json` output (after
// any lines the node check printed); ok is false if there is no status.
func statusEmail(out []byte) (email string, ok bool) {
	i := bytes.IndexByte(out, '{')
	if i < 0 {
		return "", false
	}
	var st struct{ Email string }
	if json.NewDecoder(bytes.NewReader(out[i:])).Decode(&st) != nil {
		return "", false
	}
	return st.Email, true
}
