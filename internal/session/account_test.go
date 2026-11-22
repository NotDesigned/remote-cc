package session

import (
	"strings"
	"testing"
)

func TestResolveAccountDefaultsToMachine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	machine, err := MachineProfile()
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{"home": "/root", "cfgdir": "/root/.claude"}

	s := &session{cfg: Config{}, facts: facts, logf: func(string, ...any) {}}
	if err := s.resolveAccount(); err != nil {
		t.Fatal(err)
	}
	if !s.autoAccount || s.cfg.Account != machine || s.ccfg != "/root/.local/share/rcc/accounts/"+machine {
		t.Fatalf("default: auto=%v account=%q ccfg=%q", s.autoAccount, s.cfg.Account, s.ccfg)
	}

	for _, shared := range []string{"default", "shared"} {
		s = &session{cfg: Config{Account: shared}, facts: facts, logf: func(string, ...any) {}}
		s.resolveAccount()
		if s.acct != "" || s.ccfg != "/root/.claude" || s.autoAccount {
			t.Fatalf("%s: acct=%q ccfg=%q", shared, s.acct, s.ccfg)
		}
	}

	s = &session{cfg: Config{Account: "work"}, facts: facts, logf: func(string, ...any) {}}
	s.resolveAccount()
	if s.autoAccount || !strings.HasSuffix(s.acct, "/accounts/work") {
		t.Fatalf("named: acct=%q", s.acct)
	}
}
