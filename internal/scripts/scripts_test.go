package scripts

import (
	"io/fs"
	"strings"
	"testing"
)

func TestAllScriptsLoad(t *testing.T) {
	n := 0
	fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".sh") {
			if Get(p) == "" {
				t.Errorf("%s is empty", p)
			}
			n++
		}
		return nil
	})
	if n < 20 {
		t.Fatalf("only %d scripts embedded", n)
	}
	if h := Host("server/init.sh"); !strings.HasPrefix(strings.TrimSpace(h), "# shellcheck") || !strings.Contains(h, "owned_by()") {
		t.Fatal("Host() must prepend lib/root.sh and lib/docker.sh")
	}
}
