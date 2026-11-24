// Command rcc lets Claude Code running on a remote machine (B) work on the
// local machine (A). See docs/DESIGN.md.
//
//	rcc [options] [user@]host [dir] [-- claude args...]
//	rcc serve [flags]     A side: run only the command agent (manual mode)
//	rcc shim CMD          B side: forward CMD to the agent
//	rcc-shim CMD          same, when invoked through a file named rcc-shim
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"remote-cc/internal/agent"
	"remote-cc/internal/update"
	"remote-cc/internal/procjob"
	"remote-cc/internal/session"
	"remote-cc/internal/shim"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == procjob.GuardArg {
		procjob.RunGuard()
		return
	}
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "rcc-shim" {
		os.Exit(runShim(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	session.Prog = prog()
	update.CleanupOld()
	if os.Args[1] != "shim" && os.Args[1] != "version" && os.Args[1] != "--version" {
		// Whatever rcc starts on this machine (commands, ssh) ends with it.
		if err := procjob.KillOnExit(); err != nil {
			fmt.Fprintln(os.Stderr, "rcc: warning: child processes may outlive rcc:", err)
		}
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "shim":
		os.Exit(runShim(os.Args[2:]))
	case "join":
		os.Exit(runJoin(os.Args[2:]))
	case "setup-server":
		os.Exit(runSetupServer(os.Args[2:]))
	case "remove-server":
		os.Exit(runRemoveServer(os.Args[2:]))
	case "update-server":
		os.Exit(runUpdateServer(os.Args[2:]))
	case "server":
		os.Exit(runServerCmd(os.Args[2:]))
	case "user":
		os.Exit(runUserCmd(os.Args[2:]))
	case "update":
		os.Exit(runUpdate(os.Args[2:]))
	case "login", "logout", "status", "setup-token", "token", "accounts":
		os.Exit(runAccountCommand(os.Args[1], os.Args[2:]))
	case "version", "--version":
		fmt.Println(prog(), version)
		if len(os.Args) > 2 && os.Args[2] == "--check" {
			if tag, err := update.Latest(); err == nil {
				fmt.Printf("latest release: %s%s\n", tag, map[bool]string{true: "  (run: " + prog() + " update)", false: ""}[update.Newer(tag, version)])
			}
		}
	case "help", "-h", "--help":
		usage()
	default:
		os.Exit(runSession(os.Args[1:]))
	}
}

// prog is the name rcc was invoked as, without .exe.
func prog() string {
	return strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
}

func usage() {
	fmt.Fprint(os.Stderr, usageText())
}

func usageText() string {
	p := prog()
	t := strings.ReplaceAll(usageTemplate, "\n  rcc ", "\n  "+p+" ")
	return strings.Replace(t, "usage:\n", "usage:\n", 1)
}

const usageTemplate = `usage:
  rcc [options] [user@]host [dir] [-- claude-args...]

  Runs Claude Code on host, working on dir (default: current directory) of
  this machine. Commands run here; files are served from here.

options:
  -v               verbose; -vv, -vvv, -vvvv also make ssh verbose (like ssh
                   -v/-vv/-vvv): the first connections print to the terminal,
                   the file channel's ssh output goes to the log file
  --mode MODE      auto (default) | ns (private mount namespace, needs root on
                   host) | global (normal mount; dir path must be creatable)
  --claude PATH    path of claude on host (default: auto-detect)
  --no-is-sandbox  do not set IS_SANDBOX=1 for claude when the remote user is
                   root (it is set by default so --dangerously-skip-permissions
                   works there)
  --no-prompt      do not append remote-cc's system prompt (it tells Claude
                   that commands run on this machine, not on host)
  --profile NAME   conversation history store on host (default: one per local
                   machine, auto-named; "none" = host's shared ~/.claude)
  ssh options passed through: -p PORT -i KEY -J JUMP -o OPT -F CONFIG -l USER
                              -4 -6 -A -C -q

  --account NAME   Claude account on host (default: $RCC_ACCOUNT, else this
                   machine's own account, so machines never share Claude
                   credentials; "default" = the host's shared ~/.claude). Each
                   account has its own config dir there; settings, CLAUDE.md,
                   skills, agents, commands and plugins stay shared via symlinks.

server (Docker):
  rcc setup-server [--user owner] [--alias NAME] [--port 2222] [--bind ADDR] [--data /var/lib/remote-cc]
                   [--image IMG] [--key FILE.pub]... [--keys-from-host]
                   [--migrate-history] [--name remote-cc] [--install-docker]
                   [ssh options] user@host
                   install/update a multi-user server (gateway + one container per
                   user) with you as its first user: creates a key for it on this
                   machine, writes ~/.ssh/remote-cc/config (alias <host>-cc) and
                   pins the host keys. --single: the old one-container layout.
                   user is root or any account with sudo (asks for the sudo
                   password if needed); --install-docker uses get.docker.com

  rcc remove-server [--purge] [--install-dir /opt/remote-cc] [ssh options] user@host
                   stop and delete the container and its image; --purge also
                   deletes the data dir (logins, history, host keys)

  rcc update-server [--force] [--image IMG] [ssh options] user@host
                   pull the latest server image (new Claude Code) and recreate
                   the container if anything changed; data and keys are kept;
                   refuses while sessions are running unless --force

multi-user servers (one gateway port, one container per user):
  (all accept --public-host NAME: the address users connect to, when it
   differs from how you reach the host, e.g. administering via localhost)
  rcc server init [--port 2222] [--bind ADDR] [ssh options] user@host
  rcc server migrate --user NAME [--force] [ssh options] user@host
                  turn a single-user deployment into a multi-user one
  rcc user add    [--key FILE.pub]... [--alias NAME] [ssh options] host user
                  (no --key: prints a one-time invite for the user)
  rcc user invite [--ttl 168h] [--alias NAME] [ssh options] host user
  rcc join [--alias NAME] <invite>       on the user's machine: enroll and configure
  rcc user list   [ssh options] host
  rcc user config [--alias NAME] [ssh options] host user
  rcc user remove [--purge] [--force] [ssh options] host user

updates:
  rcc update [--check] [--version vX.Y.Z]   replace this rcc with a release
  rcc version [--check]

accounts (run on host, no mount):
  rcc login [--account NAME] [ssh options] host          claude auth login
  rcc logout|status [--account NAME] [ssh options] host  claude auth logout|status
  rcc setup-token [--account NAME] [ssh options] host    claude setup-token
  rcc token --account NAME [ssh options] host            store a setup-token for
                   the account (read here, hidden); "-- --clear" removes it
  rcc accounts [ssh options] host                        list accounts

other commands:
  rcc serve [--listen ADDR] [--token TOKEN] [--shell PATH] [-q]
  rcc shim CMD
  rcc version
`

// parseArgs parses rcc options shared by sessions and account commands.
// It returns the config and the positional arguments.
func parseArgs(args []string) (session.Config, []string, bool) {
	cfg := session.Config{Mode: "auto", Account: os.Getenv("RCC_ACCOUNT")}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "rcc: %s needs a value\n", a)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		switch {
		case a == "--":
			cfg.ClaudeArgs = args[i+1:]
			i = len(args)
		case len(a) >= 2 && strings.Trim(a[1:], "v") == "" && a[0] == '-' && a != "-":
			// -v: rcc verbose; each extra v raises ssh's log level
			// (-vv = ssh -v, -vvv = ssh -vv, -vvvv = ssh -vvv).
			cfg.Verbose = true
			if n := len(a) - 2; n > 0 {
				if n > 3 {
					n = 3
				}
				cfg.SSHArgs = append(cfg.SSHArgs, "-o", fmt.Sprintf("LogLevel=DEBUG%d", n))
			}
		case a == "--mode":
			cfg.Mode = val()
		case a == "--claude":
			cfg.ClaudePath = val()
		case a == "--account":
			cfg.Account = val()
		case a == "--no-prompt":
			cfg.NoPrompt = true
		case a == "--no-is-sandbox":
			cfg.NoIsSandbox = true
		case a == "--profile":
			cfg.Profile = val()
		case a == "-p" || a == "-i" || a == "-J" || a == "-o" || a == "-F" || a == "-l":
			cfg.SSHArgs = append(cfg.SSHArgs, a, val())
		case a == "-4" || a == "-6" || a == "-A" || a == "-C" || a == "-q":
			cfg.SSHArgs = append(cfg.SSHArgs, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			fmt.Fprintf(os.Stderr, "rcc: unknown option %s\n", a)
			return cfg, nil, false
		default:
			pos = append(pos, a)
		}
	}
	return cfg, pos, true
}

func runSession(args []string) int {
	cfg, pos, ok := parseArgs(args)
	if !ok || len(pos) < 1 || len(pos) > 2 {
		usage()
		return 2
	}
	cfg.Destination = pos[0]
	cfg.Dir = "."
	if len(pos) == 2 {
		cfg.Dir = pos[1]
	}
	code, err := session.Run(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// runAccountCommand handles: login, logout, status, setup-token, token,
// accounts. Extra claude arguments go after "--".
func runAccountCommand(action string, args []string) int {
	cfg, pos, ok := parseArgs(args)
	if !ok || len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "usage: rcc %s [options] [user@]host\n", action)
		return 2
	}
	cfg.Destination = pos[0]
	if action == "token" {
		for _, a := range cfg.ClaudeArgs {
			if a == "--clear" {
				action = "token-clear"
			}
		}
	}
	code, err := session.Command(cfg, action, cfg.ClaudeArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// runSetupServer: rcc setup-server [setup flags] [ssh options] [user@]host
func runSetupServer(args []string) int {
	sc := session.SetupConfig{Port: 2222, Bind: "", DataDir: "/var/lib/remote-cc", Name: "remote-cc",
		InstallDir: "/opt/remote-cc", Image: "ghcr.io/notdesigned/remote-cc-server:latest"}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "rcc: %s needs a value\n", a)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		switch a {
		case "--port":
			p, err := strconv.Atoi(val())
			if err != nil || p < 1 || p > 65535 {
				fmt.Fprintln(os.Stderr, "rcc: bad --port")
				return 2
			}
			sc.Port = p
		case "--bind":
			sc.Bind = val()
		case "--data":
			sc.DataDir = val()
		case "--install-dir":
			sc.InstallDir = val()
		case "--image":
			sc.Image = val()
		case "--key":
			sc.KeyFiles = append(sc.KeyFiles, val())
		case "--keys-from-host":
			sc.KeysFromHost = true
		case "--migrate-history":
			sc.MigrateHistory = true
		case "--name":
			sc.Name = val()
		case "--install-docker":
			sc.InstallDocker = true
		case "--single":
			sc.Single = true
		case "--user":
			sc.User = val()
		case "--alias":
			sc.Alias = val()
		case "--public-host":
			sc.PublicHost = val()
		default:
			rest = append(rest, a)
		}
	}
	cfg, pos, ok := parseArgs(rest)
	if !ok || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rcc setup-server [--port N] [--bind ADDR] [--data DIR] [--image IMG] [--key FILE.pub]... [--keys-from-host] [--migrate-history] [--name NAME] [--install-docker] [ssh options] [user@]host")
		return 2
	}
	cfg.Destination = pos[0]
	sc.Config = cfg
	if err := session.Setup(sc); err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	return 0
}

