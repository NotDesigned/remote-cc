package session

import (
	"strings"
	"testing"
)

func TestMachineProfileStable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	a, err := MachineProfile()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := MachineProfile()
	if a != b || !validProfile(a) || !strings.Contains(a, "-") {
		t.Fatalf("profile %q / %q", a, b)
	}
}

func TestValidProfile(t *testing.T) {
	for _, p := range []string{"laptop", "work-pc.2", "a_b"} {
		if !validProfile(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"", "..", "a/b", "x y", "$(id)"} {
		if validProfile(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
}
