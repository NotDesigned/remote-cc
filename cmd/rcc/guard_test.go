//go:build !windows

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// kill -9 on rcc must still take down the commands it started, including
// their background children (the Unix counterpart of the Windows job).
func TestCommandsDieWithRcc(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "rcc")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	shimPath := filepath.Join(dir, "rcc-shim")
	os.Symlink(bin, shimPath)

	serve := exec.Command(bin, "serve", "--listen", "127.0.0.1:0", "--token", "tok", "-q")
	stderr, _ := serve.StderrPipe()
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer serve.Process.Kill()
	line, _ := bufio.NewReader(stderr).ReadString('\n')
	m := regexp.MustCompile(`listening on (\S+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("serve said %q", line)
	}

	// The command prints its shell PID and a background child's PID.
	shim := exec.Command(shimPath, `echo $$; sleep 300 & echo $!; wait`)
	shim.Env = append(os.Environ(), "RCC_SOCK=tcp:"+m[1], "RCC_TOKEN=tok")
	out, _ := shim.StdoutPipe()
	if err := shim.Start(); err != nil {
		t.Fatal(err)
	}
	defer shim.Process.Kill()
	br := bufio.NewReader(out)
	var pids []int
	for len(pids) < 2 {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading pids: %v (got %v)", err, pids)
		}
		p, _ := strconv.Atoi(strings.TrimSpace(l))
		pids = append(pids, p)
	}
	alive := func(p int) bool { return syscall.Kill(p, 0) == nil }
	for _, p := range pids {
		if !alive(p) {
			t.Fatalf("pid %d not running before kill", p)
		}
	}
	guards := guardPids(t, bin)
	if len(guards) != 1 {
		t.Fatalf("expected one guard process, found %v", guards)
	}

	serve.Process.Signal(syscall.SIGKILL) // no cleanup code runs in rcc
	serve.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pids[0]) && !alive(pids[1]) && len(guardPids(t, bin)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("after kill -9 of rcc: shell alive=%v, background child alive=%v, guards=%v",
		alive(pids[0]), alive(pids[1]), guardPids(t, bin))
}

func guardPids(t *testing.T, bin string) []string {
	out, _ := exec.Command("pgrep", "-f", "^"+regexp.QuoteMeta(bin)+" __rcc_guard$").Output()
	return strings.Fields(string(out))
}
