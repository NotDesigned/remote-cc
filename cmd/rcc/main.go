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
	case "setup-server":
		os.Exit(runSetupServer(os.Args[2:]))
	case "remove-server":
		os.Exit(runRemoveServer(os.Args[2:]))
	case "update-server":
		os.Exit(runUpdateServer(os.Args[2:]))
	case "update":
		os.Exit(runUpdate(os.Args[2:]))
	case "login", "logout", "status", "setup-token", "token", "accounts":
		os.Exit(runAccountCommand(os.Args[1], os.Args[2:]))
	case "version", "--version":
		fmt.Println("rcc", version)
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
  rcc setup-server [--port 2222] [--bind 0.0.0.0] [--data /var/lib/remote-cc]
                   [--image IMG] [--key FILE.pub]... [--keys-from-host]
                   [--migrate-history] [--name remote-cc] [--install-docker]
                   [ssh options] user@host
                   install/update the server container on a host with Docker;
                   authorizes your public key and pins the container's host key.
                   user is root or any account with sudo (asks for the sudo
                   password if needed); --install-docker uses get.docker.com

  rcc remove-server [--purge] [--install-dir /opt/remote-cc] [ssh options] user@host
                   stop and delete the container and its image; --purge also
                   deletes the data dir (logins, history, host keys)

  rcc update-server [--force] [--image IMG] [ssh options] user@host
                   pull the latest server image (new Claude Code) and recreate
                   the container if anything changed; data and keys are kept;
                   refuses while sessions are running unless --force

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
	sc := session.SetupConfig{Port: 2222, Bind: "0.0.0.0", DataDir: "/var/lib/remote-cc",
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
