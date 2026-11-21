//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"strings"
	"syscall"

	"remote-cc/internal/proto"
)

// setProcessGroup starts the command in a new session: it gets its own
// process group (for signalling the whole tree) and no controlling terminal.
// The user's terminal is in raw mode, owned by the interactive ssh showing
// Claude's TUI; a command opening /dev/tty (sudo/ssh/gpg prompts) must fail
// instead of stealing keystrokes or drawing over the TUI.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func wrapScript(s string) string { return s }

var signals = map[string]syscall.Signal{
	"INT":  syscall.SIGINT,
	"TERM": syscall.SIGTERM,
	"HUP":  syscall.SIGHUP,
	"KILL": syscall.SIGKILL,
	"QUIT": syscall.SIGQUIT,
}

func signalGroup(cmd *exec.Cmd, name string) {
	if cmd.Process == nil {
		return
	}
	sig, ok := signals[name]
	if !ok {
		sig = syscall.SIGTERM
	}
	// Negative pid: the whole process group started for this command.
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

func exitInfo(ps *os.ProcessState) *proto.Exit {
	if ps == nil {
		return &proto.Exit{Code: 126}
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := strings.TrimPrefix(signalName(ws.Signal()), "SIG")
		return &proto.Exit{Code: 128 + int(ws.Signal()), Signal: name}
	}
	return &proto.Exit{Code: ps.ExitCode()}
}

func signalName(s syscall.Signal) string {
	for k, v := range signals {
		if v == s {
			return k
		}
	}
	return s.String()
}
