//go:build !windows

package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"remote-cc/internal/agent"
)

// End-to-end over a real unix socket: agent in-process, shim as a built
// binary invoked exactly the way Claude Code invokes it.
func setup(t *testing.T) (shimPath, sock string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "rcc")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	shimPath = filepath.Join(dir, "rcc-shim")
	if err := os.Symlink(bin, shimPath); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Options{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go a.Serve(l)
	return shimPath, sock
}

func claudeStyle(shimPath, cmd string) *exec.Cmd {
	// bash -c -l "'<prefix>' '<cmd>'" — what Claude Code runs.
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	return exec.Command("bash", "-c", q(shimPath)+" "+q(cmd))
}

func TestStreamsExitAndCwd(t *testing.T) {
	shimPath, sock := setup(t)
	work := t.TempDir()
	sub := filepath.Join(work, "sub")
	os.Mkdir(sub, 0o755)
	cwdFile := filepath.Join(t.TempDir(), "claude-1234-cwd")

	full := `source /nonexistent/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && eval 'cd sub && echo out && echo err >&2' < /dev/null && pwd -P >| ` + cwdFile
	cmd := claudeStyle(shimPath, full)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "RCC_SOCK=unix:"+sock, "RCC_TOKEN=tok")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v stderr=%s", err, se.String())
	}
	if so.String() != "out\n" || se.String() != "err\n" {
		t.Fatalf("stdout=%q stderr=%q", so.String(), se.String())
	}
	got, _ := os.ReadFile(cwdFile)
	want, _ := filepath.EvalSymlinks(sub)
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("cwd file = %q, want %q", got, want)
	}
}

func TestExitCodeAndStdin(t *testing.T) {
	shimPath, sock := setup(t)
	cmd := claudeStyle(shimPath, `read x; echo "got $x"; exit 7`)
	cmd.Env = append(os.Environ(), "RCC_SOCK="+sock, "RCC_TOKEN=tok")
	cmd.Stdin = strings.NewReader("hello\n")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("err = %v", err)
	}
	if string(out) != "got hello\n" {
		t.Fatalf("out = %q", out)
	}
}

// Commands must not inherit the user's terminal (it shows Claude's TUI):
// each runs as a session leader with no controlling tty.
func TestNoControllingTerminal(t *testing.T) {
	shimPath, sock := setup(t)
	cmd := claudeStyle(shimPath, `[ "$(ps -o sid= -p $$ | tr -d ' ')" = "$$" ] && echo leader; (exec 3</dev/tty) 2>/dev/null || echo no-tty`)
	cmd.Env = append(os.Environ(), "RCC_SOCK="+sock, "RCC_TOKEN=tok")
	out, err := cmd.Output()
	if err != nil || string(out) != "leader\nno-tty\n" {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

func TestBadToken(t *testing.T) {
	shimPath, sock := setup(t)
	cmd := claudeStyle(shimPath, "true")
	cmd.Env = append(os.Environ(), "RCC_SOCK="+sock, "RCC_TOKEN=wrong")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 126 || !strings.Contains(string(out), "bad token") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

// Killing the shim (as Claude Code does on timeout / Esc) must kill the
// remote process tree too.
func TestKillPropagates(t *testing.T) {
	shimPath, sock := setup(t)
	marker := filepath.Join(t.TempDir(), "survived")
	cmd := exec.Command(shimPath, "sleep 2; touch "+marker)
	cmd.Env = append(os.Environ(), "RCC_SOCK="+sock, "RCC_TOKEN=tok")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("remote command kept running after shim was killed")
	}
}

func TestSigtermForwarded(t *testing.T) {
	shimPath, sock := setup(t)
	cmd := exec.Command(shimPath, "sleep 30")
	cmd.Env = append(os.Environ(), "RCC_SOCK="+sock, "RCC_TOKEN=tok")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	if time.Since(start) > 3*time.Second {
		t.Fatal("SIGTERM was not forwarded")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 128+15 {
		t.Fatalf("err = %v", err)
	}
}
