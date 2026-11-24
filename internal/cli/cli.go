// Package cli is rcc's command line.
package cli

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"remote-cc/internal/agent"
	"remote-cc/internal/client"
	"remote-cc/internal/procjob"
	"remote-cc/internal/rcb"
	"remote-cc/internal/remote"
	"remote-cc/internal/server"
	"remote-cc/internal/shim"
	"remote-cc/internal/sshconf"
	"remote-cc/internal/update"
)

const usageText = `usage:
  rcc [options] HOST [DIR] [-- CLAUDE-ARGS...]     Claude Code on HOST, working on DIR here
      -v, -vv, -vvv, -vvvv   verbose; extra v's make ssh verbose too
      --account NAME         Claude login: default this machine's own; "default" = shared
      --profile NAME         history: default this machine's own; "none" = the account's
      --rcb PROFILE          run as a remote-claude-browser profile: its Claude account,
                             all traffic through its proxy node (needs rcb login, server egress)
      --claude PATH  --no-prompt  --no-is-sandbox
      ssh options: -p PORT -i KEY -J JUMP -o OPT -F CONFIG -l USER -4 -6 -A -C -q

on your machine:
  rcc join [--alias NAME] INVITE              enroll this machine with an invite
  rcc device invite [--ttl 24h] HOST          invite another machine of yours
  rcc device list HOST                        your devices and pending invites
  rcc device remove [--force] HOST DEVICE     remove a device (key comment or fingerprint)
  rcc login|logout|status|setup-token [--account NAME | --rcb PROFILE] HOST
  rcc rcb login URL | profiles | logout   link this machine to remote-claude-browser
  rcc token [--account NAME] [--clear] HOST   store a setup-token for an account
  rcc accounts HOST
  rcc update [--check] [--version vX.Y.Z]     replace this program with a release
  rcc version [--check]

servers (run as root or with sudo on the Docker host):
  rcc setup-server [--user owner] [--alias NAME] [--port 2222] [--bind ADDR]
                   [--key FILE.pub]... [--install-docker] [server options] user@host
                   install a server with you as its first user, and configure this machine
  rcc user add [--key FILE.pub]... [server options] user@host NAME   (no --key: prints an invite)
  rcc user invite [--ttl 168h] [server options] user@host NAME
  rcc user list | config NAME | remove [--purge] [--force] NAME ...
  rcc update-server [--force] [--image IMG] [server options] user@host
  rcc remove-server [--purge] [server options] user@host
  rcc server init [server options] user@host                 gateway only
  rcc server migrate --user NAME [--force] user@host           convert a 0.3/0.4 single-container server
  rcc server egress --rcb URL [--image IMG] user@host          egress host for --rcb sessions (asks for a host token)
      server options: --install-dir DIR --data DIR --image IMG --public-host NAME --name INSTANCE

  rcc serve [--listen ADDR] [--token T] [--shell PATH]   the command agent alone (debugging)
`

// Main runs rcc with os.Args-style arguments and returns the exit code.
func Main(args []string, version string) int {
	if len(args) == 2 && args[1] == procjob.GuardArg {
		procjob.RunGuard()
		return 0
	}
	prog := strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	if prog == "rcc-shim" {
		return runShim(args[1:])
	}
	remote.Prog = prog
	if len(args) < 2 {
		usage()
		return 2
	}
	update.CleanupOld()
	cmd, rest := args[1], args[2:]
	if cmd != "shim" && cmd != "version" && cmd != "--version" {
		// Whatever rcc starts on this machine (commands, ssh) ends with it.
		if err := procjob.KillOnExit(); err != nil {
			fmt.Fprintln(os.Stderr, "rcc: warning: child processes may outlive rcc:", err)
		}
	}
	switch cmd {
	case "help", "-h", "--help":
		usage()
		return 0
	case "version", "--version":
		fmt.Println(prog, version)
		if len(rest) > 0 && rest[0] == "--check" {
			if tag, err := update.Latest(); err == nil {
				fmt.Printf("latest release: %s%s\n", tag, map[bool]string{true: "  (run: " + prog + " update)", false: ""}[update.Newer(tag, version)])
			}
		}
		return 0
	case "update":
		return runUpdate(rest, version)
	case "serve":
		return runServe(rest)
	case "shim":
		return runShim(rest)
	case "join":
		return runJoin(rest)
	case "device":
		return runDevice(rest)
	case "login", "logout", "status", "setup-token", "token", "accounts":
		return runAccount(cmd, rest)
	case "rcb":
		return runRCB(rest)
	case "setup-server", "update-server", "remove-server", "server", "user":
		return runServer(cmd, rest)
	}
	return runSession(args[1:])
}

func usage() {
	fmt.Fprint(os.Stderr, strings.ReplaceAll(usageText, "  rcc ", "  "+remote.Prog+" "))
}

