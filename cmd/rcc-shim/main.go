// Command rcc-shim is the small B-side binary that Claude Code runs via
// CLAUDE_CODE_SHELL_PREFIX. rcc embeds Linux builds of it and uploads the
// right one to B automatically.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"remote-cc/internal/shim"
)

func main() {
	if filepath.Base(os.Args[0]) == "git" {
		os.Exit(shim.Git(os.Args[1:]))
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "rcc-shim: expected exactly one argument (the command string)")
		os.Exit(126)
	}
	os.Exit(shim.Run(os.Args[1]))
}
