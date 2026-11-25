//go:build !linux

package session

import (
	"fmt"
	"os"
)

// Daemon runs only on Linux servers.
func Daemon(args []string) int {
	fmt.Fprintln(os.Stderr, "rcc-shim: the session daemon runs on Linux only")
	return 1
}
