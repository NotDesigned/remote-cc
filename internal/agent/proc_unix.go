//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"strings"
	"syscall"

	"remote-cc/internal/proto"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

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
