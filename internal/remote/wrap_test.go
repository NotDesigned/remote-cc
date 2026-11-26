package remote

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"remote-cc/deploy"
	"remote-cc/internal/scripts"
)

// Long scripts go compressed and still run.
func TestWrapLongScript(t *testing.T) {
	short := "echo short-ok"
	if w := wrap(short); strings.Contains(w, "gzip") {
		t.Fatalf("short script compressed: %s", w)
	}
	long := "x='" + strings.Repeat("a", 40000) + "'\necho long-ok ${#x}"
	w := wrap(long)
	if !strings.Contains(w, "gzip -dc") || len(w) > maxPlainCommand {
		t.Fatalf("long script not compressed (%d chars)", len(w))
	}
	for script, want := range map[string]string{short: "short-ok", long: "long-ok 40000"} {
		out, err := exec.Command("sh", "-c", wrap(script)).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Fatalf("ran %q: %v", out, err)
		}
	}
}

// Admin scripts go on stdin (no command line limit), ahead of their input,
// which they read intact.
func TestStdinLoader(t *testing.T) {
	params := Vars("COMPOSE_GW_B64", Base64(deploy.ComposeGateway), "COMPOSE_USER_B64", Base64(deploy.ComposeUser),
		"COMPOSE_EGRESS_B64", Base64(deploy.ComposeEgress))
	body := scripts.Host("server/update.sh")
	full := params + Vars("SELF_B64", Base64(params+body)) + body // as Elevated sends it
	// A stand-in of the same size (the real one needs docker).
	script := "x='" + strings.Repeat("a", len(full)) + "'\nIFS= read -r tok; echo \"${#x} $tok\"; cat\n"
	cmd := exec.Command("sh", "-c", stdinLoader)
	cmd.Stdin = strings.NewReader(gzipBase64(script) + "\nrcch_token\nmore\n")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != strconv.Itoa(len(full))+" rcch_token\nmore\n" {
		t.Fatalf("got %q %v", out, err)
	}
}
