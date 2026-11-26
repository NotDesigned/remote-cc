package server

import (
	"strings"
	"testing"

	"remote-cc/internal/remote"
)

// Every user command checks NAME before it reaches the server (the name
// becomes paths and lines there). The ssh binary here does not exist: a
// command that got as far as ssh would fail differently.
func TestUserCommandsCheckName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Config{Target: remote.Target{SSH: "/nonexistent/ssh", Dest: "host"}}
	for name, err := range map[string]error{
		"rebuild": UserRebuild(c, "../x", false),
		"remove":  UserRemove(c, "../x", false, false),
		"invite":  UserInvite(c, "../x", "", 0),
		"config":  UserConfig(c, "../x", ""),
	} {
		if err == nil || !strings.Contains(err.Error(), "invalid user name") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
