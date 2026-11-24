// Command rcc lets Claude Code running on a server work on this machine:
// commands run here, files are served from here. See README.md and
// docs/DESIGN.md.
package main

import (
	"os"

	"remote-cc/internal/cli"
)

var version = "dev" // set by scripts/build.sh

func main() { os.Exit(cli.Main(os.Args, version)) }
