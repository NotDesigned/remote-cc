package server

import (
	"strings"
	"testing"
)

func TestPrintUpdatePending(t *testing.T) {
	var b strings.Builder
	printUpdate(&b, map[string]string{"gateway": "updated", "updated": "bob carol", "pending": "alice(2) dave(1)", "egress": "pending(3)"})
	out := b.String()
	for _, want := range []string{
		"gateway updated (running sessions reconnect",
		"updated: bob carol\n",
		"pending: alice has 2 active session(s); its container will update when its sessions end\n",
		"pending: dave has 1 active session(s)",
		"pending: the egress host has 3 --rcb session(s); it will update when they end\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	printUpdate(&b, map[string]string{"egress": "updated"})
	if b.String() != "egress host updated (new image or compose file)\n" {
		t.Errorf("egress updated: %q", b.String())
	}
}

func TestPrintUsersPending(t *testing.T) {
	var b strings.Builder
	printUsers(&b, "user=alice|running|2|1|10M|pending\nuser=bob|running|1|0|5M|-\negress=pending\n", "host")
	out := b.String()
	if !strings.Contains(out, "UPDATE") || !strings.Contains(out, "pending\n") {
		t.Errorf("no UPDATE column: %s", out)
	}
	if !strings.Contains(out, "gets the server update once its sessions end") || !strings.Contains(out, "egress host: an update waits") {
		t.Errorf("no pending notes: %s", out)
	}
	// An older server prints five fields.
	b.Reset()
	printUsers(&b, "user=alice|running|2|1|10M\n", "host")
	if strings.Contains(b.String(), "pending") || !strings.HasSuffix(b.String(), "10M   -\n") {
		t.Errorf("older server: %q", b.String())
	}
}
