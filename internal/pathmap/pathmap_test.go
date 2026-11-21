package pathmap

import "testing"

func TestWindowsRoundTrip(t *testing.T) {
	cases := map[string]string{
		`C:\Users\x\proj`: "/c/Users/x/proj",
		`D:\`:             "/d/",
	}
	for local, remote := range cases {
		if got := ToRemote("windows", local); got != remote {
			t.Errorf("ToRemote(%q) = %q, want %q", local, got, remote)
		}
		if got := ToLocal("windows", remote); got != local {
			t.Errorf("ToLocal(%q) = %q, want %q", remote, got, local)
		}
	}
}

func TestPosixIdentity(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if ToRemote(goos, "/home/a/p") != "/home/a/p" || ToLocal(goos, "/home/a/p") != "/home/a/p" {
			t.Fatal("posix mapping must be identity")
		}
	}
}
