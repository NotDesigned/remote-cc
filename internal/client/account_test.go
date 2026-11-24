package client

import (
	"strings"
	"testing"

	"remote-cc/internal/remote"
)

func TestResolveAccountDefaultsToMachine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	machine, err := MachineProfile()
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{"home": "/root", "cfgdir": "/root/.claude"}
	mk := func(account string) *session {
		return &session{cfg: Config{Account: account}, facts: facts, conn: &remote.Conn{}}
	}

	s := mk("")
	if err := s.resolveAccount(); err != nil {
		t.Fatal(err)
	}
	if !s.autoAccount || s.cfg.Account != machine || s.ccfg != "/root/.local/share/rcc/accounts/"+machine {
		t.Fatalf("default: auto=%v account=%q ccfg=%q", s.autoAccount, s.cfg.Account, s.ccfg)
	}
	for _, shared := range []string{"default", "shared"} {
		s = mk(shared)
		s.resolveAccount()
		if s.acct != "" || s.ccfg != "/root/.claude" || s.autoAccount {
			t.Fatalf("%s: acct=%q ccfg=%q", shared, s.acct, s.ccfg)
		}
	}
	s = mk("work")
	s.resolveAccount()
	if s.autoAccount || !strings.HasSuffix(s.acct, "/accounts/work") {
		t.Fatalf("named: acct=%q", s.acct)
	}
	if err := mk("../x").resolveAccount(); err == nil {
		t.Fatal("bad account name accepted")
	}
}

func TestClaudeMissingMessage(t *testing.T) {
	host := &session{cfg: Config{Target: remote.Target{Dest: "cc"}}, facts: map[string]string{"hostname": "vps-1"}}
	if m := host.claudeMissing().Error(); !strings.Contains(m, "Docker host of a remote-cc server") || !strings.Contains(m, `"vps-1"`) {
		t.Fatalf("host case:\n%s", m)
	}
	ctr := &session{facts: map[string]string{"rccserver": "1"}}
	if m := ctr.claudeMissing().Error(); !strings.Contains(m, "inside the remote-cc container") {
		t.Fatalf("container case:\n%s", m)
	}
}
