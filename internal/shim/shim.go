// Package shim is the B-side half of the command channel. Claude Code runs it
// via CLAUDE_CODE_SHELL_PREFIX; it forwards the command to the rcc agent on A
// and behaves like the command ran locally (streams, exit code, cwd).
package shim

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"remote-cc/internal/proto"
)

const (
	EnvSock  = "RCC_SOCK"  // "unix:/path", "/path" or "tcp:host:port"
	EnvToken = "RCC_TOKEN" //
)

// Run executes cmdStr on A and returns the exit code to use.
func Run(cmdStr string) int {
	addr := os.Getenv(EnvSock)
	if addr == "" {
		return fail("%s is not set; start Claude Code through rcc", EnvSock)
	}
	nc, err := dial(addr)
	if err != nil {
		return fail("cannot reach rcc on the local machine (%s): %v", addr, err)
	}
	defer nc.Close()
	c := proto.NewConn(nc)

	rw := Rewrite(cmdStr)
	cwd, _ := os.Getwd()
	st := proto.Start{
		Version:    proto.Version,
		Token:      os.Getenv(EnvToken),
		Cmd:        rw.Cmd,
		Cwd:        cwd,
		Env:        forwardEnv(os.Environ(), rw.Cmd),
		CaptureCwd: rw.CwdFile != "",
	}
	if err := c.WriteJSON(proto.TStart, st); err != nil {
		return fail("send: %v", err)
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			c.WriteJSON(proto.TSignal, proto.Signal{Name: sigName(s)})
		}
	}()

	go func() {
		io.Copy(proto.ChunkWriter{C: c, T: proto.TStdin}, os.Stdin)
		c.Write(proto.TStdinEOF, nil)
	}()

	for {
		f, err := c.Read()
		if err != nil {
			return fail("connection to local machine lost: %v", err)
		}
		switch f.Type {
		case proto.TStdout:
			os.Stdout.Write(f.Payload)
		case proto.TStderr:
			os.Stderr.Write(f.Payload)
		case proto.TError:
			return fail("%s", f.Payload)
		case proto.TExit:
			var ex proto.Exit
			if err := json.Unmarshal(f.Payload, &ex); err != nil {
				return fail("bad exit frame: %v", err)
			}
			if rw.CwdFile != "" && ex.Cwd != "" {
				// Same semantics as `pwd -P >| FILE`.
				os.WriteFile(rw.CwdFile, []byte(ex.Cwd+"\n"), 0o600)
			}
			return ex.Code
		}
	}
}

func dial(addr string) (net.Conn, error) {
	switch {
	case strings.HasPrefix(addr, "tcp:"):
		return net.Dial("tcp", strings.TrimPrefix(addr, "tcp:"))
	default:
		return net.Dial("unix", strings.TrimPrefix(addr, "unix:"))
	}
}

var refVarRe = regexp.MustCompile(`\$\{?(__[A-Za-z0-9_]+)`)

// forwardEnv picks the variables Claude Code sets for its shell commands.
// B's general environment (PATH, HOME, …) is deliberately not forwarded:
// commands must see A's own environment.
func forwardEnv(environ []string, cmd string) map[string]string {
	referenced := map[string]bool{}
	for _, m := range refVarRe.FindAllStringSubmatch(cmd, -1) {
		referenced[m[1]] = true
	}
	keep := map[string]bool{
		"TERM": true, "COLORTERM": true, "NO_COLOR": true, "FORCE_COLOR": true,
		"COLUMNS": true, "LINES": true, "GIT_EDITOR": true,
	}
	out := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "CLAUDE_CODE_SHELL_PREFIX" || strings.HasPrefix(k, "RCC_"):
		case keep[k], referenced[k],
			strings.HasPrefix(k, "CLAUDE"), strings.HasPrefix(k, "__CLAUDE"):
			out[k] = v
		}
	}
	return out
}

func sigName(s os.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGHUP:
		return "HUP"
	default:
		return "TERM"
	}
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "rcc-shim: "+format+"\n", args...)
	return 126
}
