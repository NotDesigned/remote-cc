// Command rcc-shim is the small server-side binary: Claude Code runs it via
// CLAUDE_CODE_SHELL_PREFIX, and it runs the session daemon. rcc embeds Linux
// builds of it and uploads the right one automatically.
package main

import (
	"os"

	"remote-cc/internal/shimcmd"
)

func main() { os.Exit(shimcmd.Main(os.Args)) }
