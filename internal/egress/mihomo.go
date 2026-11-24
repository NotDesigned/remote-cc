// Package egress is the "egress host" side of the remote-claude-browser link:
// it runs each proxy node in a network namespace of its own whose only route
// is a Mihomo TUN, and hands that namespace to remote-cc sessions so Claude
// Code's whole network stack exits through the node.
package egress

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// TunDevice is the namespace's default route. Without it (Mihomo stopped)
	// the namespace has no route out at all.
	TunDevice = "rcctun"
	// TunAddress holds Mihomo's TUN; DNSAddress inside it is hijacked to Mihomo's resolver.
	TunAddress = "198.18.0.1/30"
	DNSAddress = "198.18.0.2"
	// RoutingMark tags Mihomo's own sockets (to the node and its bootstrap DNS);
	// only they may use the veth uplink.
	RoutingMark = 6666
)

// Node is one proxy node as remote-claude-browser serves it.
type Node struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Proxy json.RawMessage `json:"proxy"`
}

var privateV4 = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4"}
var privateV6 = []string{"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "fc00::/7", "fe80::/10", "ff00::/8"}

// MihomoConfig is the fixed Mihomo configuration for one node: a TUN whose
// traffic all goes to the node, DNS resolved through the node, and private
// destinations refused.
func MihomoConfig(n Node, bootstrapDNS string) ([]byte, error) {
	var proxy map[string]any
	if err := json.Unmarshal(n.Proxy, &proxy); err != nil {
		return nil, fmt.Errorf("node %s: %w", n.ID, err)
	}
	if t, _ := proxy["type"].(string); t == "" || t == "direct" {
		return nil, fmt.Errorf("node %s: not a proxy node", n.ID)
	}
	proxy["name"] = "EXIT"
	if bootstrapDNS == "" {
		bootstrapDNS = "1.1.1.1"
	}
	rules := make([]string, 0, len(privateV4)+len(privateV6)+1)
	for _, r := range privateV4 {
		rules = append(rules, "IP-CIDR,"+r+",REJECT,no-resolve")
	}
	for _, r := range privateV6 {
		rules = append(rules, "IP-CIDR6,"+r+",REJECT,no-resolve")
	}
	rules = append(rules, "MATCH,EXIT")
	cfg := map[string]any{
		"mixed-port":        0,
		"allow-lan":         false,
		"ipv6":              true, // the node itself may be reachable only over IPv6
		"mode":              "rule",
		"log-level":         "warning",
		"find-process-mode": "off",
		"geodata-mode":      false,
		"geo-auto-update":   false,
		"routing-mark":      RoutingMark,
		"profile":           map[string]any{"store-selected": false, "store-fake-ip": false},
		"dns": map[string]any{
			"enable":                  true,
			"listen":                  "127.0.0.1:1053",
			"ipv6":                    false,
			"enhanced-mode":           "redir-host",
			"use-hosts":               false,
			"use-system-hosts":        false,
			"default-nameserver":      []string{bootstrapDNS},
			"proxy-server-nameserver": []string{bootstrapDNS},
			// Sites resolve through the node; only the node's own hostname bootstraps directly.
			"nameserver":    []string{"https://1.1.1.1/dns-query#EXIT"},
			"respect-rules": true,
		},
		"tun": map[string]any{
			"enable":                true,
			"device":                TunDevice,
			"stack":                 "mixed",
			"auto-route":            false,
			"auto-redirect":         false,
			"auto-detect-interface": false,
			"inet4-address":         []string{TunAddress},
			"dns-hijack":            []string{"any:53", "tcp://any:53"},
		},
		"proxies": []any{proxy},
		"rules":   rules,
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// ValidID reports whether s is safe as a node id (also used in names and paths).
func ValidID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	return strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == ""
}
