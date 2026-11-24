//go:build linux

package egress

import (
	"strings"
	"testing"
)

func TestLayout(t *testing.T) {
	a, b := LayoutFor("proxy-a", 0), LayoutFor("proxy-a", 1)
	if a == b || a.NS != b.NS {
		t.Errorf("offset must move only the subnet: %+v %+v", a, b)
	}
	if LayoutFor("proxy-a", 0) != a {
		t.Error("layout must be stable")
	}
	if len(a.HostIf) > 15 {
		t.Errorf("interface name too long: %s", a.HostIf)
	}
	if !strings.HasPrefix(a.Subnet, "10.254.") || !strings.HasSuffix(a.Subnet, "/30") {
		t.Errorf("subnet = %s", a.Subnet)
	}
}

// Everything but Mihomo's marked sockets may leave only through the TUN.
func TestFirewallClosesTheUplink(t *testing.T) {
	cmds := setupCommands(LayoutFor("proxy-a", 0))
	var joined []string
	for _, c := range cmds {
		joined = append(joined, strings.Join(c, " "))
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{
		"iptables -A OUTPUT -o eth0 -m mark --mark 6666 -j ACCEPT",
		"iptables -A OUTPUT -o rcctun -j ACCEPT",
		"iptables -A OUTPUT -j REJECT",
		"ip6tables -A OUTPUT -j REJECT",
		"rule add fwmark 6666 lookup 100",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The catch-all REJECT must come after the accepts, per family.
	for _, fam := range []string{"iptables", "ip6tables"} {
		last := -1
		for i, c := range joined {
			if strings.Contains(c, " "+fam+" -A OUTPUT") {
				if strings.HasSuffix(c, "-j REJECT") {
					last = i
				} else if last >= 0 {
					t.Errorf("%s: accept after the final reject: %s", fam, c)
				}
			}
		}
		if last < 0 {
			t.Errorf("%s: no final reject", fam)
		}
	}
}
