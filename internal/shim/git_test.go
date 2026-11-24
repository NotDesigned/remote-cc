package shim

import (
	"reflect"
	"testing"
)

func TestGitTarget(t *testing.T) {
	const root = "/home/u/proj"
	cases := []struct {
		cwd    string
		args   []string
		dir    string
		rest   []string
		inRoot bool
	}{
		{"/home/u/proj", []string{"status", "--porcelain"}, "/home/u/proj", []string{"status", "--porcelain"}, true},
		{"/home/u/proj/sub", []string{"rev-parse", "--show-toplevel"}, "/home/u/proj/sub", []string{"rev-parse", "--show-toplevel"}, true},
		{"/root", []string{"-C", "/home/u/proj", "worktree", "add", "x"}, "/home/u/proj", []string{"worktree", "add", "x"}, true},
		{"/home/u/proj", []string{"-c", "core.quotepath=false", "-C", "sub", "log", "-C"}, "/home/u/proj/sub", []string{"-c", "core.quotepath=false", "log", "-C"}, true},
		{"/root/.claude/plugins", []string{"clone", "https://x"}, "", nil, false},
		{"/home/u/proj", []string{"-C", "../other", "status"}, "", nil, false},
		{"/home/u/projX", []string{"status"}, "", nil, false},
		{"/home/u/proj", []string{"--git-dir=/elsewhere/.git", "status"}, "", nil, false},
		{"/home/u/proj", []string{"--no-pager", "diff"}, "/home/u/proj", []string{"--no-pager", "diff"}, true},
	}
	for _, c := range cases {
		dir, rest, in := gitTarget(c.cwd, root, c.args)
		if in != c.inRoot {
			t.Errorf("%v in %s: inRoot=%v", c.args, c.cwd, in)
			continue
		}
		if in && (dir != c.dir || !reflect.DeepEqual(rest, c.rest)) {
			t.Errorf("%v in %s: dir=%q rest=%q", c.args, c.cwd, dir, rest)
		}
	}
	if _, _, in := gitTarget("/home/u/proj", "", []string{"status"}); in {
		t.Error("no root: must stay local")
	}
}

func TestGitEnv(t *testing.T) {
	got := gitEnv([]string{"GIT_TERMINAL_PROMPT=0", "PATH=/b", "HOME=/root", "LC_ALL=C", "GIT_EXEC_PATH=/usr/lib/git-core"})
	want := map[string]string{"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}
