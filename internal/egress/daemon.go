//go:build linux

package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SocketName is each user's socket inside their directory.
const SocketName = "egress.sock"

// Request asks for a node's namespace. The reply carries the namespace as a
// file descriptor (SCM_RIGHTS) when OK.
type Request struct {
	Node string `json:"node"`
}
type Reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Node  string `json:"node,omitempty"`
	Name  string `json:"name,omitempty"`
	DNS   string `json:"dns,omitempty"`
}

// Options configure the daemon.
type Options struct {
	RCBURL       string        // remote-claude-browser origin; "" = only NodesFile / cache
	Token        string        // host token (rcch_…)
	NodesFile    string        // static node list instead of RCBURL (tests)
	StateDir     string        // cache and per-node Mihomo dirs
	UsersDir     string        // <UsersDir>/<user>/egress.sock, one directory per remote-cc user
	Mihomo       string        // Mihomo binary
	BootstrapDNS string        // resolves node hostnames; default 1.1.1.1
	SyncEvery    time.Duration // default 5m
	Run          Runner        // default: exec
	HTTP         *http.Client
}

// Daemon serves node namespaces to remote-cc sessions.
type Daemon struct {
	o Options

	mu       sync.Mutex
	nodes    map[string]Node
	runtimes map[string]*nodeRuntime
	subnets  map[string]string // subnet -> node id
	sockets  map[string]net.Listener
	synced   int // node count last logged; -1 = none yet
}

func New(o Options) *Daemon {
	if o.SyncEvery == 0 {
		o.SyncEvery = 5 * time.Minute
	}
	if o.Run == nil {
		o.Run = execRunner
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Daemon{o: o, nodes: map[string]Node{}, runtimes: map[string]*nodeRuntime{},
		subnets: map[string]string{}, sockets: map[string]net.Listener{}, synced: -1}
}

func (d *Daemon) cacheFile() string { return filepath.Join(d.o.StateDir, "nodes.json") }

// Run loads the cached nodes, then syncs and serves until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(d.o.StateDir, 0o700); err != nil {
		return err
	}
	if b, err := os.ReadFile(d.cacheFile()); err == nil {
		var cached []Node
		if json.Unmarshal(b, &cached) == nil {
			d.apply(cached)
			log.Printf("loaded %d cached nodes", len(cached))
		}
	}
	if err := d.Sync(ctx); err != nil {
		log.Printf("sync: %v (serving cached nodes)", err)
	}
	tick := time.NewTicker(d.o.SyncEvery)
	defer tick.Stop()
	scan := time.NewTicker(5 * time.Second)
	defer scan.Stop()
	d.ScanUsers()
	for {
		select {
		case <-ctx.Done():
			d.shutdown()
			return nil
		case <-tick.C:
			if err := d.Sync(ctx); err != nil {
				log.Printf("sync: %v", err)
			}
		case <-scan.C:
			d.ScanUsers()
		}
	}
}

// Sync fetches the node list and applies it; the cache keeps serving if
// remote-claude-browser is unreachable.
func (d *Daemon) Sync(ctx context.Context) error {
	var body []byte
	switch {
	case d.o.NodesFile != "":
		b, err := os.ReadFile(d.o.NodesFile)
		if err != nil {
			return err
		}
		body = b
	case d.o.RCBURL != "":
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(d.o.RCBURL, "/")+"/api/remote-cc/v1/egress/nodes", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+d.o.Token)
		resp, err := d.o.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if resp.StatusCode != 200 {
			return fmt.Errorf("remote-claude-browser answered %d: %s", resp.StatusCode, bytes.TrimSpace(body))
		}
	default:
		return nil
	}
	var v struct{ Nodes []Node }
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	valid := v.Nodes[:0]
	for _, n := range v.Nodes {
		if ValidID(n.ID) && len(n.Proxy) > 0 {
			valid = append(valid, n)
		}
	}
	b, _ := json.Marshal(valid)
	tmp := d.cacheFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, d.cacheFile())
	}
	d.apply(valid)
	if len(valid) != d.synced {
		log.Printf("synced %d nodes from remote-claude-browser", len(valid))
		d.synced = len(valid)
	}
	return nil
}

// apply replaces the node list; running nodes that changed or vanished stop
// (sessions using them lose their network until they restart).
func (d *Daemon) apply(nodes []Node) {
	next := map[string]Node{}
	for _, n := range nodes {
		next[n.ID] = n
	}
	d.mu.Lock()
	var stale []*nodeRuntime
	for id, rt := range d.runtimes {
		if n, ok := next[id]; !ok || !bytes.Equal(n.Proxy, rt.node.Proxy) {
			stale = append(stale, rt)
			delete(d.runtimes, id)
			delete(d.subnets, rt.layout.Subnet)
		}
	}
	d.nodes = next
	d.mu.Unlock()
	for _, rt := range stale {
		log.Printf("node %s changed or was removed; stopping it", rt.node.ID)
		rt.stop()
	}
}

