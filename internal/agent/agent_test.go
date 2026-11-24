package agent

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"remote-cc/internal/pathmap"
	"remote-cc/internal/procjob"
	"remote-cc/internal/proto"
	"remote-cc/internal/shim"
)

// These tests run the agent the way rcc does (same shell lookup, job object
// on Windows) and talk to it the way the shim does. CI runs them on Linux,
// macOS and Windows (Git Bash).

func startAgent(t *testing.T) string {
	t.Helper()
	if err := procjob.KillOnExit(); err != nil {
		t.Logf("KillOnExit: %v", err)
	}
	a, err := New(Options{Token: "tok", TempDir: t.TempDir()})
	if err != nil {
		t.Skipf("no shell: %v", err)
	}
	t.Logf("shell: %s", a.Shell())
	if err := a.Check(t.TempDir()); err != nil {
		t.Fatalf("shell check: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go a.Serve(l)
	return l.Addr().String()
}

type result struct {
	out, errOut string
	exit        proto.Exit
	failure     string
}

func call(t *testing.T, addr string, st proto.Start) result {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	c := proto.NewConn(nc)
	st.Version, st.Token = proto.Version, "tok"
	if err := c.WriteJSON(proto.TStart, st); err != nil {
		t.Fatal(err)
	}
	c.Write(proto.TStdinEOF, nil)
	var r result
	var out, errOut strings.Builder
	for {
		f, err := c.Read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		switch f.Type {
		case proto.TStdout:
			out.Write(f.Payload)
		case proto.TStderr:
			errOut.Write(f.Payload)
		case proto.TError:
			r.failure = string(f.Payload)
			r.out, r.errOut = out.String(), errOut.String()
			return r
		case proto.TExit:
			json.Unmarshal(f.Payload, &r.exit)
			r.out, r.errOut = out.String(), errOut.String()
			return r
		}
	}
}

// claudeCmd wraps cmd the way Claude Code 2.1.3xx hands it to the shell
// prefix, then rewrites it the way the shim does.
func claudeCmd(cmd string) shim.Rewritten {
	full := `source /root/.claude/shell-snapshots/snapshot-bash-1-x.sh 2>/dev/null || true && { \builtin shopt -u extglob || \builtin setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL NO_BRACE_CCL NO_MAGIC_EQUAL_SUBST; } >/dev/null 2>&1 || true && { \builtin unalias -- 'which'; \builtin unset -f -- 'which'; } >/dev/null 2>&1 || true && eval '` +
		strings.ReplaceAll(cmd, `'`, `'\''`) + `' < /dev/null && pwd -P >| /tmp/claude-ab12-cwd`
	return shim.Rewrite(full)
}

func remoteDir(t *testing.T) (local, remote string) {
	d := t.TempDir()
	if l, err := filepath.EvalSymlinks(d); err == nil {
		d = l
	}
	return d, pathmap.ToRemote(runtime.GOOS, d)
}

func TestClaudeStyleCommand(t *testing.T) {
	addr := startAgent(t)
	local, rdir := remoteDir(t)
	os.Mkdir(filepath.Join(local, "sub"), 0o755)

	rw := claudeCmd(`echo hello; echo 中文输出; cd sub && echo err >&2`)
	r := call(t, addr, proto.Start{Cmd: rw.Cmd, Cwd: rdir, CaptureCwd: true})
	if r.failure != "" || r.exit.Code != 0 {
		t.Fatalf("failure=%q exit=%+v stdout=%q stderr=%q", r.failure, r.exit, r.out, r.errOut)
	}
	if !strings.Contains(r.out, "hello") || !strings.Contains(r.out, "中文输出") || !strings.Contains(r.errOut, "err") {
		t.Fatalf("stdout=%q stderr=%q", r.out, r.errOut)
	}
	if r.exit.Cwd != rdir+"/sub" {
		t.Fatalf("cwd %q, want %q", r.exit.Cwd, rdir+"/sub")
	}

	r = call(t, addr, proto.Start{Cmd: claudeCmd(`echo before; exit 3`).Cmd, Cwd: rdir, CaptureCwd: true})
	if r.exit.Code != 3 || !strings.Contains(r.out, "before") {
		t.Fatalf("exit=%+v stdout=%q stderr=%q", r.exit, r.out, r.errOut)
	}
}

func TestForwardedGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	addr := startAgent(t)
	local, rdir := remoteDir(t)
	git := func(args ...string) result {
		return call(t, addr, proto.Start{Cwd: rdir, Argv: append([]string{"git"}, args...),
			Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"}})
	}
	ok := func(r result) {
		t.Helper()
		if r.failure != "" || r.exit.Code != 0 {
			t.Fatalf("failure=%q exit=%+v stdout=%q stderr=%q", r.failure, r.exit, r.out, r.errOut)
		}
	}
	ok(git("init", "-q"))
	ok(git("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"))

	// Claude Code's style: hardening -c options, B-form absolute paths.
	wt := rdir + "/.claude/worktrees/w1"
	ok(git("-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=", "worktree", "add", "-q", "-b", "worktree-w1", wt, "HEAD"))
	if fi, err := os.Stat(filepath.Join(local, ".claude", "worktrees", "w1")); err != nil || !fi.IsDir() {
		t.Fatalf("worktree not created locally: %v", err)
	}
	r := git("worktree", "list", "--porcelain")
	ok(r)
	if !strings.Contains(r.out, "worktree "+wt+"\n") {
		t.Fatalf("worktree list in B's path form, want %q:\n%s", wt, r.out)
	}
	r = git("rev-parse", "--show-toplevel")
	ok(r)
	if top := strings.TrimSpace(r.out); !strings.HasPrefix(top, "/") || strings.Contains(top, ":") {
		t.Fatalf("toplevel %q is not in B's form", top)
	}
	if r := git("rev-parse", "--verify", "no-such-ref"); r.exit.Code == 0 {
		t.Fatal("git exit code lost")
	}
}
