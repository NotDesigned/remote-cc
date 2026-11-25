// Package shimcmd is rcc-shim's command line, shared by cmd/rcc-shim and by
// rcc itself (a development build uploads itself as the shim).
package shimcmd

import (
	"fmt"
	"os"
	"path/filepath"

	"remote-cc/internal/netexec"
	"remote-cc/internal/session"
	"remote-cc/internal/shim"
)

// Main runs rcc-shim with os.Args-style arguments and returns the exit code.
//
//	git ARGS…                  as bin/git: Claude Code's own git calls (shim.Git)
//	__rcc-netexec …            --rcb: run inside the profile's node namespace
//	__rcc-daemon RUN           start the session daemon (internal/session)
//	__rcc-dial SOCKET          connect ssh's stdio to the session daemon
//	COMMAND                    run one command on the computer (CLAUDE_CODE_SHELL_PREFIX)
func Main(args []string) int {
	if filepath.Base(args[0]) == "git" {
		return shim.Git(args[1:])
	}
	rest := args[1:]
	if netexec.Is(rest) {
		return netexec.Main(rest)
	}
	if len(rest) > 0 {
		switch rest[0] {
		case "__rcc-daemon":
			return session.Daemon(rest[1:])
		case "__rcc-dial":
			return session.Dial(rest[1:])
		}
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "rcc-shim: expected exactly one argument (the command string)")
		return 126
	}
	return shim.Run(rest[0])
}
