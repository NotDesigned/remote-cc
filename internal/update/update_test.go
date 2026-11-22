package update

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		tag, cur string
		want     bool
	}{
		{"v0.3.0", "v0.2.2", true},
		{"v0.2.2", "v0.2.2", false},
		{"v0.2.10", "v0.2.9", true},
		{"v0.2.1", "v0.2.2", false},
		{"v0.2.2", "dev", true},
		{"v0.2.2", "v0.2.2-3-gabc123", true},
		{"garbage", "v0.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.tag, c.cur); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.tag, c.cur, got)
		}
	}
}