// runRemoveServer: rcc remove-server [--purge] [--install-dir DIR] [ssh options] [user@]host
func runRemoveServer(args []string) int {
	dir, purge := "/opt/remote-cc", false
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--purge":
			purge = true
		case "--install-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "rcc: --install-dir needs a value")
				return 2
			}
			i++
			dir = args[i]
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, pos, ok := parseArgs(rest)
	if !ok || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rcc remove-server [--purge] [--install-dir DIR] [ssh options] [user@]host")
		return 2
	}
	cfg.Destination = pos[0]
	if err := session.Remove(cfg, dir, purge); err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	return 0
}

// runUpdate: rcc update [--check] [--force] [--version vX.Y.Z]
func runUpdate(args []string) int {
	check, force, want := false, false, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check":
			check = true
		case "--force":
			force = true
		case "--version":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "rcc: --version needs a value")
				return 2
			}
			i++
			want = args[i]
		default:
			fmt.Fprintln(os.Stderr, "usage: rcc update [--check] [--force] [--version vX.Y.Z]")
			return 2
		}
	}
	tag := want
	if tag == "" {
		var err error
		if tag, err = update.Latest(); err != nil {
			fmt.Fprintln(os.Stderr, "rcc:", err)
			return 1
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
		fmt.Fprintln(os.Stderr, "rcc: update failed:", err)
		return 1
	}
	fmt.Printf("updated %s to %s\n", exe, tag)
		fmt.Printf("servers: run `%s update-server <host>` to update a remote-cc server container\n", prog())
	return 0
}

