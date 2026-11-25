package cli

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"remote-cc/internal/client"
	"remote-cc/internal/rcb"
)

func find(t *testing.T, args ...string) *command {
	t.Helper()
	c, _, rest, noun := lookup(args)
	if c == nil || noun != "" || len(rest) != 0 {
		t.Fatalf("lookup %q: %v %q %q", args, c, rest, noun)
	}
	return c
}

func TestParse(t *testing.T) {
	c := find(t, "server", "user", "add")
	in, err := parse(c, []string{"-vvv", "-p", "22", "--alias", "w", "--key", "a", "-6", "host", "--key", "b", "dir"})
	if err != nil {
		t.Fatal(err)
	}
	if !in.target.Verbose || !reflect.DeepEqual(in.target.Args, []string{"-o", "LogLevel=DEBUG2", "-p", "22", "-6"}) {
		t.Fatalf("target %+v", in.target)
	}
	if in.s("--alias") != "w" || !reflect.DeepEqual(in.l("--key"), []string{"a", "b"}) || !reflect.DeepEqual(in.pos, []string{"host", "dir"}) {
		t.Fatalf("flags %+v", in)
	}

	s := commands[0]
	in, err = parse(s, []string{"--account", "w", "h", "--no-prompt", "--", "--resume", "-x"})
	if err != nil || in.s("--account") != "w" || !in.b("--no-prompt") || !reflect.DeepEqual(in.after, []string{"--resume", "-x"}) {
		t.Fatalf("session: %+v %v", in, err)
	}
	for _, bad := range [][]string{{"--bogus"}, {"-p"}, {"--account"}, {"--purge"}} {
		if _, err := parse(s, bad); err == nil {
			t.Errorf("session accepted %q", bad)
		}
	}
	if in, _ := parse(s, []string{"-v"}); !in.target.Verbose || len(in.target.Args) != 0 {
		t.Fatalf("-v: %+v", in.target)
	}
	if _, err := parse(s, []string{"h", "--help"}); err != errHelp {
		t.Fatalf("--help: %v", err)
	}
}

// Options a command does not take are refused, not silently dropped.
func TestOwnOptionsOnly(t *testing.T) {
	for _, tc := range []struct{ args, line []string }{
		{[]string{"server", "remove"}, []string{"--install-docker", "h"}},
		{[]string{"server", "user", "list"}, []string{"--key", "k", "h"}},
		{[]string{"device", "list"}, []string{"-p", "2222", "h"}}, // device commands use the ssh alias only
		{[]string{"device", "join"}, []string{"-v", "x"}},
		{[]string{"version"}, []string{"--bogus"}},
	} {
		if _, err := parse(find(t, tc.args...), tc.line); err == nil {
			t.Errorf("%q accepted %q", tc.args, tc.line)
		}
	}
}

// serve's -q is its own, not ssh's.
func TestServeQuiet(t *testing.T) {
	in, err := parse(find(t, "serve"), []string{"--listen", "127.0.0.1:0", "-q"})
	if err != nil || !in.b("-q") || len(in.target.Args) != 0 {
		t.Fatalf("%+v %v", in, err)
	}
}

func TestLookup(t *testing.T) {
	for args, want := range map[string]string{
		"server user add root@h alice": "server user add", "device join inv": "device join", "server update --all": "server update", "server list": "server list",
		"account login cc": "account login", "account list cc": "account list", "rcb use p": "rcb use", "server egress h": "server egress",
	} {
		c, name, _, _ := lookup(strings.Fields(args))
		if c == nil || c.name != want || !strings.HasPrefix(args, name) {
			t.Errorf("%q: %v %q", args, c, name)
		}
	}
	for _, noun := range []string{"server user", "server", "device", "rcb", "account"} {
		if c, _, _, n := lookup(append(strings.Fields(noun), "bogus")); c != nil || n != noun {
			t.Errorf("%s bogus: %v %q", noun, c, n)
		}
	}
	// Earlier names are gone, and the new ones they point to exist.
	for old, now := range renamed {
		if c, _, _, _ := lookup([]string{old, "h"}); c != nil {
			t.Errorf("%s still runs %q", old, c.name)
		}
		if c, _, rest, n := lookup(strings.Fields(now)); (c == nil || len(rest) != 0) && n != now {
			t.Errorf("%s → %q: no such command", old, now)
		}
	}
	// A host is a session.
	if c, _, _, n := lookup([]string{"myserver-cc", "."}); c != nil || n != "" {
		t.Errorf("host: %v %q", c, n)
	}
}

// The table is consistent: options are declared, names are unique, and the
// synopsis or help mentions every option a command takes.
func TestCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands {
		for _, n := range []string{c.name} {
			if seen[n] {
				t.Errorf("command %q twice", n)
			}
			seen[n] = true
		}
		if c.run == nil {
			t.Errorf("%q has no run", c.name)
		}
		for _, f := range c.flags {
			if flagKinds[f] == 0 {
				t.Errorf("%q: undeclared option %s", c.name, f)
			}
			if !regexp.MustCompile(regexp.QuoteMeta(f) + `([^a-z-]|$)`).MatchString(c.args + "\n" + c.help) {
				t.Errorf("%q: %s is not in its synopsis or help", c.name, f)
			}
		}
	}
}

func TestPinAccount(t *testing.T) {
	t.Setenv("RCC_ACCOUNT", "work")
	p := &rcb.Profile{ID: "p-1"}
	// --rcb: the profile's account, RCC_ACCOUNT ignored.
	c := client.Config{RCB: p}
	if err := pinAccount(&c); err != nil || c.Account != "rcb-p-1" {
		t.Errorf("rcb: %q %v", c.Account, err)
	}
	c = client.Config{RCB: p, Account: "rcb-p-1"}
	if err := pinAccount(&c); err != nil || c.Account != "rcb-p-1" {
		t.Errorf("rcb, same --account: %q %v", c.Account, err)
	}
	c = client.Config{RCB: p, Account: "default"}
	if err := pinAccount(&c); err == nil {
		t.Error("rcb with another --account accepted")
	}
	// Without --rcb: --account, else RCC_ACCOUNT.
	c = client.Config{}
	if pinAccount(&c); c.Account != "work" {
		t.Errorf("env: %q", c.Account)
	}
	c = client.Config{Account: "home"}
	if pinAccount(&c); c.Account != "home" {
		t.Errorf("flag: %q", c.Account)
	}
}