func fail(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	return 0
}

func code(c int, err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		if c == 0 {
			c = 1
		}
	}
	return c
}

// flags declares a command's options besides the shared ssh ones.
type flags struct {
	str   map[string]*string
	list  map[string]*[]string
	bools map[string]*bool
}

// parse splits args into the ssh target options (and -v…), the command's
// own flags, positional arguments, and everything after "--".
func parse(args []string, f flags) (t remote.Target, pos, after []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			return t, pos, args[i+1:], nil
		case len(a) >= 2 && a[0] == '-' && strings.Trim(a[1:], "v") == "":
			// -v: rcc verbose; each extra v raises ssh's log level.
			t.Verbose = true
			if n := min(3, len(a)-2); n > 0 {
				t.Args = append(t.Args, "-o", fmt.Sprintf("LogLevel=DEBUG%d", n))
			}
		case a == "-p" || a == "-i" || a == "-J" || a == "-o" || a == "-F" || a == "-l":
			v, e := next()
			if e != nil {
				return t, nil, nil, e
			}
			t.Args = append(t.Args, a, v)
		case a == "-4" || a == "-6" || a == "-A" || a == "-C" || a == "-q":
			t.Args = append(t.Args, a)
		case f.str[a] != nil:
			v, e := next()
			if e != nil {
				return t, nil, nil, e
			}
			*f.str[a] = v
		case f.list[a] != nil:
			v, e := next()
			if e != nil {
				return t, nil, nil, e
			}
			*f.list[a] = append(*f.list[a], v)
		case f.bools[a] != nil:
			*f.bools[a] = true
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return t, nil, nil, fmt.Errorf("unknown option %s", a)
		default:
			pos = append(pos, a)
		}
	}
	return t, pos, nil, nil
}

func runSession(args []string) int {
	var c client.Config
	var rcbSel string
	t, pos, after, err := parse(args, flags{
		str:   map[string]*string{"--claude": &c.ClaudePath, "--profile": &c.Profile, "--account": &c.Account, "--rcb": &rcbSel},
		bools: map[string]*bool{"--no-prompt": &c.NoPrompt, "--no-is-sandbox": &c.NoIsSandbox},
	})
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "rcc:", err)
		}
		usage()
		return 2
	}
	if c.Account == "" {
		c.Account = os.Getenv("RCC_ACCOUNT")
	}
	if c.RCB, err = rcbProfile(rcbSel); err != nil {
		return fail(err)
	}
	t.Dest = pos[0]
	c.Target, c.ClaudeArgs, c.Dir = t, after, "."
	if len(pos) == 2 {
		c.Dir = pos[1]
	}
	return code(client.Run(c))
}

func runAccount(action string, args []string) int {
	var c client.Config
	var clear bool
	var rcbSel string
	t, pos, after, err := parse(args, flags{
		str:   map[string]*string{"--account": &c.Account, "--claude": &c.ClaudePath, "--rcb": &rcbSel},
		bools: map[string]*bool{"--clear": &clear},
	})
	if err != nil || len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s %s [--account NAME] HOST\n", remote.Prog, action)
		return 2
	}
	if c.Account == "" {
		c.Account = os.Getenv("RCC_ACCOUNT")
	}
	if rcbSel != "" && action == "accounts" {
		return fail(errors.New("accounts lists every account; --rcb does not apply"))
	}
	if c.RCB, err = rcbProfile(rcbSel); err != nil {
		return fail(err)
	}
	if action == "token" && (clear || (len(after) > 0 && after[0] == "--clear")) {
		action = "token-clear"
	}
	t.Dest = pos[0]
	c.Target = t
	return code(client.Account(c, action, after))
}

func runJoin(args []string) int {
	var alias string
	_, pos, _, err := parse(args, flags{str: map[string]*string{"--alias": &alias}})
	if err != nil || len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s join [--alias NAME] INVITE\n", remote.Prog)
		return 2
	}
	s, err := sshconf.Join(pos[0], alias)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\nDone: this machine can now reach %s as %q.\n  %s login %s     # sign in to Claude (once per machine)\n  %s %s           # in a project directory\n",
		s.Host, s.Alias, remote.Prog, s.Alias, remote.Prog, s.Alias)
	return 0
}

