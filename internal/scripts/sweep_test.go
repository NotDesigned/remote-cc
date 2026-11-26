package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// A run dir whose daemon pid now belongs to another process (pids are
// reused after a restart) is swept; one whose daemon still runs is kept.
func TestSessionSweepChecksPidReuse(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	cache := t.TempDir()
	live := filepath.Join(cache, "run", "live")
	reused := filepath.Join(cache, "run", "reused")
	os.MkdirAll(live, 0o700)
	os.MkdirAll(reused, 0o700)
	// "The daemon": its command line names its run dir.
	daemon := exec.Command("sh", "-c", "sleep 30", "rcc-shim", live)
	other := exec.Command("sleep", "30")
	for _, c := range []*exec.Cmd{daemon, other} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer c.Process.Kill()
	}
	os.WriteFile(filepath.Join(live, "daemon.pid"), []byte(strconv.Itoa(daemon.Process.Pid)+"\n"), 0o600)
	os.WriteFile(filepath.Join(reused, "daemon.pid"), []byte(strconv.Itoa(other.Process.Pid)+"\n"), 0o600)
	cmd := exec.Command("sh", "-c", Client("client/session.sh"))
	cmd.Env = append(os.Environ(), "CACHE="+cache, "RUN="+filepath.Join(cache, "run", "new"), "R=/tmp/x",
		"SHIM_SHA=none", "LAUNCH_B64=eA==", "PROF=", "CFG="+t.TempDir(), "HOME="+t.TempDir())
	cmd.Run() // fails later: there is no rcc-shim
	if _, err := os.Stat(live); err != nil {
		t.Error("swept a running session")
	}
	if _, err := os.Stat(reused); err == nil {
		t.Error("kept a run whose pid belongs to another process")
	}
}
