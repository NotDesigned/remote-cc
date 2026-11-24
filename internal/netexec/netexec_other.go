//go:build !linux

package netexec

import (
	"fmt"
	"os"
)

func Main(args []string) int {
	fmt.Fprintln(os.Stderr, "rcc: node namespaces need Linux")
	return 126
}
