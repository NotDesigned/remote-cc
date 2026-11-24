//go:build linux

package egress

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Layout names one node's namespace and its uplink: a /30 in 10.254.0.0/16.
type Layout struct {
	NS     string // network namespace, /run/netns/<NS>
	HostIf string // veth end in the egress container
	HostIP string
	PeerIP string
	Subnet string
}

// LayoutFor derives a node's names and addresses; offset resolves slot collisions.
func LayoutFor(id string, offset int) Layout {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:])
	slot := (int(binary.BigEndian.Uint16(sum[:2]))+offset)%16382 + 1
	base := slot * 4
	prefix := fmt.Sprintf("10.254.%d", base>>8)
	return Layout{
		NS:     "rce-" + h[:12],
		HostIf: "rce" + h[:10],
		HostIP: fmt.Sprintf("%s.%d", prefix, base&255+1),
		PeerIP: fmt.Sprintf("%s.%d", prefix, base&255+2),
		Subnet: fmt.Sprintf("%s.%d/30", prefix, base&255),
	}
}

// Runner runs the system commands; tests replace it.
type Runner func(name string, args ...string) error

func execRunner(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setupCommands build a node's namespace. Mihomo runs inside it as root; its
// sockets carry RoutingMark and are the only ones routed to (and allowed on)
// the veth uplink. Everything else may leave only through the TUN, so a
// stopped Mihomo means no network rather than a direct one. IPv6 is closed
// except for Mihomo's own connections to the node.
func setupCommands(l Layout) [][]string {
	ns := func(args ...string) []string { return append([]string{"ip", "netns", "exec", l.NS}, args...) }
	mark := fmt.Sprint(RoutingMark)
	return [][]string{
		{"ip", "netns", "add", l.NS},
		{"ip", "link", "add", l.HostIf, "type", "veth", "peer", "name", "eth0", "netns", l.NS},
		{"ip", "addr", "add", l.HostIP + "/30", "dev", l.HostIf},
		{"ip", "link", "set", l.HostIf, "up"},
		{"ip", "-n", l.NS, "link", "set", "lo", "up"},
		{"ip", "-n", l.NS, "addr", "add", l.PeerIP + "/30", "dev", "eth0"},
		{"ip", "-n", l.NS, "link", "set", "eth0", "up"},
		{"ip", "-n", l.NS, "route", "add", "default", "via", l.HostIP, "dev", "eth0", "table", "100"},
		{"ip", "-n", l.NS, "rule", "add", "fwmark", mark, "lookup", "100", "priority", "100"},
		ns("iptables", "-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"),
		ns("iptables", "-A", "OUTPUT", "-o", "eth0", "-m", "mark", "--mark", mark, "-j", "ACCEPT"),
		ns("iptables", "-A", "OUTPUT", "-o", TunDevice, "-j", "ACCEPT"),
		ns("iptables", "-A", "OUTPUT", "-j", "REJECT"),
		ns("iptables", "-A", "INPUT", "-i", "lo", "-j", "ACCEPT"),
		ns("iptables", "-A", "INPUT", "-i", TunDevice, "-j", "ACCEPT"),
		ns("iptables", "-A", "INPUT", "-i", "eth0", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
		ns("iptables", "-A", "INPUT", "-j", "DROP"),
		ns("ip6tables", "-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"),
		ns("ip6tables", "-A", "OUTPUT", "-o", "eth0", "-m", "mark", "--mark", mark, "-j", "ACCEPT"),
		ns("ip6tables", "-A", "OUTPUT", "-j", "REJECT"),
		ns("ip6tables", "-A", "INPUT", "-i", "lo", "-j", "ACCEPT"),
		ns("ip6tables", "-A", "INPUT", "-i", "eth0", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
		ns("ip6tables", "-A", "INPUT", "-j", "DROP"),
		// The uplink reaches the internet through the egress container.
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", l.Subnet, "-j", "MASQUERADE"},
	}
}

func teardownCommands(l Layout) [][]string {
	return [][]string{
		{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", l.Subnet, "-j", "MASQUERADE"},
		{"ip", "netns", "del", l.NS}, // removes the veth pair too
	}
}

// nodeRuntime is one node's namespace with a supervised Mihomo in it.
type nodeRuntime struct {
	node   Node
	layout Layout
	dir    string
	run    Runner
	mihomo string
	dns    string

	mu      sync.Mutex
	ready   chan struct{} // closed while the TUN route is up
	cancel  context.CancelFunc
	stopped chan struct{}
}

func (r *nodeRuntime) start() error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	cfg, err := MihomoConfig(r.node, r.dns)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.dir, "config.json"), cfg, 0o600); err != nil {
		return err
	}
	for _, c := range teardownCommands(r.layout) { // leftovers of a crashed run
		r.run(c[0], c[1:]...)
	}
	for _, c := range setupCommands(r.layout) {
		if err := r.run(c[0], c[1:]...); err != nil {
			for _, t := range teardownCommands(r.layout) {
				r.run(t[0], t[1:]...)
			}
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.stopped, r.ready = cancel, make(chan struct{}), make(chan struct{})
	go r.supervise(ctx)
	return nil
}

func (r *nodeRuntime) markReady(up bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.ready:
		if !up {
			r.ready = make(chan struct{})
		}
	default:
		if up {
			close(r.ready)
		}
	}
}

// supervise keeps Mihomo running and the TUN route in place, with backoff.
func (r *nodeRuntime) supervise(ctx context.Context) {
	defer close(r.stopped)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", r.layout.NS, r.mihomo, "-d", r.dir, "-f", filepath.Join(r.dir, "config.json"))
		cmd.Stdout, cmd.Stderr = logWriter(r.node.ID), logWriter(r.node.ID)
		if err := cmd.Start(); err != nil {
			log.Printf("node %s: mihomo: %v", r.node.ID, err)
		} else {
			done := make(chan struct{})
			go func() {
				if err := r.waitTun(ctx, done); err == nil {
					r.markReady(true)
					log.Printf("node %s (%s): up via %s", r.node.ID, r.node.Name, r.layout.NS)
				} else if ctx.Err() == nil {
					log.Printf("node %s: %v", r.node.ID, err)
				}
			}()
			err := cmd.Wait()
			close(done)
			r.markReady(false)
			if ctx.Err() == nil {
				log.Printf("node %s: mihomo exited: %v", r.node.ID, err)
			}
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// waitTun waits for Mihomo's TUN, then makes it the namespace's default route.
func (r *nodeRuntime) waitTun(ctx context.Context, done <-chan struct{}) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errors.New("mihomo exited")
		case <-time.After(100 * time.Millisecond):
		}
		if r.run("ip", "-n", r.layout.NS, "link", "show", TunDevice) != nil {
			continue
		}
		return r.run("ip", "-n", r.layout.NS, "route", "replace", "default", "dev", TunDevice)
	}
	return errors.New("mihomo did not create its TUN device")
}

// wait blocks until the node is usable or ctx ends.
func (r *nodeRuntime) wait(ctx context.Context) error {
	r.mu.Lock()
	ready := r.ready
	r.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("node %s (%s) did not come up; check the node in remote-claude-browser", r.node.ID, r.node.Name)
	}
}

func (r *nodeRuntime) stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.stopped
	}
	for _, c := range teardownCommands(r.layout) {
		r.run(c[0], c[1:]...)
	}
	os.RemoveAll(r.dir)
}

type prefixWriter struct{ prefix string }

func (w prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			log.Printf("%s: %s", w.prefix, line)
		}
	}
	return len(p), nil
}

func logWriter(id string) prefixWriter { return prefixWriter{"node " + id + ": mihomo"} }
