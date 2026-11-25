//go:build linux

package netexec

import "testing"

func TestExitAllowed(t *testing.T) {
	cases := []struct {
		ip, expected string
		want         bool
	}{
		{"203.0.113.7", "", true},
		{"203.0.113.7", "203.0.113.7", true},
		{"203.0.113.7", "198.51.100.1, 203.0.113.7", true},
		{"203.0.113.7", "198.51.100.1", false},
		{"203.0.113.70", "203.0.113.7", false},
	}
	for _, c := range cases {
		if got := ExitAllowed(c.ip, c.expected); got != c.want {
			t.Errorf("ExitAllowed(%q, %q) = %v", c.ip, c.expected, got)
		}
	}
}

func TestAccountEmail(t *testing.T) {
	in, email := AccountEmail([]byte(`{"loggedIn": true, "authMethod": "claude.ai", "email": "a@example.com"}` + "\n"))
	if !in || email != "a@example.com" {
		t.Errorf("got %v %q", in, email)
	}
	if in, _ := AccountEmail([]byte(`{"loggedIn": false}`)); in {
		t.Error("logged out read as logged in")
	}
	if in, _ := AccountEmail([]byte("garbage")); in {
		t.Error("garbage read as logged in")
	}
}

func TestIs(t *testing.T) {
	if !Is([]string{Stage1, "--", "claude"}) || !Is([]string{Stage2}) {
		t.Error("stages not recognised")
	}
	// A command string Claude Code passes to the shim is never a stage.
	if Is([]string{"echo hi"}) || Is(nil) {
		t.Error("ordinary command taken as a stage")
	}
}

func TestCheckAccount(t *testing.T) {
	if w, err := CheckAccount(true, "A@example.com", "a@example.com"); w != "" || err != nil {
		t.Errorf("match: %q %v", w, err)
	}
	if _, err := CheckAccount(true, "b@example.com", "a@example.com"); err == nil {
		t.Error("other account accepted")
	}
	if _, err := CheckAccount(false, "", "a@example.com"); err == nil {
		t.Error("logged out accepted")
	}
	// A stored setup-token: logged in, no email to compare.
	if w, err := CheckAccount(true, "", "a@example.com"); err != nil || w == "" {
		t.Errorf("token login: %q %v", w, err)
	}
}
