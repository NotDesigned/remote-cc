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
