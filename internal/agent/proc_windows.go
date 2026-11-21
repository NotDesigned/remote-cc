//go:build windows

package agent

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"remote-cc/internal/proto"
)

const createNewProcessGroup = 0x00000200

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
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
