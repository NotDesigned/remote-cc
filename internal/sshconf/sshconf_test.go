package sshconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInviteRoundTrip(t *testing.T) {
	iv := Invite{V: 1, Host: "srv.example.com", Port: 2222, User: "alice", Alias: "srv-cc", GwKey: "ssh-ed25519 AAAA", UserKey: "ssh-ed25519 BBBB", ID: "abc", Key: "-----BEGIN-----\n", Expires: "203001010000"}
	got, err := DecodeInvite("  " + iv.Encode() + "\n")
	if err != nil || *got != iv {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	for _, bad := range []string{"", "rcc1.", "rcc1.!!!", "xyz", "rcc1.e30"} {
		if _, err := DecodeInvite(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDefaultAlias(t *testing.T) {
	for host, want := range map[string]string{"server-la.example.com": "server-la-cc", "1.2.3.4": "cc", "2001:db8::1": "cc", "Box": "box-cc"} {
		if a := DefaultAlias(host); a != want {
			t.Errorf("DefaultAlias(%q) = %q, want %q", host, a, want)
		}
	}
}

func TestConfigureLookupRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host mine\n    HostName x\n"), 0o600)
	for _, s := range []Server{
		{Alias: "a-cc", Host: "a.example.com", Port: 2222, User: "alice"},
		{Alias: "b-cc", Host: "b.example.com", Port: 22, User: "bob", Jump: "root@b.example.com"},
	} {
		if err := Configure(s, "ssh-ed25519 GW", "ssh-ed25519 UK"); err != nil {
			t.Fatal(err)
		}
		got, err := Lookup(s.Alias)
		if err != nil || got != s {
			t.Fatalf("Lookup(%s) = %+v, %v; want %+v", s.Alias, got, err, s)
		}
	}
	// Re-configuring replaces the block instead of appending.
	Configure(Server{Alias: "a-cc", Host: "a.example.com", Port: 2223, User: "alice"}, "ssh-ed25519 GW", "ssh-ed25519 UK")
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "remote-cc", "config"))
	if strings.Count(string(b), "# remote-cc:a-cc begin") != 1 || !strings.Contains(string(b), "Port 2223") {
		t.Fatalf("config:\n%s", b)
	}
	main, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if !strings.HasPrefix(string(main), "Include remote-cc/config\n") || strings.Count(string(main), "Include") != 1 || !strings.Contains(string(main), "Host mine") {
		t.Fatalf("~/.ssh/config:\n%s", main)
	}
	kh, _ := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	for _, w := range []string{"[a.example.com]:2222 ssh-ed25519 GW", "rcc-alice@a.example.com ssh-ed25519 UK", "b.example.com ssh-ed25519 GW"} {
		if !strings.Contains(string(kh), w) {
			t.Errorf("known_hosts lacks %q", w)
		}
	}
}

func TestFingerprint(t *testing.T) {
	_, line, err := NewKey("c")
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint(line)
	if !strings.HasPrefix(fp, "SHA256:") || Fingerprint("restrict,command=\"x\" "+line) != fp {
		t.Fatalf("fingerprint %q", fp)
	}
}
