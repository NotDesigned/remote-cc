package agent

import (
	"bytes"
	"testing"
)

func TestToPosixDrives(t *testing.T) {
	cases := map[string]string{
		"C:/Users/x/proj\n":                   "/c/Users/x/proj\n",
		"worktree D:/a/b\x00HEAD 123\x00":     "worktree /d/a/b\x00HEAD 123\x00",
		"gitdir: C:/p/.git/worktrees/w":       "gitdir: /c/p/.git/worktrees/w",
		"see http://x and abc:/no and 'E:/q'": "see http://x and abc:/no and '/e/q'",
		"--path=C:/a":                         "--path=/c/a",
		"no drive here: C:\\back\\slash":      "no drive here: C:\\back\\slash",
	}
	for in, want := range cases {
		if got := string(toPosixDrives([]byte(in))); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestDriveWriterSplitsChunks(t *testing.T) {
	var out bytes.Buffer
	d := &driveWriter{w: &out}
	for _, chunk := range []string{"first C", ":/a\nsecond ", "D:/b", "\nlast E:/c"} {
		d.Write([]byte(chunk))
	}
	d.Flush()
	if got := out.String(); got != "first /c/a\nsecond /d/b\nlast /e/c" {
		t.Fatalf("%q", got)
	}
}

func TestLocalArgs(t *testing.T) {
	in := []string{"-c", "core.hooksPath=/dev/null", "worktree", "add", "/e/Work/p/.claude/worktrees/w", "--git-dir=/c/x/.git", "/d", "HEAD:/a", "/etc"}
	want := []string{"-c", "core.hooksPath=/dev/null", "worktree", "add", "E:/Work/p/.claude/worktrees/w", "--git-dir=C:/x/.git", "D:/", "HEAD:/a", "/etc"}
	got := localArgs(in)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d: %q, want %q", i, got[i], want[i])
		}
	}
}
