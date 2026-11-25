package cli

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"remote-cc/internal/agent"
	"remote-cc/internal/client"
	"remote-cc/internal/remote"
	"remote-cc/internal/shim"
	"remote-cc/internal/sshconf"
	"remote-cc/internal/update"
)

func runJoin(in *invocation) int {
	s, err := sshconf.Join(in.pos[0], in.s("--alias"))
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\nDone: this machine can now reach %s as %q.\n  %s account login %s     # sign in to Claude (once per machine)\n  %s %s           # in a project directory\n",
		s.Host, s.Alias, remote.Prog, s.Alias, remote.Prog, s.Alias)
	return 0
}

func runDeviceInvite(in *invocation) int {
	ttl := "24h"
	if v := in.s("--ttl"); v != "" {
		ttl = v
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return usageError(in.cmd, errors.New("bad --ttl (e.g. 2h, 24h, 168h)"))
	}
	return fail(client.DeviceInvite(in.pos[0], d))
}

func runDeviceList(in *invocation) int { return fail(client.DeviceList(in.pos[0])) }

func runDeviceRemove(in *invocation) int {
	return fail(client.DeviceRemove(in.pos[0], in.pos[1], in.b("--force")))
}

func runVersion(in *invocation) int {
	fmt.Println(remote.Prog, in.version)
	if in.b("--check") {
		if tag, err := update.Latest(); err == nil {
			hint := ""
			if update.Newer(tag, in.version) {
				hint = "  (run: " + remote.Prog + " update)"
			}
			fmt.Printf("latest release: %s%s\n", tag, hint)
		}
	}
	return 0
}

func runUpdate(in *invocation) int {
	want := in.s("--version")
	tag := want
	if tag == "" {
		var err error
		if tag, err = update.Latest(); err != nil {
			return fail(err)
		}
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	which := "latest"
	if want != "" {
		which = "requested"
	}
	fmt.Printf("current: %s   %s: %s\n", in.version, which, tag)
	if in.b("--check") {
		return 0
	}
	if want == "" && !in.b("--force") && !update.Newer(tag, in.version) {
		fmt.Println("already up to date")
		return 0
	}
	exe, err := update.Install(tag)
	if err != nil {
		return fail(fmt.Errorf("update failed: %w", err))
	}
	fmt.Printf("updated %s to %s\nservers: run `%s server update SERVER` to update a remote-cc server\n", exe, tag, remote.Prog)
	return 0
}

// runShim is rcc-shim: run one command string through the agent.
func runShim(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "rcc-shim: expected exactly one argument (the command string)")
		return 126
	}
	return shim.Run(args[0])
}

// runServe runs the command agent alone, for debugging the command channel.
func runServe(in *invocation) int {
	listen, token := in.s("--listen"), in.s("--token")
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	if token == "" {
		token = remote.RandHex(16)
	}
	opt := agent.Options{Token: token, Shell: in.s("--shell")}
	if !in.b("-q") {
		opt.Logger = log.New(os.Stderr, "rcc: ", log.LstdFlags)
	}
	a, err := agent.New(opt)
	if err != nil {
		return fail(err)
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "rcc: agent listening on %s (shell %s)\n", l.Addr(), a.Shell())
	fmt.Fprintf(os.Stderr, "rcc: on the server: export RCC_SOCK=<forwarded socket> RCC_TOKEN=%s CLAUDE_CODE_SHELL_PREFIX=<path to rcc-shim>\n", token)
	return fail(a.Serve(l))
}
