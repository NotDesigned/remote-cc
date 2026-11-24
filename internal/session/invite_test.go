package session

import "testing"

func TestInviteRoundTripAndAlias(t *testing.T) {
	iv := invite{V: 1, Host: "srv.example.com", Port: 2222, User: "alice", Alias: "srv-cc", GwKey: "ssh-ed25519 AAAA", UserKey: "ssh-ed25519 BBBB", ID: "abc", Key: "-----BEGIN-----\n", Expires: "203001010000"}
	got, err := decodeInvite("  " + iv.encode() + "\n")
	if err != nil || *got != iv {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	for _, bad := range []string{"", "rcc1.", "rcc1.!!!", "xyz", "rcc1." + "e30"} {
		if _, err := decodeInvite(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for host, want := range map[string]string{"server-la.example.com": "server-la-cc", "1.2.3.4": "cc", "2001:db8::1": "cc", "Box": "box-cc"} {
		if a := defaultAlias(host); a != want {
			t.Errorf("defaultAlias(%q) = %q, want %q", host, a, want)
		}
	}
}