// runUpdateServer: rcc update-server [--force] [--image IMG] [--install-dir DIR] [ssh options] [user@]host
func runUpdateServer(args []string) int {
	dir, image, force := "/opt/remote-cc", "", false
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "rcc: %s needs a value\n", a)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		switch a {
		case "--force":
			force = true
		case "--image":
			image = val()
		case "--install-dir":
			dir = val()
		default:
			rest = append(rest, a)
		}
	}
	cfg, pos, ok := parseArgs(rest)
	if !ok || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rcc update-server [--force] [--image IMG] [--install-dir DIR] [ssh options] [user@]host")
		return 2
	}
	cfg.Destination = pos[0]
	if err := session.UpdateServer(cfg, dir, image, force); err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	return 0
}

// splitMulti pulls the multi-user admin flags out of args.
func splitMulti(args []string, extra map[string]*[]string, bools map[string]*bool) (session.MultiConfig, []string, bool) {
	mc := session.MultiConfig{InstallDir: "/opt/remote-cc", DataDir: "/var/lib/remote-cc", Port: 2222,
		Image: "ghcr.io/notdesigned/remote-cc-server:latest"}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "rcc: %s needs a value\n", a)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		if p, ok := extra[a]; ok {
			*p = append(*p, val())
			continue
		}
		if b, ok := bools[a]; ok {
			*b = true
			continue
		}
		switch a {
		case "--install-dir":
			mc.InstallDir = val()
		case "--data":
			mc.DataDir = val()
		case "--port":
			p, err := strconv.Atoi(val())
			if err != nil || p < 1 || p > 65535 {
				fmt.Fprintln(os.Stderr, "rcc: bad --port")
				os.Exit(2)
			}
			mc.Port = p
		case "--bind":
			mc.Bind = val()
		case "--image":
			mc.Image = val()
		case "--public-host":
			mc.PublicHost = val()
		default:
			rest = append(rest, a)
		}
	}
	cfg, pos, ok := parseArgs(rest)
	mc.Config = cfg
	return mc, pos, ok
}

