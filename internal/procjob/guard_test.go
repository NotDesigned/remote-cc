//go:build !windows

package procjob

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Started as `<this test binary> __rcc_guard`, the process is a guard (not
// another test run) and ends when its pipe closes.
func TestTestBinaryRunsAsGuard(t *testing.T) {
	cmd := exec.Command(os.Args[0], GuardArg)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("guard: %v", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the guard did not end when its pipe closed")
	}
}
