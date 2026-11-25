// Package cli is rcc's command line.
//
// Every command is one entry in the table built by commandTable: its name,
// synopsis, the options it takes, how many positional arguments, and the
// function that runs it. Dispatch, option parsing, per-command usage and the
// help listing all come from that table, so a command is added (or changed)
// in one place. The command files are grouped by audience: session.go (a
// Claude Code session and its accounts), local.go (this machine: devices,
// device, update), rcb.go (remote-claude-browser), server.go (admin).
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"remote-cc/internal/procjob"
	"remote-cc/internal/rcb"
	"remote-cc/internal/remote"
	"remote-cc/internal/shimcmd"
	"remote-cc/internal/update"
)

// command is one entry of the command table.
type command struct {
	name    string // "server user add"; "" is the session
	args    string // synopsis after the name
	summary string
	group   string   // heading in the help listing; "" = hidden
	help    string   // extra text for `rcc help CMD` (option notes)
	ssh     bool     // takes ssh options (-p -i -J -o -F -l -4 -6 -A -C -q) and -v…
	flags   []string // its own options, each declared in flagKinds
	nargs   [2]int   // positional arguments: min, max (-1: any)
	noJob   bool     // does not tie child processes to rcc (shim, version, help)
	raw     bool     // arguments are not rcc's to parse: all of them are positional
	short   string   // args in the overview, when leaving out the options is not enough
	run     func(*invocation) int
}

// synopsis is "rcc NAME ARGS".
func (c *command) synopsis() string {
	s := remote.Prog
	if c.name != "" {
		s += " " + c.name
	}
	if c.args != "" {
		s += " " + c.args
	}
	return s
}

// invocation is a parsed command line.
type invocation struct {
	cmd     *command
	name    string        // the name it was called by
	target  remote.Target // ssh options and -v (Dest left for the command)
	pos     []string      // positional arguments
	after   []string      // everything after "--"
	str     map[string]string
	list    map[string][]string
	on      map[string]bool
	version string // rcc's own
}

func (in *invocation) s(name string) string   { return in.str[name] }
func (in *invocation) b(name string) bool     { return in.on[name] }
func (in *invocation) l(name string) []string { return in.list[name] }

type flagKind int

const (
	strFlag flagKind = iota + 1
	boolFlag
	listFlag // repeatable, takes a value each time
)

// flagKinds declares every command option. A command lists the ones it takes.
var flagKinds = map[string]flagKind{
	// sessions and accounts
	"--account": strFlag, "--profile": strFlag, "--claude": strFlag, "--rcb": strFlag,
	"--no-prompt": boolFlag, "--no-is-sandbox": boolFlag, "--remote-git": boolFlag, "--clear": boolFlag,
	// this machine
	"--alias": strFlag, "--ttl": strFlag, "--version": strFlag, "--check": boolFlag,
	// servers
	"--install-dir": strFlag, "--data": strFlag, "--image": strFlag, "--public-host": strFlag, "--name": strFlag,
	"--user": strFlag, "--port": strFlag, "--bind": strFlag, "--forget": strFlag, "--key": listFlag,
	"--install-docker": boolFlag, "--purge": boolFlag, "--force": boolFlag, "--all": boolFlag,
	// serve
	"--listen": strFlag, "--token": strFlag, "--shell": strFlag, "-q": boolFlag,
}

// sshValue and sshBool are the ssh options a remote command passes through.
var (
	sshValue = map[string]bool{"-p": true, "-i": true, "-J": true, "-o": true, "-F": true, "-l": true}
	sshBool  = map[string]bool{"-4": true, "-6": true, "-A": true, "-C": true, "-q": true}
)

// errHelp is parse's answer to -h / --help.
var errHelp = errors.New("help requested")

