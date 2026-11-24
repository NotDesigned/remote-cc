// Package netexec starts a program inside a node's network namespace from the
// egress host (internal/egress), after checking where its traffic leaves.
// Two stages, both run as rcc-shim:
//
//	__rcc-netexec -- PROG ARGS...   get the namespace, enter it, re-exec stage 2
//	__rcc-netcheck -- PROG ARGS...  (whole process inside) check exit, account; exec PROG
//
// The caller provides a private mount namespace (unshare -m) so stage 1 can
// point /etc/resolv.conf at the namespace's resolver for this session only.
package netexec

const (
	Stage1 = "__rcc-netexec"
	Stage2 = "__rcc-netcheck"

	EnvSock     = "RCC_EGRESS_SOCK"      // egress daemon socket
	EnvNode     = "RCC_EGRESS_NODE"      // node id
	EnvResolv   = "RCC_EGRESS_RESOLV"    // file to write the resolver config to
	EnvExpectIP = "RCC_EGRESS_EXPECT_IP" // comma-separated exit IPs to accept; "" = any
	EnvTZ       = "RCC_EGRESS_TZ"        // TZ for the program
	EnvEmail    = "RCC_EGRESS_EMAIL"     // Claude account the program must be logged in as; "" = no check
	EnvLabel    = "RCC_EGRESS_LABEL"     // human name of the profile, for messages
	EnvCheckURL = "RCC_EGRESS_CHECK_URL" // exit lookup; default https://ipwho.is/

	DefaultSock = "/run/rcc-egress/egress.sock"
)

// Is reports whether args (after the program name) start a netexec stage.
func Is(args []string) bool {
	return len(args) > 0 && (args[0] == Stage1 || args[0] == Stage2)
}