func runDevice(args []string) int {
	usage := func() int {
		fmt.Fprintf(os.Stderr, "usage:\n  %[1]s device invite [--ttl 24h] HOST\n  %[1]s device list HOST\n  %[1]s device remove [--force] HOST DEVICE\n", remote.Prog)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	ttl := "24h"
	var force bool
	_, pos, _, err := parse(args[1:], flags{str: map[string]*string{"--ttl": &ttl}, bools: map[string]*bool{"--force": &force}})
	if err != nil {
		return usage()
	}
	switch {
	case args[0] == "invite" && len(pos) == 1:
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return fail(errors.New("bad --ttl (e.g. 2h, 24h, 168h)"))
		}
		return fail(client.DeviceInvite(pos[0], d))
	case args[0] == "list" && len(pos) == 1:
		return fail(client.DeviceList(pos[0]))
	case args[0] == "remove" && len(pos) == 2:
		return fail(client.DeviceRemove(pos[0], pos[1], force))
	}
	return usage()
}

func runServer(cmd string, args []string) int {
	sub := ""
	if cmd == "server" || cmd == "user" {
		if len(args) == 0 {
			usage()
			return 2
		}
		sub, args = args[0], args[1:]
	}
	var c server.Config
	var port, user, alias, ttl, image, rcbURL string
	var keys []string
	var purge, force bool
	t, pos, _, err := parse(args, flags{
		str: map[string]*string{"--install-dir": &c.InstallDir, "--data": &c.DataDir, "--port": &port, "--bind": &c.Bind,
			"--image": &image, "--public-host": &c.PublicHost, "--name": &c.Name, "--user": &user, "--alias": &alias, "--ttl": &ttl,
			"--rcb": &rcbURL},
		list:  map[string]*[]string{"--key": &keys},
		bools: map[string]*bool{"--install-docker": &c.InstallDocker, "--purge": &purge, "--force": &force},
	})
	if err != nil {
		return fail(err)
	}
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return fail(errors.New("bad --port"))
		}
		c.Port = p
	}
	c.Image = image
	need := func(n int) bool {
		if len(pos) != n {
			usage()
			return false
		}
		t.Dest = pos[0]
		c.Target = t
		return true
	}
	switch {
	case cmd == "setup-server" && need(1):
		var extra []string
		for _, kf := range keys {
			b, err := os.ReadFile(kf)
			if err != nil {
				return fail(err)
			}
			for _, l := range strings.Split(string(b), "\n") {
				if l = strings.TrimSpace(l); sshconf.Fingerprint(l) != "" {
					extra = append(extra, l)
				}
			}
		}
		return fail(server.Setup(c, user, alias, extra))
	case cmd == "update-server" && need(1):
		return fail(server.Update(c, image, force))
	case cmd == "remove-server" && need(1):
		return fail(server.Remove(c, purge))
	case cmd == "server" && sub == "init" && need(1):
		return fail(server.Init(c))
	case cmd == "server" && sub == "egress" && need(1):
		if rcbURL == "" {
			return fail(errors.New("server egress needs --rcb URL (your remote-claude-browser)"))
		}
		return fail(server.Egress(c, rcbURL, image))
	case cmd == "server" && sub == "migrate" && need(1):
		if user == "" {
			return fail(errors.New("server migrate needs --user NAME (the name the current owner gets)"))
		}
		return fail(server.Migrate(c, user, alias, force))
	case cmd == "user" && sub == "list" && need(1):
		return fail(server.UserList(c))
	case cmd == "user" && sub == "add" && need(2):
		return fail(server.UserAdd(c, pos[1], keys, alias))
	case cmd == "user" && sub == "invite" && need(2):
		d := 7 * 24 * time.Hour
		if ttl != "" {
			if d, err = time.ParseDuration(ttl); err != nil {
				return fail(errors.New("bad --ttl (e.g. 24h, 168h)"))
			}
		}
		return fail(server.UserInvite(c, pos[1], alias, d))
	case cmd == "user" && sub == "config" && need(2):
		return fail(server.UserConfig(c, pos[1], alias))
	case cmd == "user" && sub == "remove" && need(2):
		return fail(server.UserRemove(c, pos[1], purge, force))
	}
	return 2
}

func runUpdate(args []string, version string) int {
	var want string
	var check, force bool
	_, pos, _, err := parse(args, flags{str: map[string]*string{"--version": &want},
		bools: map[string]*bool{"--check": &check, "--force": &force}})
	if err != nil || len(pos) != 0 {
		fmt.Fprintf(os.Stderr, "usage: %s update [--check] [--force] [--version vX.Y.Z]\n", remote.Prog)
		return 2
	}
	tag := want
	if tag == "" {
		if tag, err = update.Latest(); err != nil {
			return fail(err)
		}
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	fmt.Printf("current: %s   %s: %s\n", version, map[bool]string{true: "requested", false: "latest"}[want != ""], tag)
	if check {
		return 0
	}
	if want == "" && !force && !update.Newer(tag, version) {
		fmt.Println("already up to date")
		return 0
	}
	exe, err := update.Install(tag)
	if err != nil {
		return fail(fmt.Errorf("update failed: %w", err))
	}
	fmt.Printf("updated %s to %s\nservers: run `%s update-server <host>` to update a remote-cc server\n", exe, tag, remote.Prog)
	return 0
}

func runShim(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "rcc-shim: expected exactly one argument (the command string)")
		return 126
	}
	return shim.Run(args[0])
}