// parse splits args into ssh options (and -v…) when the command takes them,
// the command's own options, positional arguments, and everything after
// "--". Options and positional arguments may be mixed.
func parse(c *command, args []string) (*invocation, error) {
	in := &invocation{cmd: c, str: map[string]string{}, list: map[string][]string{}, on: map[string]bool{}}
	own := map[string]bool{}
	for _, f := range c.flags {
		own[f] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			in.after = args[i+1:]
			return in, nil
		case a == "-h" || a == "--help":
			return in, errHelp
		case own[a]: // before the ssh options: serve's -q is its own
			switch flagKinds[a] {
			case boolFlag:
				in.on[a] = true
			case strFlag:
				v, err := value()
				if err != nil {
					return nil, err
				}
				in.str[a] = v
			case listFlag:
				v, err := value()
				if err != nil {
					return nil, err
				}
				in.list[a] = append(in.list[a], v)
			}
		case c.ssh && len(a) >= 2 && a[0] == '-' && strings.Trim(a[1:], "v") == "":
			// -v: rcc verbose; each extra v raises ssh's log level.
			in.target.Verbose = true
			if n := min(3, len(a)-2); n > 0 {
				in.target.Args = append(in.target.Args, "-o", fmt.Sprintf("LogLevel=DEBUG%d", n))
			}
		case c.ssh && sshValue[a]:
			v, err := value()
			if err != nil {
				return nil, err
			}
			in.target.Args = append(in.target.Args, a, v)
		case c.ssh && sshBool[a]:
			in.target.Args = append(in.target.Args, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return nil, fmt.Errorf("unknown option %s", a)
		default:
			in.pos = append(in.pos, a)
		}
	}
	return in, nil
}

var commands []*command

// renamed is the commands' names before the noun-verb layout. They are not accepted any more
// (a session would take them for a HOST): rcc says the new name instead.
var renamed = map[string]string{
	"setup-server": "server setup", "update-server": "server update", "remove-server": "server remove", "servers": "server list",
	"login": "account login", "logout": "account logout", "status": "account status", "setup-token": "account setup-token",
	"token": "account token", "accounts": "account list",
	"join": "device join", "user": "server user",
}

func init() { commands = commandTable() }

// The two kinds of host a command names.
const (
	// hostHelp: what session, account and device commands connect to.
	hostHelp = "HOST is your container: the ssh alias that server setup or device join made (e.g. myserver-cc)."
	// serverHelp: what server and user commands connect to.
	serverHelp = "SERVER is the Docker host itself, over ssh as root or a sudo user (e.g. root@myserver).\n" +
		"server options: --install-dir DIR --data DIR --image IMG --public-host NAME --name INSTANCE"
	sshHelp = "ssh options: -p PORT -i KEY -J JUMP -o OPT -F CONFIG -l USER -4 -6 -A -C -q;\n" +
		"-v, -vv, -vvv, -vvvv: verbose; extra v's make ssh verbose too"
)

