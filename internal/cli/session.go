package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"remote-cc/internal/client"
	"remote-cc/internal/rcb"
)

// runSession: rcc [options] HOST [DIR] [-- CLAUDE-ARGS...].
func runSession(in *invocation) int {
	c := client.Config{ClaudePath: in.s("--claude"), Profile: in.s("--profile"), Account: in.s("--account"),
		NoPrompt: in.b("--no-prompt"), NoIsSandbox: in.b("--no-is-sandbox"), RemoteGit: in.b("--remote-git")}
	sel, pos := in.s("--rcb"), in.pos
	if len(pos) == 0 {
		var host string
		var err error
		if sel, host, err = rcbDefaults(sel); err != nil {
			return usageError(in.cmd, err)
		}
		pos = []string{host}
	}
	if err := withRCB(&c, sel); err != nil {
		return fail(err)
	}
	c.Target, c.ClaudeArgs, c.Dir = in.target, in.after, "."
	c.Target.Dest = pos[0]
	if len(pos) == 2 {
		c.Dir = pos[1]
	}
	return code(client.Run(c))
}

// runAccount is an account command: client.Account's action login, logout,
// status, setup-token, token or accounts.
func runAccount(action string) func(*invocation) int {
	return func(in *invocation) int { return account(in, action) }
}

func account(in *invocation, action string) int {
	c := client.Config{Account: in.s("--account"), ClaudePath: in.s("--claude")}
	sel, pos := in.s("--rcb"), in.pos
	if len(pos) == 0 {
		if sel == "" && c.Account != "" {
			return usageError(in.cmd, errors.New("HOST missing"))
		}
		// With --rcb, or in a directory set up with rcb use: the remembered server.
		var host string
		var err error
		if sel, host, err = rcbDefaults(sel); err != nil {
			return usageError(in.cmd, err)
		}
		pos = []string{host}
	}
	if err := withRCB(&c, sel); err != nil {
		return fail(err)
	}
	after := in.after
	// `token HOST -- --clear` is the old spelling of --clear.
	if action == "token" && (in.b("--clear") || (len(after) > 0 && after[0] == "--clear")) {
		action = "token-clear"
	}
	c.Target = in.target
	c.Target.Dest = pos[0]
	return code(client.Account(c, action, after))
}

// withRCB resolves --rcb (if any) into c and settles the Claude account.
func withRCB(c *client.Config, sel string) (err error) {
	if c.RCB, c.RCBLink, err = rcbProfile(sel); err != nil {
		return err
	}
	return pinAccount(c)
}

// pinAccount picks the Claude account: --rcb always means its profile's own
// (RCC_ACCOUNT does not apply, a different --account is an error); else
// --account, else RCC_ACCOUNT.
func pinAccount(c *client.Config) error {
	if p := c.RCB; p != nil {
		if c.Account != "" && c.Account != p.Account() {
			return fmt.Errorf("--rcb %s uses account %s; drop --account %s", p.ID, p.Account(), c.Account)
		}
		c.Account = p.Account()
		return nil
	}
	if c.Account == "" {
		c.Account = os.Getenv("RCC_ACCOUNT")
	}
	return nil
}

// rcbProfile resolves --rcb against the linked remote-claude-browser.
func rcbProfile(sel string) (*rcb.Profile, *rcb.Link, error) {
	if sel == "" {
		return nil, nil, nil
	}
	l, err := rcb.Load()
	if err != nil {
		return nil, nil, err
	}
	ps, err := rcb.Profiles(l)
	if err != nil {
		return nil, nil, err
	}
	p, err := chooseProfile(ps, sel)
	if err != nil {
		return nil, nil, err
	}
	if err := p.Usable(); err != nil {
		return nil, nil, err
	}
	exit := "exit not checked yet"
	if p.Exit != nil {
		exit = "exit " + p.Exit.IP
		if p.Exit.TimeZone != "" {
			exit += ", " + p.Exit.TimeZone
		}
	}
	fmt.Fprintf(os.Stderr, "rcc: profile %s → node %s (%s)\n", p.Label(), p.Node.Name, exit)
	return &p, &l, nil
}

// chooseProfile is rcb.Find, asking which one when a name fits several.
func chooseProfile(ps []rcb.Profile, sel string) (rcb.Profile, error) {
	p, err := rcb.Find(ps, sel)
	var amb *rcb.AmbiguousError
	if !errors.As(err, &amb) || !term.IsTerminal(int(os.Stdin.Fd())) {
		return p, err
	}
	fmt.Fprintf(os.Stderr, "%q fits several profiles:\n", sel)
	return pickProfile(amb.Candidates)
}

// pickProfile asks which of ps to use (a terminal is needed).
func pickProfile(ps []rcb.Profile) (rcb.Profile, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return rcb.Profile{}, errors.New("which profile? name one (see: rcb profiles)")
	}
	amb := &rcb.AmbiguousError{Candidates: ps}
	for i, c := range amb.Candidates {
		node := "no node"
		if c.Node != nil {
			node = c.Node.Name
		}
		fmt.Fprintf(os.Stderr, "  %d) %s  %s  [%s]\n", i+1, c.ID, c.Label(), node)
	}
	fmt.Fprint(os.Stderr, "which one? ")
	var answer string
	fmt.Scanln(&answer)
	n, convErr := strconv.Atoi(strings.TrimSpace(answer))
	if convErr != nil || n < 1 || n > len(amb.Candidates) {
		return rcb.Profile{}, errors.New("no profile chosen")
	}
	return amb.Candidates[n-1], nil
}

// rcbDefaults fills in what a command left out: the profile (and server)
// remembered for this directory, else the only remote-cc server here.
func rcbDefaults(sel string) (profile, host string, err error) {
	cwd, _ := os.Getwd()
	u, dir, ok := rcb.Lookup(cwd)
	if sel == "" {
		if !ok {
			return "", "", errors.New("no HOST given, and this directory has no profile (rcb use PROFILE)")
		}
		fmt.Fprintf(os.Stderr, "rcc: using profile %s on %s (set for %s)\n", u.Profile, u.Server, dir)
		return u.Profile, u.Server, nil
	}
	if ok && u.Server != "" {
		return sel, u.Server, nil
	}
	switch servers := rcb.Servers(); len(servers) {
	case 1:
		return sel, servers[0], nil
	case 0:
		return "", "", errors.New("no remote-cc server on this machine yet (device join one first)")
	default:
		return "", "", fmt.Errorf("which server? give HOST, one of: %s", strings.Join(servers, ", "))
	}
}
