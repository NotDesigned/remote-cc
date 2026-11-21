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
	"strings"

	"remote-cc/internal/agent"
	"remote-cc/internal/session"
	"remote-cc/internal/shim"
)

var version = "dev"

func main() {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "rcc-shim" {
		os.Exit(runShim(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "shim":
		os.Exit(runShim(os.Args[2:]))
	case "version", "--version":
		fmt.Println("rcc", version)
	case "help", "-h", "--help":
		usage()
	default:
		os.Exit(runSession(os.Args[1:]))
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  rcc [options] [user@]host [dir] [-- claude-args...]

  Runs Claude Code on host, working on dir (default: current directory) of
  this machine. Commands run here; files are served from here.

options:
  -v               verbose
  --mode MODE      auto (default) | ns (private mount namespace, needs root on
                   host) | global (normal mount; dir path must be creatable)
  --claude PATH    path of claude on host (default: auto-detect)
  ssh options passed through: -p PORT -i KEY -J JUMP -o OPT -F CONFIG -l USER
                              -4 -6 -A -C -q

other commands:
  rcc serve [--listen ADDR] [--token TOKEN] [--shell PATH] [-q]
  rcc shim CMD
  rcc version
`)
}

func runSession(args []string) int {
	cfg := session.Config{Mode: "auto"}
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
		case a == "-v":
			cfg.Verbose = true
		case a == "--mode":
			cfg.Mode = val()
		case a == "--claude":
			cfg.ClaudePath = val()
		case a == "-p" || a == "-i" || a == "-J" || a == "-o" || a == "-F" || a == "-l":
			cfg.SSHArgs = append(cfg.SSHArgs, a, val())
		case a == "-4" || a == "-6" || a == "-A" || a == "-C" || a == "-q":
			cfg.SSHArgs = append(cfg.SSHArgs, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			fmt.Fprintf(os.Stderr, "rcc: unknown option %s\n", a)
			usage()
			return 2
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
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