// commandTable is every command, in help order. The session (no name) comes
// first.
func commandTable() []*command {
	const (
		gSession = "Claude Code in your container (HOST), working on a directory here"
		gLocal   = "your container (HOST) and this machine"
		gRCB     = "remote-claude-browser (as one of its browser profiles)"
		gServer  = "administration (SERVER: the Docker host, as root or with sudo)"
		gDebug   = "debugging"
	)
	srvOpts := []string{"--install-dir", "--data", "--image", "--public-host", "--name"}
	srv := func(own ...string) []string { return append(own, srvOpts...) }
	const rcbHost = "\nWith --rcb, HOST may be left out: the server set for this directory (rcb use), or the only one."
	account := func(action, args, summary string, flags ...string) *command {
		return &command{name: "account " + action, args: args, group: gLocal, ssh: true,
			summary: summary, help: hostHelp + rcbHost + "\n--claude PATH: claude in the container.\n" + sshHelp,
			flags: append([]string{"--account", "--claude", "--rcb"}, flags...), nargs: [2]int{0, 1}, run: runAccount(action)}
	}
	const acctArgs = "[--account NAME | --rcb PROFILE] HOST"
	return []*command{
		{name: "", args: "[options] HOST [DIR] [-- CLAUDE-ARGS...]", group: gSession, ssh: true,
			summary: "Claude Code in HOST, working on DIR (default .) here",
			help: hostHelp + `
options:
  --account NAME         Claude login: default this machine's own; "default" = shared
  --profile NAME         history: default this machine's own; "none" = the account's
  --rcb PROFILE          run as a remote-claude-browser profile: its Claude account,
                         all traffic through its proxy node (needs rcb login, server egress)
  --claude PATH  --no-prompt  --no-is-sandbox
  --remote-git           Claude Code's own git calls (status, --worktree, …) run
                         in the container instead of here
Arguments after -- go to claude.
With no HOST, the profile and server set for this directory (rcb use) apply.
` + sshHelp,
			flags: []string{"--claude", "--profile", "--account", "--rcb", "--no-prompt", "--no-is-sandbox", "--remote-git"},
			nargs: [2]int{0, 2}, run: runSession},

		account("login", acctArgs, "sign in to Claude, for one account"),
		account("logout", acctArgs, "sign out"),
		account("status", acctArgs, "the account's login state"),
		account("setup-token", acctArgs, "make a long-lived token (then: account token)"),
		account("token", "[--account NAME | --rcb PROFILE] [--clear] HOST", "store a setup-token for an account (--clear: remove it)", "--clear"),
		{name: "account list", args: "HOST", group: gLocal, ssh: true, summary: "every account and its login state",
			help: hostHelp + "\n--claude PATH: claude in the container.\n" + sshHelp, flags: []string{"--claude"}, nargs: [2]int{1, 1}, run: runAccount("accounts")},
		{name: "device join", args: "[--alias NAME] INVITE", group: gLocal, summary: "enroll this machine with an invite (sets up HOST)",
			help: "--alias NAME: the HOST name to give it (default: from the invite).", flags: []string{"--alias"}, nargs: [2]int{1, 1}, run: runJoin},
		{name: "device invite", args: "[--ttl 24h] HOST", group: gLocal, summary: "invite another machine of yours", help: hostHelp,
			flags: []string{"--ttl"}, nargs: [2]int{1, 1}, run: runDeviceInvite},
		{name: "device list", args: "HOST", group: gLocal, summary: "your devices and pending invites", help: hostHelp,
			nargs: [2]int{1, 1}, run: runDeviceList},
		{name: "device remove", args: "[--force] HOST DEVICE", group: gLocal, summary: "remove a device (key comment or fingerprint)", help: hostHelp,
			flags: []string{"--force"}, nargs: [2]int{2, 2}, run: runDeviceRemove},
		{name: "update", args: "[--check] [--force] [--version vX.Y.Z]", group: gLocal, summary: "replace this program with a release",
			flags: []string{"--check", "--force", "--version"}, nargs: [2]int{0, 0}, run: runUpdate},
		{name: "version", args: "[--check]", group: gLocal, summary: "this program's version (--check: and the latest)",
			flags: []string{"--check"}, nargs: [2]int{0, 0}, noJob: true, run: runVersion},

		{name: "rcb login", args: "URL", group: gRCB, summary: "link this machine (approve it in the workbench)",
			nargs: [2]int{1, 1}, run: runRCBLogin},
		{name: "rcb profiles", group: gRCB, summary: "your browser profiles", nargs: [2]int{0, 0}, run: runRCBProfiles},
		{name: "rcb use", args: "PROFILE [HOST]", group: gRCB, summary: "remember for this directory; then just: rcc",
			help: hostHelp, nargs: [2]int{1, 2}, run: runRCBUse},
		{name: "rcb logout", group: gRCB, summary: "forget the link", nargs: [2]int{0, 0}, run: runRCBLogout},

		{name: "server setup", group: gServer, ssh: true,
			args:    "[--user owner] [--alias NAME] [--port 2222] [--bind ADDR] [--key FILE.pub]... [--install-docker] [server options] SERVER",
			summary: "install a server, you as its first user; set up HOST here", help: serverHelp + "\n--alias NAME: the HOST name for it here (default <server>-cc).",
			flags: srv("--user", "--alias", "--port", "--bind", "--key", "--install-docker"), nargs: [2]int{1, 1}, run: runSetupServer},
		{name: "server update", args: "[--force] [--image IMG] [server options] SERVER | --all [--force] [--image IMG]",
			short: "SERVER | --all", group: gServer, help: serverHelp, ssh: true,
			summary: "new server image; recreate what changed (--all: every server here)",
			flags:   srv("--force", "--all"), nargs: [2]int{0, 1}, run: runUpdateServer},
		{name: "server list", args: "[--forget SERVER [--install-dir DIR]]", group: gServer,
			summary: "servers this machine administers", help: serverHelp,
			flags: []string{"--forget", "--install-dir"}, nargs: [2]int{0, 0}, run: runServers},
		{name: "server remove", args: "[--purge] [server options] SERVER", group: gServer,
			summary: "remove the containers (--purge: and the data)", help: serverHelp, ssh: true,
			flags: srv("--purge"), nargs: [2]int{1, 1}, run: runRemoveServer},
		{name: "server init", args: "[--port 2222] [--bind ADDR] [--install-docker] [server options] SERVER", group: gServer,
			summary: "gateway only", help: serverHelp, ssh: true,
			flags: srv("--port", "--bind", "--install-docker"), nargs: [2]int{1, 1}, run: runServerInit},
		{name: "server migrate", args: "--user NAME [--alias NAME] [--force] [server options] SERVER", group: gServer,
			summary: "convert a 0.3/0.4 single-container server", help: serverHelp, ssh: true,
			flags: srv("--user", "--alias", "--force"), nargs: [2]int{1, 1}, run: runServerMigrate},
		{name: "server egress", args: "--rcb URL [server options] SERVER", group: gServer,
			summary: "egress host for --rcb sessions (an RCB admin approves it)", help: serverHelp, ssh: true,
			flags: srv("--rcb"), nargs: [2]int{1, 1}, run: runServerEgress},
		{name: "server user add", args: "[--key FILE.pub]... [--alias NAME] [server options] SERVER NAME", group: gServer,
			summary: "add a user (no --key: prints an invite)", help: serverHelp, ssh: true,
			flags: srv("--key", "--alias"), nargs: [2]int{2, 2}, run: runUserAdd},
		{name: "server user invite", args: "[--ttl 168h] [--alias NAME] [server options] SERVER NAME", group: gServer,
			summary: "a new invite for a user", help: serverHelp, ssh: true,
			flags: srv("--ttl", "--alias"), nargs: [2]int{2, 2}, run: runUserInvite},
		{name: "server user list", args: "[server options] SERVER", group: gServer, summary: "the users", help: serverHelp, ssh: true,
			flags: srv(), nargs: [2]int{1, 1}, run: runUserList},
		{name: "server user config", args: "[--alias NAME] [server options] SERVER NAME", group: gServer,
			summary: "a user's ssh config", help: serverHelp, ssh: true,
			flags: srv("--alias"), nargs: [2]int{2, 2}, run: runUserConfig},
		{name: "server user remove", args: "[--purge] [--force] [server options] SERVER NAME", group: gServer,
			summary: "remove a user (--purge: and their data)", help: serverHelp, ssh: true,
			flags: srv("--purge", "--force"), nargs: [2]int{2, 2}, run: runUserRemove},

		{name: "serve", args: "[--listen ADDR] [--token T] [--shell PATH] [-q]", group: gDebug,
			summary: "the command agent alone", flags: []string{"--listen", "--token", "--shell", "-q"}, nargs: [2]int{0, 0}, run: runServe},
		{name: "shim", args: "COMMAND", nargs: [2]int{0, -1}, noJob: true, raw: true,
			run: func(in *invocation) int { return runShim(in.pos) }},
		{name: "help", args: "[COMMAND]", nargs: [2]int{0, -1}, noJob: true, run: runHelp},
	}
}

