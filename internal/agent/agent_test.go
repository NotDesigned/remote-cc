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

func TestForwardedRg(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("no rg")
	}
	addr := startAgent(t)
	local, rdir := remoteDir(t)
	os.MkdirAll(filepath.Join(local, "sub", "deep"), 0o755)
	os.WriteFile(filepath.Join(local, "sub", "deep", "a.go"), []byte("package a\nfunc Hello() {}\n"), 0o644)
	os.WriteFile(filepath.Join(local, "b.txt"), []byte("nothing\n"), 0o644)
	// A config that would change the output Claude Code parses.
	cfg := filepath.Join(t.TempDir(), "rgrc")
	os.WriteFile(cfg, []byte("--vimgrep\n"), 0o644)
	t.Setenv("RIPGREP_CONFIG_PATH", cfg)
	rg := func(dir string, paths []int, args ...string) result {
		return call(t, addr, proto.Start{Cwd: dir, Argv: append([]string{"rg"}, args...), Paths: paths})
	}

	// Grep in Claude Code's relative form: cwd is the search directory.
	r := rg(rdir, []int{7}, "--hidden", "--glob", "!.svn", "--max-columns", "500", "-l", "Hello", ".")
	if r.failure != "" || r.exit.Code != 0 || r.out != "./sub/deep/a.go\n" {
		t.Fatalf("failure=%q exit=%+v stdout=%q stderr=%q", r.failure, r.exit, r.out, r.errOut)
	}
	// Absolute targets come back in B's form, with / between parts.
	r = rg("/", []int{4}, "--files", "--null", "--glob", "*.go", rdir)
	if want := rdir + "/sub/deep/a.go\x00"; r.exit.Code != 0 || r.out != want {
		t.Fatalf("exit=%+v stdout=%q, want %q (stderr %q)", r.exit, r.out, want, r.errOut)
	}
	r = rg(rdir, []int{2}, "--json", "Hello", rdir+"/sub")
	if r.exit.Code != 0 || !strings.Contains(r.out, `"text":"`+rdir+`/sub/deep/a.go"`) {
		t.Fatalf("exit=%+v stdout=%q", r.exit, r.out)
	}
	// No match: exit code 1, as rg's.
	if r = rg(rdir, []int{2}, "-l", "nomatch", "."); r.exit.Code != 1 || r.out != "" {
		t.Fatalf("exit=%+v stdout=%q", r.exit, r.out)
	}

	// No rg here: the agent says so, and the shim searches on the server.
	t.Setenv("PATH", "")
	if r = rg(rdir, []int{2}, "-l", "Hello", "."); r.failure != "rcc: "+proto.RgMissing {
		t.Fatalf("failure=%q exit=%+v", r.failure, r.exit)
	}
}

func TestRgLocalArgs(t *testing.T) {
	got := rgLocalArgs([]string{"--glob", "/c/x", "/d/pat", "/c/Users/p", "/e"}, []int{3, 4})
	want := []string{"--glob", "/c/x", "/d/pat", "C:/Users/p", "E:/"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%q", got)
	}
}

func TestRgJSONPathsOnWindows(t *testing.T) {
	var out strings.Builder
	d := &driveWriter{w: &out, json: true}
	d.Write([]byte(`{"type":"match","data":{"path":{"text":"C:\\Users\\x\\sub\\a.go"},"lines":{"text":"s := \"a\\\\b\"\n"}}}` + "\n"))
	d.Write([]byte(`{"type":"begin","data":{"path":{"text":".\\sub\\b.go"}}}` + "\n"))
	d.Flush()
	want := `{"type":"match","data":{"path":{"text":"/c/Users/x/sub/a.go"},"lines":{"text":"s := \"a\\\\b\"\n"}}}` + "\n" +
		`{"type":"begin","data":{"path":{"text":"./sub/b.go"}}}` + "\n"
	if out.String() != want {
		t.Fatalf("%s", out.String())
	}
}
