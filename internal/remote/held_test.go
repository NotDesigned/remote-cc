//go:build !windows

package remote

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSSH is an ssh stand-in that runs the remote command with sh here.
func fakeSSH(t *testing.T) Target {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nfor a; do last=$a; done\nexec sh -c \"$last\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Target{SSH: p, Dest: "test"}
}

// A shared connection that is up but dead: the probe through it gets no
// answer within MasterWait; the shared connection is retired ("-O stop":
// sessions riding it go on, new calls connect afresh) and the probe runs on
// a new one.
func TestDeadMaster(t *testing.T) {
	tg := fakeSSH(t)
	dir := t.TempDir()
	// "-O check" says the master is up; until "-O stop", commands hang.
	script := `#!/bin/sh
for a; do last=$a; case $a in check) exit 0 ;; stop) touch "` + dir + `/retired"; exit 0 ;; esac; done
# like ssh, which hands its stdout to the shared connection's process
[ -e "` + dir + `/retired" ] || { sleep 30 & exec sleep 30; }
exec sh -c "$last"
`
	if err := os.WriteFile(tg.SSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := controlPathName(tg)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, nil, 0o600) // the socket
	c, err := Open(tg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.masterUp {
		t.Fatal("the shared connection was not seen")
	}
	defer func(w time.Duration) { MasterWait = w }(MasterWait)
	MasterWait = 300 * time.Millisecond
	start := time.Now()
	f, h, err := c.FactsHeld("echo a=1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if f["a"] != "1" || time.Since(start) > 5*time.Second {
		t.Fatalf("facts %v after %v", f, time.Since(start))
	}
	if _, err := os.Stat(filepath.Join(dir, "retired")); err != nil {
		t.Fatal("the shared connection was not retired")
	}
}

// The probe's ssh session waits after the facts and runs the next script
// with the rest of stdin as its own.
func TestFactsHeld(t *testing.T) {
	c, err := Open(fakeSSH(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, h, err := c.FactsHeld("echo a=1\necho junk line\necho b=two")
	if err != nil {
		t.Fatal(err)
	}
	if f["a"] != "1" || f["b"] != "two" || len(f) != 2 {
		t.Fatalf("facts %v", f)
	}
	var stderr strings.Builder
	p, err := h.Start("echo started; echo to-stderr >&2; IFS= read -r l; echo \"got $l\"; cat", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Start("echo again", io.Discard); err == nil {
		t.Fatal("a held session started twice")
	}
	io.WriteString(p, "line one\nrest\n")
	p.in.Close()
	out, _ := io.ReadAll(p)
	if string(out) != "started\ngot line one\nrest\n" {
		t.Fatalf("got %q", out)
	}
	<-p.done
	if stderr.String() != "to-stderr\n" {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// An unused held session ends when closed; a probe that fails is an error.
func TestFactsHeldClose(t *testing.T) {
	c, err := Open(fakeSSH(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, h, err := c.FactsHeld("echo a=1")
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	select {
	case <-h.p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the held session did not end")
	}
	if _, _, err := c.FactsHeld("echo a=1; exit 3"); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("got %v", err)
	}
	// Output after the facts goes nowhere until a script starts.
	_, h, err = c.FactsHeld("echo a=1")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := h.Start("echo x", io.Discard)
	if l, _ := bufio.NewReader(p).ReadString('\n'); l != "x\n" {
		t.Fatalf("got %q", l)
	}
	p.Close()
}

// A long script goes gzipped when the server has gzip.
func TestFactsHeldGzip(t *testing.T) {
	c, err := Open(fakeSSH(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, h, err := c.FactsHeld("echo bin_gzip=/bin/gzip")
	if err != nil {
		t.Fatal(err)
	}
	if !h.gzip {
		t.Fatal("gzip not noticed")
	}
	script := "# " + strings.Repeat("padding ", 1000) + "\necho long; cat"
	p, err := h.Start(script, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(p, "rest\n")
	p.in.Close()
	if out, _ := io.ReadAll(p); string(out) != "long\nrest\n" {
		t.Fatalf("got %q", out)
	}
}