// lookup finds the command args start with: the longest name that matches
// whole words. noun is set when args start with a command group ("server",
// "server user", …) but name none of its commands: the longest such group.
func lookup(args []string) (c *command, name string, rest []string, noun string) {
	for _, n := range []int{3, 2, 1} {
		if len(args) < n {
			continue
		}
		words := strings.Join(args[:n], " ")
		for _, c := range commands {
			if c.name == words {
				return c, words, args[n:], ""
			}
		}
	}
	for n := min(2, len(args)); n > 0; n-- {
		if w := strings.Join(args[:n], " "); isNoun(w) {
			return nil, "", nil, w
		}
	}
	return nil, "", nil, ""
}

func isNoun(w string) bool {
	for _, c := range commands {
		if strings.HasPrefix(c.name, w+" ") {
			return true
		}
	}
	return false
}

// Main runs rcc with os.Args-style arguments and returns the exit code.
func Main(args []string, version string) int {
	if len(args) == 2 && args[1] == procjob.GuardArg {
		procjob.RunGuard()
		return 0
	}
	prog := strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	switch prog {
	case "rcc-shim", "git": // a development build uploaded as the shim (bin/git too)
		return shimcmd.Main(args)
	}
	remote.Prog = prog
	args = args[1:]
	if len(args) == 0 {
		// In a directory set up with `rcb use`, a bare command starts the session.
		if cwd, err := os.Getwd(); err != nil || !rcbDirectory(cwd) {
			usage(os.Stderr)
			return 2
		}
	}
	if len(args) > 0 && args[0] == "--version" {
		args[0] = "version"
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		args = []string{"help"}
	}
	if len(args) > 0 && renamed[args[0]] != "" {
		old, now := args[0], renamed[args[0]]
		if isNoun(now) && len(args) > 1 { // user add → server user add
			old, now = old+" "+args[1], now+" "+args[1]
		}
		fmt.Fprintf(os.Stderr, "rcc: %s is now: %s %s (see: %[2]s help %[3]s)\n", old, remote.Prog, now)
		return 2
	}
	c, name, rest, noun := lookup(args)
	if noun != "" {
		groupUsage(os.Stderr, noun)
		return 2
	}
	if c == nil { // anything else is a session: rcc [options] HOST [DIR]
		c, name, rest = commands[0], "", args
	}
	return dispatch(c, name, rest, version)
}