// runServe runs the command agent alone, for debugging the command channel.
func runServe(args []string) int {
	listen, token, shell := "127.0.0.1:0", "", ""
	var quiet bool
	_, pos, _, err := parse(args, flags{str: map[string]*string{"--listen": &listen, "--token": &token, "--shell": &shell},
		bools: map[string]*bool{"-q": &quiet}})
	if err != nil || len(pos) != 0 {
		fmt.Fprintln(os.Stderr, "usage: rcc serve [--listen ADDR] [--token TOKEN] [--shell PATH] [-q]")
		return 2
	}
	if token == "" {
		token = remote.RandHex(16)
	}
	opt := agent.Options{Token: token, Shell: shell}
	if !quiet {
		opt.Logger = log.New(os.Stderr, "rcc: ", log.LstdFlags)
	}
	a, err := agent.New(opt)
	if err != nil {
		return fail(err)
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "rcc: agent listening on %s (shell %s)\n", l.Addr(), a.Shell())
	fmt.Fprintf(os.Stderr, "rcc: on the server: export RCC_SOCK=<forwarded socket> RCC_TOKEN=%s CLAUDE_CODE_SHELL_PREFIX=<path to rcc-shim>\n", token)
	return fail(a.Serve(l))
}

// rcbProfile resolves --rcb against the linked remote-claude-browser.
func rcbProfile(sel string) (*rcb.Profile, error) {
	if sel == "" {
		return nil, nil
	}
	l, err := rcb.Load()
	if err != nil {
		return nil, err
	}
	ps, err := rcb.Profiles(l)
	if err != nil {
		return nil, err
	}
	p, err := rcb.Find(ps, sel)
	if err != nil {
		return nil, err
	}
	if err := p.Usable(); err != nil {
		return nil, err
	}
	exit := "exit not checked yet"
	if p.Exit != nil {
		exit = "exit " + p.Exit.IP
		if p.Exit.TimeZone != "" {
			exit += ", " + p.Exit.TimeZone
		}
	}
	fmt.Fprintf(os.Stderr, "rcc: profile %s → node %s (%s)\n", p.Label(), p.Node.Name, exit)
	return &p, nil
}

func runRCB(args []string) int {
	usage := func() int {
		fmt.Fprintf(os.Stderr, "usage:\n  %[1]s rcb login URL     link this machine (asks for a client token)\n  %[1]s rcb profiles      your browser profiles\n  %[1]s rcb logout        forget the link\n", remote.Prog)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch {
	case args[0] == "login" && len(args) == 2:
		u, err := rcb.NormalizeURL(args[1])
		if err != nil {
			return fail(err)
		}
		tok := strings.TrimSpace(os.Getenv("RCC_RCB_TOKEN"))
		if tok == "" {
			if tok, err = remote.ReadPassword("client token (remote-claude-browser: 代理与 API → remote-cc → 电脑上的 remote-cc): "); err != nil {
				return fail(err)
			}
			tok = strings.TrimSpace(tok)
		}
		if !strings.HasPrefix(tok, "rcck_") {
			return fail(errors.New("that is not a client token (they start with rcck_)"))
		}
		l := rcb.Link{URL: u, Token: tok}
		ps, err := rcb.Profiles(l)
		if err != nil {
			return fail(err)
		}
		if err := rcb.Save(l); err != nil {
			return fail(err)
		}
		fmt.Printf("linked to %s: %d profile(s)\n  %s rcb profiles     # list them\n  %s --rcb PROFILE HOST # run Claude Code as one\n", u, len(ps), remote.Prog, remote.Prog)
		return 0
	case args[0] == "profiles" && len(args) == 1:
		l, err := rcb.Load()
		if err != nil {
			return fail(err)
		}
		ps, err := rcb.Profiles(l)
		if err != nil {
			return fail(err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tCLAUDE ACCOUNT\tNODE\tEXIT\tTIME ZONE")
		for _, p := range ps {
			node, ip, tz := "-", "-", "-"
			if p.Node != nil {
				node = p.Node.Name
			}
			if p.Exit != nil {
				ip, tz = p.Exit.IP, p.Exit.TimeZone
			}
			email := p.Email
			switch {
			case p.Unavailable:
				email += " (unavailable)"
			case email == "":
				email = "-"
			}
			fmt.Fprintln(tw, strings.Join([]string{p.ID, p.Name, email, node, ip, tz}, "\t"))
		}
		tw.Flush()
		return 0
	case args[0] == "logout" && len(args) == 1:
		return fail(rcb.Remove())
	}
	return usage()
}
