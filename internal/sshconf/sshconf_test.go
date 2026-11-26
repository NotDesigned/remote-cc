package sshconf

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempHome points HOME (and the cache, for shared ssh connections) at a
// temp dir: tests never touch this machine's ~/.ssh.
func tempHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	return home
}

func hostKey(t *testing.T) string {
	_, line, err := NewKey("")
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func validInvite(t *testing.T) Invite {
	priv, _, err := NewKey("")
	if err != nil {
		t.Fatal(err)
	}
	return Invite{V: 1, Host: "srv.example.com", Port: 2222, User: "alice", Alias: "srv-cc", GwKey: hostKey(t), UserKey: hostKey(t),
		ID: "0123456789abcdef", Key: string(priv), Expires: "203001010000"}
}

func TestInviteRoundTrip(t *testing.T) {
	iv := validInvite(t)
	got, err := DecodeInvite("  " + iv.Encode() + "\n")
	if err != nil || *got != iv {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	for _, bad := range []string{"", "rcc1.", "rcc1.!!!", "xyz", "rcc1.e30"} {
		if _, err := DecodeInvite(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, h := range []string{"1.2.3.4", "2001:db8::1", "a-b.example.com.", "x_y.lan"} {
		iv := validInvite(t)
		iv.Host = h
		if _, err := DecodeInvite(iv.Encode()); err != nil {
			t.Errorf("host %q: %v", h, err)
		}
	}
}

// A crafted invite must not get anything into ~/.ssh/remote-cc/config,
// ~/.ssh/known_hosts or a file name.
func TestInviteRejectsInjection(t *testing.T) {
	gw := hostKey(t)
	cases := map[string]func(*Invite){
		"newline in host":       func(iv *Invite) { iv.Host = "srv.example.com\n    ProxyCommand touch /tmp/pwned" },
		"space in host":         func(iv *Invite) { iv.Host = "srv ProxyCommand=x" },
		"quote in host":         func(iv *Invite) { iv.Host = `srv"x` },
		"option as host":        func(iv *Invite) { iv.Host = "-oProxyCommand=x" },
		"ipv6 zone":             func(iv *Invite) { iv.Host = "fe80::1%eth0" },
		"port":                  func(iv *Invite) { iv.Port = 70000 },
		"negative port":         func(iv *Invite) { iv.Port = -1 },
		"newline in gw key":     func(iv *Invite) { iv.GwKey = gw + "\n@cert-authority * " + gw },
		"newline in user key":   func(iv *Invite) { iv.UserKey = gw + "\n* " + gw },
		"marker as key":         func(iv *Invite) { iv.GwKey = "@cert-authority * " + gw },
		"not a key":             func(iv *Invite) { iv.GwKey = "ssh-ed25519 AAAA" },
		"key type mismatch":     func(iv *Invite) { iv.UserKey = "ssh-rsa " + strings.Fields(gw)[1] },
		"path in id":            func(iv *Invite) { iv.ID = "../../../.bashrc" },
		"short id":              func(iv *Invite) { iv.ID = "abc" },
		"bad alias":             func(iv *Invite) { iv.Alias = "x\nHost *" },
		"bad user":              func(iv *Invite) { iv.User = "Root" },
		"private key garbage":   func(iv *Invite) { iv.Key = "-----BEGIN-----\n" },
		"two private keys":      func(iv *Invite) { iv.Key += iv.Key },
		"newline in expiry":     func(iv *Invite) { iv.Expires = "1\nx" },
		"private key not a key": func(iv *Invite) { iv.Key = "hello" },
	}
	for name, mod := range cases {
		iv := validInvite(t)
		mod(&iv)
		if _, err := DecodeInvite(iv.Encode()); err == nil {
			t.Errorf("%s: accepted", name)
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
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host mine\n    HostName x\n"), 0o600)
	gw, uk := hostKey(t), hostKey(t)
	for _, s := range []Server{
		{Alias: "a-cc", Host: "a.example.com", Port: 2222, User: "alice"},
		{Alias: "b-cc", Host: "b.example.com", Port: 22, User: "bob", Jump: "root@b.example.com"},
	} {
		if err := Configure(s, gw, uk); err != nil {
			t.Fatal(err)
		}
		got, err := Lookup(s.Alias)
		if err != nil || got != s {
			t.Fatalf("Lookup(%s) = %+v, %v; want %+v", s.Alias, got, err, s)
		}
	}
	// Re-configuring replaces the block instead of appending.
	if err := Configure(Server{Alias: "a-cc", Host: "a.example.com", Port: 2223, User: "alice"}, gw, uk); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "remote-cc", "config"))
	if strings.Count(string(b), "# remote-cc:a-cc begin") != 1 || !strings.Contains(string(b), "Port 2223") {
		t.Fatalf("config:\n%s", b)
	}
	main, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if !strings.HasPrefix(string(main), "Include remote-cc/config\n") || strings.Count(string(main), "Include") != 1 || !strings.Contains(string(main), "Host mine") {
		t.Fatalf("~/.ssh/config:\n%s", main)
	}
	kh, _ := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	for _, w := range []string{"[a.example.com]:2222 " + gw, "rcc-alice@a.example.com " + uk, "b.example.com " + gw} {
		if !strings.Contains(string(kh), w) {
			t.Errorf("known_hosts lacks %q", w)
		}
	}
}

func TestConfigureRejectsInjection(t *testing.T) {
	home := tempHome(t)
	gw := hostKey(t)
	for _, c := range []struct {
		s      Server
		gw, uk string
	}{
		{Server{Alias: "a", Host: "h\nProxyCommand x", Port: 22, User: "u"}, gw, gw},
		{Server{Alias: "a", Host: "h", Port: 22, User: "u", Jump: "x\n  ProxyCommand y"}, gw, gw},
		{Server{Alias: "a", Host: "h", Port: 22, User: "u", Jump: "-oProxyCommand=y"}, gw, gw},
		{Server{Alias: "a", Host: "h", Port: 22, User: "u"}, gw + "\n@cert-authority * " + gw, gw},
		{Server{Alias: "a", Host: "h", Port: 0, User: "u"}, gw, gw},
	} {
		if err := Configure(c.s, c.gw, c.uk); err == nil {
			t.Errorf("accepted %+v", c.s)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "known_hosts")); err == nil {
		t.Fatal("wrote known_hosts")
	}
}

// A newly pinned host key replaces older ones for the same name (plain or
// hashed); other hosts' entries stay.
func TestPinReplacesOldHostKeysAndForget(t *testing.T) {
	home := tempHome(t)
	old, gw, uk := hostKey(t), hostKey(t), hostKey(t)
	salt := []byte("0123456789abcdefghij")
	m := hmac.New(sha1.New, salt)
	m.Write([]byte("rcc-alice@a.example.com"))
	hashed := "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(m.Sum(nil))
	kh := filepath.Join(home, ".ssh", "known_hosts")
	os.WriteFile(kh, []byte("[a.example.com]:2222 "+old+"\n"+hashed+" "+old+"\nother.example.com "+old+"\n@cert-authority *.corp "+old+"\n"), 0o600)
	s := Server{Alias: "a-cc", Host: "a.example.com", Port: 2222, User: "alice"}
	if err := Configure(s, gw, uk); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(kh)
	want := "other.example.com " + old + "\n@cert-authority *.corp " + old + "\n[a.example.com]:2222 " + gw + "\nrcc-alice@a.example.com " + uk + "\n"
	if string(b) != want {
		t.Fatalf("known_hosts:\n%s\nwant:\n%s", b, want)
	}
	Configure(Server{Alias: "b-cc", Host: "b.example.com", Port: 2222, User: "bob"}, gw, uk)
	gone, err := Forget("a.example.com", 2222)
	if err != nil || len(gone) != 1 || gone[0] != "a-cc" {
		t.Fatalf("Forget: %v %v", gone, err)
	}
	b, _ = os.ReadFile(kh)
	if strings.Contains(string(b), "a.example.com") || !strings.Contains(string(b), "rcc-bob@b.example.com") {
		t.Fatalf("known_hosts after Forget:\n%s", b)
	}
	if a := Aliases(); len(a) != 1 || a[0] != "b-cc" {
		t.Fatalf("aliases after Forget: %v", a)
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
