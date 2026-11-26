package shim

import (
	"reflect"
	"testing"
)

func TestRgTarget(t *testing.T) {
	const root = "/home/u/proj"
	// Claude Code 2.1.3xx's own argument lists (see shim.Rg): Grep and Glob
	// run in the search directory (opened as /proc/self/fd/N, which getcwd
	// resolves) with the target ".", or with an absolute target.
	grep := []string{"--hidden", "--glob", "!.git", "--glob", "!.svn", "--max-columns", "500", "-l", "--null",
		"func main", "--glob", "*.go", "--iglob", "!**/secret/**", "."}
	glob := []string{"--files", "--null", "--glob", "**/*.md", "--sort=modified", "--no-ignore", "--hidden", "--iglob", "!/x", "."}
	content := []string{"--hidden", "--max-columns", "500", "--json", "-n", "-C", "2", "-e", "-flag", "--type", "go", "/home/u/proj/sub"}
	index := []string{"--files", "--follow", "--hidden", "--glob", "!.git/", "/home/u/proj"}
	cases := []struct {
		name  string
		cwd   string
		args  []string
		dir   string
		paths []int
		ok    bool
	}{
		{"grep in project", root, grep, root, []int{14}, true},
		{"grep in subdir", root + "/a/b", grep, root + "/a/b", []int{14}, true},
		{"glob", root, glob, root, []int{9}, true},
		{"content, -e pattern", root, content, root, []int{11}, true},
		{"file index, cwd outside", "/", index, root, []int{5}, true},
		{"cwd outside, relative", "/root", grep, "", nil, false},
		{"path outside", root, []string{"-l", "x", "/etc"}, "", nil, false},
		{"mixed paths", root, []string{"-l", "x", "a", "/root/.claude"}, "", nil, false},
		{"two paths inside", root, []string{"-l", "x", "a", root + "/b"}, root, []int{2, 3}, true},
		{"sibling with same prefix", root, []string{"x", "/home/u/projX"}, "", nil, false},
		{"escapes with ..", root, []string{"x", "../other"}, "", nil, false},
		{"single file via fd", root, []string{"-n", "x", "/proc/self/fd/3"}, "", nil, false},
		{"stdin", root, []string{"x", "-"}, "", nil, false},
		{"no path", root, []string{"-l", "x"}, "", nil, false},
		{"only a pattern", root, []string{"--files-with-matches", "x"}, "", nil, false},
		{"pattern file", root, []string{"-f", "pats", "."}, "", nil, false},
		{"pattern file long", root, []string{"--file=pats", "."}, "", nil, false},
		{"preprocessor", root, []string{"--pre", "cat", "x", "."}, "", nil, false},
		{"ignore file", root, []string{"--ignore-file", "/root/.ignore", "x", "."}, "", nil, false},
		{"version", root, []string{"--version"}, "", nil, false},
		{"attached short value", root, []string{"-C3", "-tgo", "x", "."}, root, []int{3}, true},
		{"cluster ending in value", root, []string{"-inC", "3", "x", "."}, root, []int{3}, true},
		{"regexp=", root, []string{"--regexp=x", "a", "b"}, root, []int{1, 2}, true},
		{"value that looks like a path", root, []string{"--glob", "/etc", "x", "."}, root, []int{3}, true},
		{"double dash", root, []string{"-l", "--", "-pat", "-dir"}, root, []int{3}, true},
		{"files with no path", root, []string{"--files"}, "", nil, false},
		{"pattern after path-like flag order", root, []string{".", "-e", "x"}, root, []int{0}, true},
	}
	for _, c := range cases {
		dir, paths, ok := rgTarget(c.cwd, root, c.args)
		if ok != c.ok {
			t.Errorf("%s: ok=%v", c.name, ok)
			continue
		}
		if ok && (dir != c.dir || !reflect.DeepEqual(paths, c.paths)) {
			t.Errorf("%s: dir=%q paths=%v, want %q %v", c.name, dir, paths, c.dir, c.paths)
		}
	}
	if _, _, ok := rgTarget(root, "", grep); ok {
		t.Error("no root: must stay on B")
	}
}

func TestRgMissingFile(t *testing.T) {
	t.Setenv(EnvSock, "unix:/run/x/s")
	if f := rgMissingFile(); f != "/run/x/rg-missing" {
		t.Fatal(f)
	}
	t.Setenv(EnvSock, "tcp:127.0.0.1:1")
	if f := rgMissingFile(); f != "" {
		t.Fatal(f)
	}
}
