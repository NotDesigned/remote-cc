package session

import (
	"strings"
	"testing"
)

func TestClaudeMissingMessage(t *testing.T) {
	host := &session{cfg: Config{Destination: "cc"}, facts: map[string]string{"hostname": "vps-1"}}
	if m := host.claudeMissing().Error(); !strings.Contains(m, "Docker host, not the remote-cc container") || !strings.Contains(m, "Port 2222") || !strings.Contains(m, `"vps-1"`) {
		t.Fatalf("host case:\n%s", m)
	}
	ctr := &session{cfg: Config{Destination: "cc"}, facts: map[string]string{"rccserver": "1"}}
	if m := ctr.claudeMissing().Error(); !strings.Contains(m, "inside the remote-cc container") {
		t.Fatalf("container case:\n%s", m)
	}
}
