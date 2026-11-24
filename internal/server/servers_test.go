package server

import (
	"testing"

	"remote-cc/internal/remote"
)

func TestRememberForget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	a := Config{Target: remote.Target{Dest: "root@a", Args: []string{"-p", "2200"}}, InstallDir: "/opt/remote-cc"}
	b := Config{Target: remote.Target{Dest: "ubuntu@b"}, InstallDir: "/opt/remote-cc"}
	b2 := Config{Target: remote.Target{Dest: "ubuntu@b"}, InstallDir: "/opt/other"}
	remember(a)
	remember(b)
	remember(b2)
	remember(a) // again: replaced, not duplicated
	ks, err := KnownServers()
	if err != nil || len(ks) != 3 {
		t.Fatalf("%v %+v", err, ks)
	}
	if ks[2].Dest != "root@a" || len(ks[2].Args) != 2 {
		t.Fatalf("re-remembered entry: %+v", ks[2])
	}
	if n, _ := Forget("ubuntu@b", "/opt/other"); n != 1 {
		t.Fatalf("forget one install dir: %d", n)
	}
	if n, _ := Forget("ubuntu@b", ""); n != 1 {
		t.Fatalf("forget the rest: %d", n)
	}
	if ks, _ := KnownServers(); len(ks) != 1 || ks[0].Dest != "root@a" {
		t.Fatalf("%+v", ks)
	}
}