// ensure starts a node's namespace if needed and waits until it routes.
func (d *Daemon) ensure(ctx context.Context, id string) (*nodeRuntime, error) {
	d.mu.Lock()
	n, ok := d.nodes[id]
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("this egress host has no node %q (the host token's user cannot use it in remote-claude-browser)", id)
	}
	rt := d.runtimes[id]
	if rt == nil {
		var l Layout
		for off := 0; ; off++ {
			l = LayoutFor(id, off)
			if _, used := d.subnets[l.Subnet]; !used {
				break
			}
		}
		rt = &nodeRuntime{node: n, layout: l, dir: filepath.Join(d.o.StateDir, "nodes", l.NS), run: d.o.Run,
			mihomo: d.o.Mihomo, dns: d.o.BootstrapDNS}
		if err := rt.start(); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		d.runtimes[id], d.subnets[l.Subnet] = rt, id
	}
	d.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return rt, rt.wait(wctx)
}

var userName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ScanUsers listens on <UsersDir>/<user>/egress.sock for every user directory.
func (d *Daemon) ScanUsers() {
	entries, err := os.ReadDir(d.o.UsersDir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !userName.MatchString(e.Name()) {
			continue
		}
		seen[e.Name()] = true
		path := filepath.Join(d.o.UsersDir, e.Name(), SocketName)
		d.mu.Lock()
		_, have := d.sockets[e.Name()]
		d.mu.Unlock()
		if have {
			if _, err := os.Stat(path); err == nil {
				continue
			}
		}
		os.Remove(path)
		l, err := net.Listen("unix", path)
		if err != nil {
			log.Printf("user %s: %v", e.Name(), err)
			continue
		}
		os.Chmod(path, 0o600)
		d.mu.Lock()
		if old := d.sockets[e.Name()]; old != nil {
			old.Close()
		}
		d.sockets[e.Name()] = l
		d.mu.Unlock()
		go d.serve(e.Name(), l)
	}
	d.mu.Lock()
	for u, l := range d.sockets {
		if !seen[u] {
			l.Close()
			delete(d.sockets, u)
		}
	}
	d.mu.Unlock()
}

func (d *Daemon) serve(user string, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go d.handle(user, c.(*net.UnixConn))
	}
}

func (d *Daemon) handle(user string, c *net.UnixConn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(40 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, 4096)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	reply := func(r Reply, fd int) {
		b, _ := json.Marshal(r)
		var oob []byte
		if fd >= 0 {
			oob = syscall.UnixRights(fd)
		}
		c.WriteMsgUnix(append(b, '\n'), oob, nil)
	}
	var req Request
	if json.Unmarshal(line, &req) != nil || !ValidID(req.Node) {
		reply(Reply{Error: "bad request"}, -1)
		return
	}
	rt, err := d.ensure(context.Background(), req.Node)
	if err != nil {
		log.Printf("user %s, node %s: %v", user, req.Node, err)
		reply(Reply{Error: err.Error()}, -1)
		return
	}
	f, err := os.Open("/run/netns/" + rt.layout.NS)
	if err != nil {
		reply(Reply{Error: "namespace unavailable"}, -1)
		return
	}
	defer f.Close()
	log.Printf("user %s: session on node %s (%s)", user, rt.node.ID, rt.node.Name)
	reply(Reply{OK: true, Node: rt.node.ID, Name: rt.node.Name, DNS: DNSAddress}, int(f.Fd()))
}

func (d *Daemon) shutdown() {
	d.mu.Lock()
	for _, l := range d.sockets {
		l.Close()
	}
	rts := d.runtimes
	d.runtimes = map[string]*nodeRuntime{}
	d.mu.Unlock()
	for _, rt := range rts {
		rt.stop()
	}
}

// Dial asks the egress daemon at sock for a node's namespace and returns its
// file descriptor (the caller closes it).
func Dial(sock, node string, timeout time.Duration) (int, Reply, error) {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return -1, Reply{}, fmt.Errorf("no egress host at %s (deploy it with 'server egress'): %w", sock, err)
	}
	defer c.Close()
	uc := c.(*net.UnixConn)
	uc.SetDeadline(time.Now().Add(timeout))
	b, _ := json.Marshal(Request{Node: node})
	if _, err := uc.Write(append(b, '\n')); err != nil {
		return -1, Reply{}, err
	}
	buf, oob := make([]byte, 4096), make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, Reply{}, err
	}
	var r Reply
	if err := json.Unmarshal(bytes.TrimSpace(buf[:n]), &r); err != nil {
		return -1, Reply{}, err
	}
	if !r.OK {
		return -1, r, errors.New(r.Error)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return -1, r, errors.New("no namespace in the reply")
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return -1, r, errors.New("no namespace in the reply")
	}
	return fds[0], r, nil
}
