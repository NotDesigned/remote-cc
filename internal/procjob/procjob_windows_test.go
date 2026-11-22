//go:build windows

package procjob

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The test binary re-runs itself as a helper that joins the job, starts a
// long-lived grandchild, prints its PID and waits. The test then kills the
// helper the hardest way (TerminateProcess) and expects the grandchild gone.
func TestMain(m *testing.M) {
	if os.Getenv("RCC_PROCJOB_HELPER") == "1" {
		if err := KillOnExit(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		c := exec.Command("ping", "-n", "120", "127.0.0.1")
		if err := c.Start(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println("PID", c.Process.Pid)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestGrandchildDiesWithParent(t *testing.T) {
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), "RCC_PROCJOB_HELPER=1")
	out, _ := helper.StdoutPipe()
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	f := strings.Fields(line)
	if len(f) != 2 || f[0] != "PID" {
		helper.Process.Kill()
		t.Fatalf("helper said %q", line)
	}
	pid, _ := strconv.Atoi(f[1])
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		helper.Process.Kill()
		t.Fatalf("open grandchild: %v", err)
	}
	defer windows.CloseHandle(h)

	helper.Process.Kill() // TerminateProcess: no cleanup code runs in the helper
	helper.Wait()

	ev, err := windows.WaitForSingleObject(h, 5000)
	if err != nil || ev != windows.WAIT_OBJECT_0 {
		windows.TerminateProcess(h, 1)
		t.Fatalf("grandchild still running 5s after its parent was killed (wait=%d err=%v)", ev, err)
	}
}
