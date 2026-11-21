// Command rcc-shim is the small B-side binary that Claude Code runs via
// CLAUDE_CODE_SHELL_PREFIX. rcc embeds Linux builds of it and uploads the
// right one to B automatically.
package main

import (
	"fmt"
	"os"

	"remote-cc/internal/shim"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "rcc-shim: expected exactly one argument (the command string)")
		os.Exit(126)
	}
	os.Exit(shim.Run(os.Args[1]))
}
