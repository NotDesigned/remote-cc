// Command rcc lets Claude Code running on a remote machine (B) work on the
// local machine (A). See docs/DESIGN.md.
//
//	rcc serve [flags]     A side: run the command agent (manual / M1 mode)
//	rcc shim CMD          B side: forward CMD to the agent
//	rcc-shim CMD          same, when invoked through a symlink named rcc-shim
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
		fmt.Fprintf(os.Stderr, "rcc: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  rcc serve [--listen ADDR] [--token TOKEN] [--shell PATH] [-q]
  rcc shim CMD
`)
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
