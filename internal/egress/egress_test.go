package egress

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMihomoConfig(t *testing.T) {
	n := Node{ID: "proxy-a", Name: "LA", Proxy: json.RawMessage(`{"name":"LA","type":"vless","server":"node.example.com","port":443,"uuid":"u"}`)}
	b, err := MihomoConfig(n, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["routing-mark"].(float64) != RoutingMark {
		t.Errorf("routing-mark = %v", cfg["routing-mark"])
	}
	if cfg["mixed-port"].(float64) != 0 || cfg["allow-lan"] != false {
		t.Errorf("no proxy port may be open: %v %v", cfg["mixed-port"], cfg["allow-lan"])
	}
	tun := cfg["tun"].(map[string]any)
	if tun["enable"] != true || tun["device"] != TunDevice || tun["auto-route"] != false {
		t.Errorf("tun = %v", tun)
	}
	p := cfg["proxies"].([]any)[0].(map[string]any)
	if p["name"] != "EXIT" || p["server"] != "node.example.com" {
		t.Errorf("proxy = %v", p)
	}
	rules := cfg["rules"].([]any)
	if rules[len(rules)-1] != "MATCH,EXIT" {
		t.Errorf("last rule = %v", rules[len(rules)-1])
	}
	if !strings.Contains(string(b), "IP-CIDR,10.0.0.0/8,REJECT,no-resolve") {
		t.Error("private destinations must be refused")
	}
	dns := cfg["dns"].(map[string]any)
	if dns["default-nameserver"].([]any)[0] != "1.1.1.1" {
		t.Errorf("bootstrap = %v", dns["default-nameserver"])
	}
	for _, bad := range []string{`{"name":"x","type":"direct"}`, `{"name":"x"}`, `not json`} {
		if _, err := MihomoConfig(Node{ID: "x", Proxy: json.RawMessage(bad)}, ""); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"proxy-24eae0681ea904255ce79dfe", "a_b-C9"} {
		if !ValidID(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a b", "a/b", strings.Repeat("a", 65), "x;y"} {
		if ValidID(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
