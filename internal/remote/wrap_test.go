package remote

import (
	"os/exec"
	"strings"
	"testing"

	"remote-cc/deploy"
	"remote-cc/internal/scripts"
)

// Long scripts go compressed and still run; the admin scripts stay well
// under Windows' 32767-character command line limit.
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
	for _, name := range []string{"server/update.sh", "server/migrate.sh", "server/egress.sh", "server/init.sh", "server/user-add.sh"} {
		params := Vars("COMPOSE_GW_B64", Base64(deploy.ComposeGateway), "COMPOSE_USER_B64", Base64(deploy.ComposeUser),
			"COMPOSE_EGRESS_B64", Base64(deploy.ComposeEgress))
		body := scripts.Host(name)
		full := params + Vars("SELF_B64", Base64(params+body)) + body // as Elevated sends it
		if n := len(wrap(full)); n > 24000 {
			t.Errorf("%s: %d characters on the command line", name, n)
		}
	}
}
