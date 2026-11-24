// Package scripts holds the POSIX sh scripts rcc runs on servers. They are
// plain files (linted with shellcheck in CI) embedded into the binary. A
// script sent to a server is: parameter assignments + the requested parts.
//
//	lib/     shared prologues and functions
//	client/  run in the user's container during a session
//	server/  run as root on the Docker host (admin commands)
//	device/  run in the user's container (self-service device management)
package scripts

import (
	"embed"
	"strings"
)

//go:embed lib client server device
var files embed.FS

// Get returns one script by path, e.g. "client/probe.sh".
func Get(name string) string {
	b, err := files.ReadFile(name)
	if err != nil {
		panic("scripts: missing " + name)
	}
	return string(b)
}

// Join concatenates scripts in order.
func Join(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(Get(n))
		b.WriteString("\n")
	}
	return b.String()
}

// Host is a server (Docker host) script: become root, load the Docker
// helpers, then run name.
func Host(name string) string { return Join("lib/root.sh", "lib/docker.sh", name) }

// Client is a script for the user's container, with the account helpers.
func Client(name string) string { return Join("lib/account.sh", name) }
