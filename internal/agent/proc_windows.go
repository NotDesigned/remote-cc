//go:build windows

package agent

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"remote-cc/internal/proto"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// setProcessGroup gives the command its own hidden console instead of the
// user's, which is busy showing Claude's TUI through ssh: console programs
// can neither read keystrokes from it nor draw over it, and no window flashes.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow}
}

// wrapScript switches the hidden console to UTF-8 first, so Windows programs
// (cmd, ping, …) do not print in the ANSI code page (e.g. GBK on Chinese
// Windows) and garble what Claude reads.
func wrapScript(s string) string {
	return "chcp.com 65001 >/dev/null 2>&1; " + s
}

// Windows has no POSIX signals; any signal tears down the whole process tree.
func signalGroup(cmd *exec.Cmd, name string) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if kill.Run() != nil {
		_ = cmd.Process.Kill()
	}
}

func exitInfo(ps *os.ProcessState) *proto.Exit {
	if ps == nil {
		return &proto.Exit{Code: 126}
	}
	return &proto.Exit{Code: ps.ExitCode()}
}