func rcbDirectory(dir string) bool { _, _, ok := rcb.Lookup(dir); return ok }

func dispatch(c *command, name string, args []string, version string) int {
	update.CleanupOld()
	if !c.noJob {
		// Whatever rcc starts on this machine (commands, ssh) ends with it.
		if err := procjob.KillOnExit(); err != nil {
			fmt.Fprintln(os.Stderr, "rcc: warning: child processes may outlive rcc:", err)
		}
	}
	in, err := &invocation{cmd: c, pos: args}, error(nil)
	if !c.raw {
		in, err = parse(c, args)
	}
	if err == errHelp {
		commandHelp(os.Stdout, c)
		return 0
	}
	if err == nil && (len(in.pos) < c.nargs[0] || (c.nargs[1] >= 0 && len(in.pos) > c.nargs[1])) {
		err = errors.New("wrong number of arguments")
		if c.name == "" && len(in.pos) > c.nargs[1] {
			err = fmt.Errorf("too many arguments (%q is not a command: %s help)", in.pos[0], remote.Prog)
		}
	}
	if err != nil {
		return usageError(c, err)
	}
	in.name, in.version = name, version
	return c.run(in)
}

// usageError reports a bad command line, with the command's synopsis.
func usageError(c *command, err error) int {
	fmt.Fprintf(os.Stderr, "rcc: %v\nusage: %s\n", err, c.synopsis())
	if c.name != "help" {
		fmt.Fprintf(os.Stderr, "(more: %s)\n", strings.TrimSpace(remote.Prog+" help "+c.name))
	}
	return 2
}

// badUsage marks a run function's error as a bad command line (exit 2,
// with the synopsis) rather than a failure.
type badUsage struct{ error }