func fail(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	return 0
}

// runServerCmd: rcc server init|migrate ...
func runServerCmd(args []string) int {
	usage := func() int {
		fmt.Fprintf(os.Stderr, `usage:
  %[1]s server init    [--port 2222] [--bind ADDR] [--data /var/lib/remote-cc] [--image IMG] [ssh options] user@host
  %[1]s server migrate --user NAME [--alias NAME] [--force] [--image IMG] [ssh options] user@host
                 convert a single-user deployment: its owner becomes user NAME
`, prog())
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	var user, alias []string
	var force bool
	mc, pos, ok := splitMulti(args[1:], map[string]*[]string{"--user": &user, "--alias": &alias}, map[string]*bool{"--force": &force})
	if !ok || len(pos) != 1 {
		return usage()
	}
	mc.Destination = pos[0]
	switch args[0] {
	case "init":
		return fail(session.ServerInit(mc))
	case "migrate":
		if len(user) == 0 {
			fmt.Fprintln(os.Stderr, "rcc: server migrate needs --user NAME (the name the current owner gets)")
			return 2
		}
		a := ""
		if len(alias) > 0 {
			a = alias[len(alias)-1]
		}
		return fail(session.ServerMigrate(mc, user[len(user)-1], a, force))
	}
	return usage()
}

// runUserCmd: rcc user add|list|remove|config ...
func runUserCmd(args []string) int {
	usage := func() int {
		p := prog()
		fmt.Fprintf(os.Stderr, `usage:
  %[1]s user add    [--key FILE.pub]... [--alias NAME] [ssh options] host user
                    (without --key it prints a one-time invite)
  %[1]s user invite [--ttl 168h] [--alias NAME] [ssh options] host user
  %[1]s user list   [ssh options] host
  %[1]s user config [--alias NAME] [ssh options] host user
  %[1]s user remove [--purge] [--force] [ssh options] host user
`, p)
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	var keys, alias, ttlv []string
	var purge, force bool
	mc, pos, ok := splitMulti(args[1:], map[string]*[]string{"--key": &keys, "--alias": &alias, "--ttl": &ttlv},
		map[string]*bool{"--purge": &purge, "--force": &force})
	if !ok {
		return usage()
	}
	last := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return v[len(v)-1]
	}
	switch args[0] {
	case "list":
		if len(pos) != 1 {
			return usage()
		}
		mc.Destination = pos[0]
		return fail(session.UserList(mc))
	case "invite":
		if len(pos) != 2 {
			return usage()
		}
		mc.Destination = pos[0]
		ttl := 7 * 24 * time.Hour
		if t := last(ttlv); t != "" {
			d, err := time.ParseDuration(t)
			if err != nil {
				fmt.Fprintln(os.Stderr, "rcc: bad --ttl (e.g. 24h, 168h)")
				return 2
			}
			ttl = d
		}
		return fail(session.UserInvite(mc, pos[1], last(alias), ttl))
	case "add", "config", "remove":
		if len(pos) != 2 {
			return usage()
		}
		mc.Destination = pos[0]
		switch args[0] {
		case "add":
			return fail(session.UserAdd(mc, pos[1], keys, last(alias)))
		case "config":
			return fail(session.UserConfig(mc, pos[1], last(alias)))
		default:
			return fail(session.UserRemove(mc, pos[1], purge, force))
		}
	}
	return usage()
}

// runJoin: rcc join [--alias NAME] <invite>
func runJoin(args []string) int {
	alias, inv := "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--alias" && i+1 < len(args):
			i++
			alias = args[i]
		case inv == "" && !strings.HasPrefix(args[i], "-"):
			inv = args[i]
		default:
			fmt.Fprintln(os.Stderr, "usage: "+prog()+" join [--alias NAME] <invite>")
			return 2
		}
	}
	if inv == "" {
		fmt.Fprintln(os.Stderr, "usage: "+prog()+" join [--alias NAME] <invite>")
		return 2
	}
	return fail(session.Join(inv, alias))
}

func runShim(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "rcc-shim: expected exactly one argument (the command string)")
		return 126
	}
	return shim.Run(args[0])
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:0", "TCP address to listen on")
	token := fs.String("token", "", "shared secret (random if empty)")
	shell := fs.String("shell", "", "bash-compatible shell (auto-detect if empty)")
	quiet := fs.Bool("q", false, "do not log commands")
	fs.Parse(args)

	if *token == "" {
		*token = randomToken()
	}
	opt := agent.Options{Token: *token, Shell: *shell}
	if !*quiet {
		opt.Logger = log.New(os.Stderr, "rcc: ", log.LstdFlags)
	}
	a, err := agent.New(opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "rcc: agent listening on %s (shell %s)\n", l.Addr(), a.Shell())
	fmt.Fprintf(os.Stderr, "rcc: on B:  export RCC_SOCK=<forwarded socket> RCC_TOKEN=%s CLAUDE_CODE_SHELL_PREFIX=<path to rcc-shim>\n", *token)
	if err := a.Serve(l); err != nil {
		fmt.Fprintln(os.Stderr, "rcc:", err)
		return 1
	}
	return 0
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
