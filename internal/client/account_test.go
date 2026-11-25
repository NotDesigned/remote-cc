package client

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"remote-cc/internal/netexec"
	"remote-cc/internal/rcb"
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

func TestResolveAccountRCB(t *testing.T) {
	facts := map[string]string{"home": "/root", "cfgdir": "/root/.claude"}
	p := &rcb.Profile{ID: "p-1", Node: &rcb.Node{ID: "n"}}
	s := &session{cfg: Config{RCB: p}, facts: facts, conn: &remote.Conn{}}
	if err := s.resolveAccount(); err != nil || s.cfg.Account != "rcb-p-1" || !strings.HasSuffix(s.acct, "/accounts/rcb-p-1") {
		t.Fatalf("rcb: %q %q %v", s.cfg.Account, s.acct, err)
	}
	s = &session{cfg: Config{RCB: p, Account: "work"}, facts: facts, conn: &remote.Conn{}}
	if err := s.resolveAccount(); err == nil {
		t.Fatal("--rcb with another account accepted")
	}
	s = &session{cfg: Config{Account: "rcb-p-1"}, facts: facts, conn: &remote.Conn{}}
	if err := s.resolveAccount(); err == nil {
		t.Fatal("rcb account without --rcb accepted")
	}
}

// fakeRCB answers grants (g1, g2, …) and exit checks (exits in order; "" = fail).
func fakeRCB(t *testing.T, exits ...string) (*rcb.Link, *[]string) {
	var calls []string
	grants, checks := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/remote-cc/v1/profiles/p-1/grant":
			grants++
			calls = append(calls, fmt.Sprintf("grant%d", grants))
			fmt.Fprintf(w, `{"grant":"g%d"}`, grants)
		case "/api/remote-cc/v1/profiles/p-1/check":
			calls = append(calls, "check")
			ip := exits[min(checks, len(exits)-1)]
			checks++
			if ip == "" {
				w.WriteHeader(502)
				return
			}
			fmt.Fprintf(w, `{"exit":{"ip":%q,"timeZone":"Asia/Tokyo"}}`, ip)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &rcb.Link{URL: srv.URL, Token: "rcck_x"}, &calls
}

func TestRCBRun(t *testing.T) {
	profile := func(exit *rcb.Exit) *rcb.Profile {
		return &rcb.Profile{ID: "p-1", Name: "Work", Node: &rcb.Node{ID: "n", Name: "LA"}, Exit: exit}
	}
	run := func(p *rcb.Profile, l *rcb.Link, codes ...int) (int, error, []string) {
		s := &session{cfg: Config{RCB: p, RCBLink: l}}
		var got []string
		code, err := s.rcbRun(func(grant string) (int, error) {
			got = append(got, grant)
			return codes[min(len(got)-1, len(codes)-1)], nil
		})
		return code, err, got
	}

	// No recorded exit: checked before running, and the result used.
	l, calls := fakeRCB(t, "198.51.100.1")
	p := profile(nil)
	if code, err, got := run(p, l, 0); code != 0 || err != nil || strings.Join(got, ",") != "g1" ||
		p.Exit == nil || p.Exit.IP != "198.51.100.1" || strings.Join(*calls, ",") != "check,grant1" {
		t.Errorf("no exit: %d %v %v %v", code, err, got, *calls)
	}
	// A recorded exit without a time zone is checked too.
	l, calls = fakeRCB(t, "198.51.100.1")
	if _, err, _ := run(profile(&rcb.Exit{IP: "198.51.100.1"}), l, 0); err != nil || (*calls)[0] != "check" {
		t.Errorf("no time zone: %v %v", err, *calls)
	}
	// The check fails: refuse, run nothing.
	l, _ = fakeRCB(t, "")
	if code, err, got := run(profile(nil), l, 0); err == nil || code == 0 || len(got) != 0 || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("failed check: %d %v %v", code, err, got)
	}

	recorded := &rcb.Exit{IP: "203.0.113.7", TimeZone: "America/Los_Angeles"}
	// Exit moved: re-check, run again once with a fresh grant.
	l, calls = fakeRCB(t, "198.51.100.1")
	p = profile(recorded)
	if code, err, got := run(p, l, netexec.ExitMismatch, 0); code != 0 || err != nil || strings.Join(got, ",") != "g1,g2" ||
		p.Exit.IP != "198.51.100.1" || strings.Join(*calls, ",") != "grant1,check,grant2" {
		t.Errorf("moved: %d %v %v %v", code, err, got, *calls)
	}
	// Still the recorded exit: give up.
	l, _ = fakeRCB(t, "203.0.113.7")
	if code, err, got := run(profile(&rcb.Exit{IP: "203.0.113.7", TimeZone: "UTC"}), l, netexec.ExitMismatch); code != netexec.ExitMismatch || err == nil || len(got) != 1 {
		t.Errorf("same exit: %d %v %v", code, err, got)
	}
	// Mismatch twice: only one retry.
	l, _ = fakeRCB(t, "198.51.100.1", "192.0.2.1")
	if code, _, got := run(profile(&rcb.Exit{IP: "203.0.113.7", TimeZone: "UTC"}), l, netexec.ExitMismatch); code != netexec.ExitMismatch || len(got) != 2 {
		t.Errorf("retried more than once: %d %v", code, got)
	}
	// Not linked.
	if _, err, _ := run(profile(recorded), nil, 0); err == nil {
		t.Error("ran without a link")
	}
}

func TestStatusEmail(t *testing.T) {
	for out, want := range map[string]string{
		`{"loggedIn":true,"email":"a@example.com"}`:                         "a@example.com",
		"rcc: network via node LA\n{\"loggedIn\":true,\"email\":\"b@x\"}\n": "b@x",
		`{"loggedIn":true,"authMethod":"oauth_token"}`:                      "", // stored setup-token
	} {
		if got, ok := statusEmail([]byte(out)); !ok || got != want {
			t.Errorf("%q: %q %v", out, got, ok)
		}
	}
	if _, ok := statusEmail([]byte("error: not found")); ok {
		t.Error("no status read as one")
	}
}
