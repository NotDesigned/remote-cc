package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestUpdateIsSerialized(t *testing.T) {
	p := filepath.Join(t.TempDir(), "list")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := Update(p, 0o600, func(old []byte) ([]byte, error) {
				return append(old, fmt.Sprintf("%d\n", i)...), nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), "\n"); n != 20 {
		t.Fatalf("lost updates: %d of 20 lines\n%s", n, b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".list.tmp*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestWriteKeepsModeAndFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes and symlinks")
	}
	d := t.TempDir()
	real := filepath.Join(d, "dotfiles-config")
	os.WriteFile(real, []byte("old"), 0o640)
	link := filepath.Join(d, "config")
	os.Symlink(real, link)
	if err := Write(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	b, _ := os.ReadFile(real)
	fi, _ := os.Stat(real)
	if string(b) != "new" || fi.Mode().Perm() != 0o640 {
		t.Fatalf("got %q %v", b, fi.Mode())
	}
	if err := Write(filepath.Join(d, "fresh"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(d, "fresh")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v", fi.Mode())
	}
}

func TestUpdateErrorChangesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("keep"), 0o600)
	if err := Update(p, 0o600, func([]byte) ([]byte, error) { return nil, fmt.Errorf("no") }); err == nil {
		t.Fatal("want the error")
	}
	if b, _ := os.ReadFile(p); string(b) != "keep" {
		t.Fatalf("changed: %q", b)
	}
}
