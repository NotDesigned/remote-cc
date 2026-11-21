// Package pathmap converts between A's native absolute paths and the path
// under which they appear on B (always a POSIX path).
//
// On POSIX hosts the mapping is the identity. On Windows, `C:\Users\x\proj`
// maps to `/c/Users/x/proj` — the same form Git Bash uses, so commands run
// by Git Bash on A see exactly the paths Claude Code sees on B.
package pathmap

import (
	"path/filepath"
	"strings"
)

// ToRemote maps an absolute local path (native form for goos) to B's form.
func ToRemote(goos, local string) string {
	if goos != "windows" {
		return filepath.ToSlash(local)
	}
	p := strings.ReplaceAll(local, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(p[:1])
		rest := p[2:]
		if rest == "" {
			rest = "/"
		}
		return "/" + drive + rest
	}
	return p
}

// ToLocal maps a path in B's form back to the native form for goos.
func ToLocal(goos, remote string) string {
	if goos != "windows" {
		return remote
	}
	// /c/Users/x -> C:\Users\x
	if len(remote) >= 2 && remote[0] == '/' && isLetter(remote[1]) && (len(remote) == 2 || remote[2] == '/') {
		rest := remote[2:]
		if rest == "" {
			rest = "/"
		}
		return strings.ToUpper(remote[1:2]) + ":" + strings.ReplaceAll(rest, "/", `\`)
	}
	return strings.ReplaceAll(remote, "/", `\`)
}

func isLetter(b byte) bool { return (b|0x20) >= 'a' && (b|0x20) <= 'z' }
