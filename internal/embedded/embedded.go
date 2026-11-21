// Package embedded carries prebuilt Linux rcc-shim binaries so that rcc can
// install the shim on B regardless of A's OS/arch. scripts/build.sh fills
// shims/ before building rcc; in a plain `go build` the directory holds only
// a placeholder and rcc falls back to uploading its own executable when A
// and B share OS and architecture.
package embedded

import (
	"embed"
	"io/fs"
)

//go:embed shims
var shims embed.FS

// Shim returns the rcc-shim binary for linux/<goarch>, or nil.
func Shim(goarch string) []byte {
	b, err := fs.ReadFile(shims, "shims/rcc-shim-linux-"+goarch)
	if err != nil {
		return nil
	}
	return b
}