// usage is the overview: every command with its positional arguments,
// grouped. The options are in `rcc help COMMAND`.
func usage(w io.Writer) {
	var list []*command
	for _, c := range commands {
		if c.group != "" {
			list = append(list, c)
		}
	}
	fmt.Fprintln(w, "usage:")
	width, group := listWidth(list), list[0].group
	for _, c := range list {
		if c.group != group {
			fmt.Fprintf(w, "\n%s:\n", c.group)
			group = c.group
		}
		fmt.Fprint(w, listLine(c, width))
	}
	fmt.Fprintf(w, "\noptions and details of one command: %s help COMMAND (e.g. %[1]s help server user add)\n", remote.Prog)
}

// groupUsage lists the commands under one noun ("user", "rcb", …).
func groupUsage(w io.Writer, noun string) {
	var list []*command
	for _, c := range commands {
		if strings.HasPrefix(c.name, noun+" ") {
			list = append(list, c)
		}
	}
	fmt.Fprintln(w, "usage:")
	width := listWidth(list)
	for _, c := range list {
		fmt.Fprint(w, listLine(c, width))
	}
	fmt.Fprintf(w, "\noptions and details: %s help %s COMMAND\n", remote.Prog, noun)
}

// brief is "rcc NAME ARGS" without the options: "[--ttl 24h]",
// "[--key FILE.pub]...", "--user NAME", "[server options]".
func (c *command) brief() string {
	args := c.short
	if args == "" {
		args = optGroup.ReplaceAllString(strings.ReplaceAll(c.args, "[server options]", ""), "")
	}
	return strings.Join(strings.Fields(remote.Prog+" "+c.name+" "+args), " ")
}

var optGroup = regexp.MustCompile(`\[-[^\[\]]*(\[[^\[\]]*\])?[^\[\]]*\](\.\.\.)?|--\w[\w-]* [A-Z]+\b`)

// listWidth is the column the summaries start at (long synopses push their
// summary to the next line).
func listWidth(list []*command) int {
	const max = 40
	w := 0
	for _, c := range list {
		if n := len(c.brief()); n <= max && n > w {
			w = n
		}
	}
	return w
}

func listLine(c *command, width int) string {
	sum := strings.ReplaceAll(c.summary, "rcc", remote.Prog)
	if b := c.brief(); len(b) <= width {
		return fmt.Sprintf("  %-*s   %s\n", width, b, sum)
	} else {
		return fmt.Sprintf("  %s\n  %*s   %s\n", b, width, "", sum)
	}
}

func commandHelp(w io.Writer, c *command) {
	fmt.Fprintf(w, "usage: %s\n", c.synopsis())
	if c.summary != "" {
		fmt.Fprintf(w, "\n%s\n", strings.ReplaceAll(c.summary, "rcc", remote.Prog))
	}
	if c.help != "" {
		fmt.Fprintf(w, "\n%s\n", c.help)
	}
}

// runHelp: rcc help [COMMAND…].
func runHelp(in *invocation) int {
	if len(in.pos) == 0 {
		usage(os.Stdout)
		return 0
	}
	c, _, rest, noun := lookup(in.pos)
	switch {
	case noun != "":
		groupUsage(os.Stdout, noun)
	case c != nil && len(rest) == 0:
		commandHelp(os.Stdout, c)
	default:
		fmt.Fprintf(os.Stderr, "rcc: no command %q; commands: %s\n", strings.Join(in.pos, " "), strings.Join(commandNames(), ", "))
		return 2
	}
	return 0
}

// CommandNames is every command's name ("server user add", "help", …), for
// checks that compare the documentation with the command table.
func CommandNames() []string {
	var ns []string
	for _, c := range commands {
		if c.name != "" {
			ns = append(ns, c.name)
		}
	}
	sort.Strings(ns)
	return ns
}

func commandNames() []string {
	var ns []string
	for _, c := range commands {
		if c.group != "" && c.name != "" {
			ns = append(ns, c.name)
		}
	}
	sort.Strings(ns)
	return ns
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
