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
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
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

// Request asks for a node's namespace with the session's grant from
// remote-claude-browser. The reply carries the namespace as a file
// descriptor (SCM_RIGHTS) when OK.
type Request struct {
	Node  string `json:"node"`
	Grant string `json:"grant,omitempty"`
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
	RCBURL       string        // remote-claude-browser origin
	Token        string        // host token (rcch_…), approved by a workbench administrator
	NodesFile    string        // static {"nodes":[…]} instead of RCB grants (testing)
	StateDir     string        // per-runtime Mihomo dirs
	UsersDir     string        // <UsersDir>/<user>/egress.sock, one directory per remote-cc user
	Mihomo       string        // Mihomo binary
	BootstrapDNS string        // resolves node hostnames; default 1.1.1.1
	RefreshEvery time.Duration // re-check running nodes with RCB; default 5m
	MaxRuntimes  int           // namespaces (each with a Mihomo) at once; default 64
	MaxPerUser   int           // of them per remote-cc user; default 8
	Run          Runner        // default: exec
	Spawn        Spawner       // starts Mihomo in a namespace; default: ip netns exec
	HTTP         *http.Client
}

// netnsDir holds the named namespaces ("ip netns").
const netnsDir = "/run/netns"

// privateSockDir, inside UsersDir and never a user's, is where sockets are
// made before they move into a user's directory.
const privateSockDir = ".sockets"

// deniedLimit is how many refresh rounds in a row remote-claude-browser may
// reject the host token before every node stops; one is ridden out, as a
// restarting remote-claude-browser may briefly answer 401.
const deniedLimit = 2

// runtimeKey names one tenant's use of a node: each remote-cc user (the socket
// the request came in on) and remote-claude-browser user (whose grant it
// redeemed) gets a namespace and Mihomo of their own, so tenants never share
// connections, DNS cache or a revocation.
type runtimeKey struct{ user, rcbUser, node string }

// layoutSeed makes every runtime's names unique: a namespace an old session
// still holds never blocks the next runtime for the same key.
func layoutSeed(k runtimeKey, gen uint64) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", k.user, k.rcbUser, k.node, gen)
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// userSocket is the listener in one user's directory, and which file it is,
// so a socket the user removed or replaced is made again.
type userSocket struct {
	l        *net.UnixListener
	path     string
	dev, ino uint64
}

// inPlace reports whether path is still this listener's socket.
func (s *userSocket) inPlace() bool {
	fi, err := os.Lstat(s.path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == s.dev && uint64(st.Ino) == s.ino
}

// Daemon serves node namespaces to remote-cc sessions.
type Daemon struct {
	o Options

	mu       sync.Mutex
	static   map[string]Node // NodesFile mode
	runtimes map[runtimeKey]*nodeRuntime
	subnets  map[string]*nodeRuntime // every runtime holding addresses: starting, serving or stopping
	keys     map[runtimeKey]*keyLock // orders starts and stops per key
	gen      uint64
	closing  bool
	denied   int // refresh rounds in a row that got 401
	sockets  map[string]*userSocket
	failing  map[string]bool // users whose socket could not be made (logged once)
	sockSeq  uint64
}

func New(o Options) *Daemon {
	if o.RefreshEvery == 0 {
		o.RefreshEvery = 5 * time.Minute
	}
	if o.MaxRuntimes <= 0 {
		o.MaxRuntimes = 64
	}
	if o.MaxPerUser <= 0 {
		o.MaxPerUser = 8
	}
	if o.Run == nil {
		o.Run = execRunner
	}
	if o.Spawn == nil {
		o.Spawn = nsSpawn
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Daemon{o: o, runtimes: map[runtimeKey]*nodeRuntime{}, subnets: map[string]*nodeRuntime{},
		keys: map[runtimeKey]*keyLock{}, sockets: map[string]*userSocket{}, failing: map[string]bool{}}
}

// Run serves until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(d.o.StateDir, 0o700); err != nil {
		return err
	}
	if d.o.NodesFile != "" {
		b, err := os.ReadFile(d.o.NodesFile)
		if err != nil {
			return err
		}
		var v struct{ Nodes []Node }
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		d.static = map[string]Node{}
		for _, n := range v.Nodes {
			if ValidID(n.ID) {
				d.static[n.ID] = n
			}
		}
		log.Printf("serving %d static nodes from %s", len(d.static), d.o.NodesFile)
	} else if d.o.RCBURL == "" {
		return errors.New("no remote-claude-browser configured (RCC_EGRESS_RCB_URL)")
	}
	d.cleanLeftovers()
	refresh := time.NewTicker(d.o.RefreshEvery)
	defer refresh.Stop()
	scan := time.NewTicker(5 * time.Second)
	defer scan.Stop()
	d.ScanUsers()
	for {
		select {
		case <-ctx.Done():
			d.shutdown()
			return nil
		case <-refresh.C:
			d.Refresh(ctx)
		case <-scan.C:
			d.ScanUsers()
		}
	}
}

// cleanLeftovers removes what an earlier run left: nothing uses it now, and
// /run/netns survives a container restart.
func (d *Daemon) cleanLeftovers() {
	os.RemoveAll(filepath.Join(d.o.StateDir, "nodes"))
	os.RemoveAll(filepath.Join(d.o.UsersDir, privateSockDir))
	entries, _ := os.ReadDir(netnsDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "rce-") {
			d.o.Run("ip", "netns", "del", e.Name())
		}
	}
}

// rcb calls remote-claude-browser with the host token.
func (d *Daemon) rcb(ctx context.Context, method, path string, in any) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(d.o.RCBURL, "/")+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.o.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.o.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

func rcbMessage(status int, body []byte) string {
	var e struct{ Message string }
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	if status == http.StatusUnauthorized {
		return "remote-claude-browser rejected this egress host (approve it again: server egress)"
	}
	return fmt.Sprintf("remote-claude-browser answered HTTP %d", status)
}

// resolve turns a request into its node and the remote-claude-browser user
// whose grant it is: the static list (no user), or the grant redeemed with
// remote-claude-browser (which checks the user may use it).
func (d *Daemon) resolve(ctx context.Context, req Request) (Node, string, error) {
	if d.static != nil {
		if n, ok := d.static[req.Node]; ok {
			return n, "", nil
		}
		return Node{}, "", fmt.Errorf("no static node %q", req.Node)
	}
	if req.Grant == "" {
		return Node{}, "", errors.New("the session has no grant from remote-claude-browser")
	}
	status, body, err := d.rcb(ctx, "POST", "/api/remote-cc/v1/egress/redeem", map[string]string{"grant": req.Grant})
	if err != nil {
		return Node{}, "", fmt.Errorf("cannot reach remote-claude-browser: %w", err)
	}
	if status != http.StatusOK {
		return Node{}, "", errors.New(rcbMessage(status, body))
	}
	var v struct {
		Node   Node
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(body, &v); err != nil || !ValidID(v.Node.ID) || len(v.Node.Proxy) == 0 {
		return Node{}, "", errors.New("unexpected answer from remote-claude-browser")
	}
	if v.Node.ID != req.Node {
		return Node{}, "", errors.New("the grant is for another node")
	}
	return v.Node, v.UserID, nil
}

// nodePath is where Refresh re-checks rt: only the user whose grant started
// it, so revoking one user ends just their sessions.
func nodePath(rt *nodeRuntime) string {
	p := "/api/remote-cc/v1/egress/nodes/" + url.PathEscape(rt.node.ID)
	if rt.key.rcbUser != "" {
		p += "?user=" + url.QueryEscape(rt.key.rcbUser)
	}
	return p
}

// staleReason says why a node check means rt must stop, or "" to keep it;
// anything unclear (an error page, a restart) keeps what runs.
func staleReason(status int, body []byte, have Node) string {
	switch status {
	case http.StatusNotFound, http.StatusForbidden:
		return "no longer granted to this host or user"
	case http.StatusOK:
		var v struct{ Node Node }
		if json.Unmarshal(body, &v) != nil || len(v.Node.Proxy) == 0 {
			return ""
		}
		if !bytes.Equal(compact(v.Node.Proxy), compact(have.Proxy)) {
			return "changed (sessions restart with the new one)"
		}
	}
	return ""
}

func (d *Daemon) running() []*nodeRuntime {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*nodeRuntime, 0, len(d.runtimes))
	for _, rt := range d.runtimes {
		out = append(out, rt)
	}
	return out
}

// Refresh re-checks the running nodes with remote-claude-browser: a node
// whose configuration changed, or that is no longer granted to its user on
// this host, stops (its sessions lose their network until they restart).
// If the host token itself is rejected deniedLimit rounds in a row, every
// node stops: nothing may run on a revoked approval.
func (d *Daemon) Refresh(ctx context.Context) {
	if d.static != nil {
		return
	}
	running := d.running()
	type answer struct {
		status int
		body   []byte
		err    error
	}
	answers := map[string]answer{}
	unauthorized, answered := false, false
	for _, rt := range running {
		path := nodePath(rt)
		a, ok := answers[path]
		if !ok {
			a.status, a.body, a.err = d.rcb(ctx, "GET", path, nil)
			answers[path] = a
		}
		if a.err != nil {
			continue // remote-claude-browser unreachable: keep what runs
		}
		if a.status == http.StatusUnauthorized {
			unauthorized = true
			break
		}
		answered = true
		if why := staleReason(a.status, a.body, rt.node); why != "" {
			log.Printf("%s: %s; stopping it", rt, why)
			d.stop(rt)
		}
	}
	d.mu.Lock()
	if unauthorized {
		d.denied++
	} else if answered {
		d.denied = 0
	}
	denied := d.denied
	d.mu.Unlock()
	if !unauthorized {
		return
	}
	if denied < deniedLimit {
		log.Printf("remote-claude-browser rejected this egress host's token; every node stops if it does so again")
		return
	}
	all := d.running()
	log.Printf("remote-claude-browser rejected this egress host's token %d times in a row (revoked, or its owner is no longer an enabled administrator); stopping all %d nodes. Approve the host again: server egress", denied, len(all))
	for _, rt := range all {
		d.stop(rt)
	}
}

func compact(b []byte) []byte {
	var out bytes.Buffer
	if json.Compact(&out, b) != nil {
		return b
	}
	return out.Bytes()
}

// lockKey serializes starting and stopping one key's runtime, so a stale stop
// never interleaves with the start that replaces it.
func (d *Daemon) lockKey(k runtimeKey) (unlock func()) {
	d.mu.Lock()
	kl := d.keys[k]
	if kl == nil {
		kl = &keyLock{}
		d.keys[k] = kl
	}
	kl.refs++
	d.mu.Unlock()
	kl.mu.Lock()
	return func() {
		kl.mu.Unlock()
		d.mu.Lock()
		if kl.refs--; kl.refs == 0 {
			delete(d.keys, k)
		}
		d.mu.Unlock()
	}
}

func (d *Daemon) stop(rt *nodeRuntime) {
	unlock := d.lockKey(rt.key)
	defer unlock()
	d.stopHeld(rt)
}

// stopHeld stops rt; the caller holds its key. Only entries that still point
// to rt are removed, and its addresses are freed only after teardown.
func (d *Daemon) stopHeld(rt *nodeRuntime) {
	d.mu.Lock()
	if d.runtimes[rt.key] == rt {
		delete(d.runtimes, rt.key)
	}
	d.mu.Unlock()
	rt.stop()
	d.release(rt)
}

func (d *Daemon) release(rt *nodeRuntime) {
	d.mu.Lock()
	if d.subnets[rt.layout.Subnet] == rt {
		delete(d.subnets, rt.layout.Subnet)
	}
	d.mu.Unlock()
}

// userCount counts user's runtimes, including starting and stopping ones;
// the caller holds d.mu.
func (d *Daemon) userCount(user string) int {
	n := 0
	for _, rt := range d.subnets {
		if rt.key.user == user {
			n++
		}
	}
	return n
}

// ensure starts k's runtime for n if needed and waits until it routes.
func (d *Daemon) ensure(ctx context.Context, k runtimeKey, n Node) (*nodeRuntime, error) {
	d.makeRoom(k)
	unlock := d.lockKey(k)
	rt, err := d.acquire(k, n)
	unlock()
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return rt, rt.wait(wctx)
}

// makeRoom stops the user's least recently requested runtime when a new one
// would pass MaxPerUser: nothing tells which namespaces sessions still use,
// and a user who moved between nodes must not be locked out. It runs before
// k's lock is taken, so no two key locks are ever held together.
func (d *Daemon) makeRoom(k runtimeKey) {
	d.mu.Lock()
	var victim *nodeRuntime
	if d.runtimes[k] == nil && d.userCount(k.user) >= d.o.MaxPerUser {
		for key, rt := range d.runtimes {
			if key.user == k.user && (victim == nil || rt.used.Before(victim.used)) {
				victim = rt
			}
		}
	}
	d.mu.Unlock()
	if victim != nil {
		log.Printf("user %s may run %d nodes at once; stopping %s, the least recently used", k.user, d.o.MaxPerUser, victim)
		d.stop(victim)
	}
}

// acquire returns k's runtime for n, starting one if needed; the caller holds k.
func (d *Daemon) acquire(k runtimeKey, n Node) (*nodeRuntime, error) {
	d.mu.Lock()
	rt := d.runtimes[k]
	d.mu.Unlock()
	if rt != nil && !bytes.Equal(compact(rt.node.Proxy), compact(n.Proxy)) {
		log.Printf("%s changed; restarting it", rt)
		d.stopHeld(rt)
		rt = nil
	}
	if rt != nil {
		d.mu.Lock()
		rt.used = time.Now()
		d.mu.Unlock()
		return rt, nil
	}
	rt, err := d.reserve(k, n)
	if err != nil {
		return nil, err
	}
	if err := rt.start(); err != nil {
		d.release(rt)
		return nil, err
	}
	d.mu.Lock()
	d.runtimes[k] = rt
	d.mu.Unlock()
	return rt, nil
}

// reserve picks names and a subnet for a new runtime of k, within the limits.
func (d *Daemon) reserve(k runtimeKey, n Node) (*nodeRuntime, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return nil, errors.New("the egress host is shutting down")
	}
	if len(d.subnets) >= d.o.MaxRuntimes {
		return nil, fmt.Errorf("this egress host already runs %d node namespaces, its limit; try again later", len(d.subnets))
	}
	if d.userCount(k.user) >= d.o.MaxPerUser {
		return nil, fmt.Errorf("user %s already runs %d node namespaces, the limit; try again shortly", k.user, d.o.MaxPerUser)
	}
	var seed string
	var l Layout
	for {
		d.gen++
		seed = layoutSeed(k, d.gen)
		l = LayoutFor(seed, 0)
		if !d.nameUsed(l) {
			break
		}
	}
	for off := 1; d.subnets[l.Subnet] != nil; off++ {
		l = LayoutFor(seed, off)
	}
	rt := &nodeRuntime{key: k, node: n, layout: l, dir: filepath.Join(d.o.StateDir, "nodes", l.NS),
		run: d.o.Run, spawn: d.o.Spawn, mihomo: d.o.Mihomo, dns: d.o.BootstrapDNS,
		used: time.Now(), done: make(chan struct{})}
	d.subnets[l.Subnet] = rt
	return rt, nil
}

// nameUsed reports a (vanishingly unlikely) hash collision with a live
// runtime's names; the caller holds d.mu.
func (d *Daemon) nameUsed(l Layout) bool {
	for _, rt := range d.subnets {
		if rt.layout.NS == l.NS || rt.layout.HostIf == l.HostIf {
			return true
		}
	}
	return false
}

var userName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ScanUsers keeps a socket at <UsersDir>/<user>/egress.sock for every user
// directory, re-making one the user removed or replaced.
func (d *Daemon) ScanUsers() {
	entries, err := os.ReadDir(d.o.UsersDir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !userName.MatchString(name) {
			continue
		}
		seen[name] = true
		d.mu.Lock()
		cur := d.sockets[name]
		d.mu.Unlock()
		if cur != nil && cur.inPlace() {
			continue
		}
		s, err := d.listen(name, filepath.Join(d.o.UsersDir, name, SocketName))
		d.mu.Lock()
		if err != nil {
			if !d.failing[name] {
				log.Printf("user %s: %v", name, err)
			}
			d.failing[name] = true
			d.mu.Unlock()
			continue
		}
		delete(d.failing, name)
		old := d.sockets[name]
		d.sockets[name] = s
		d.mu.Unlock()
		if old != nil {
			old.l.Close() // never unlinks (see listen): the path is the new socket now
		}
		go d.serve(name, s.l)
	}
	d.mu.Lock()
	for u, s := range d.sockets {
		if !seen[u] {
			s.l.Close()
			delete(d.sockets, u)
		}
	}
	for u := range d.failing {
		if !seen[u] {
			delete(d.failing, u)
		}
	}
	d.mu.Unlock()
}

// listen makes a socket for user at path. The user may write their
// directory, so nothing there is chmod-ed or followed: the socket is bound
// and given its mode in a directory only this daemon writes, then renamed
// over path. That replaces a file or symlink at path without following it,
// and swaps an old socket for the new one with no moment between.
func (d *Daemon) listen(user, path string) (*userSocket, error) {
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory; remove it to get the egress socket", path)
	}
	dir := filepath.Join(d.o.UsersDir, privateSockDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	d.mu.Lock()
	d.sockSeq++
	tmp := filepath.Join(dir, fmt.Sprintf("%s.%d", user, d.sockSeq))
	d.mu.Unlock()
	os.Remove(tmp)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// Closing must never unlink by path: by then it names the next socket.
	l.SetUnlinkOnClose(false)
	s := &userSocket{l: l, path: path}
	err = os.Chmod(tmp, 0o600)
	if err == nil {
		var fi os.FileInfo
		if fi, err = os.Lstat(tmp); err == nil {
			st := fi.Sys().(*syscall.Stat_t)
			s.dev, s.ino = uint64(st.Dev), uint64(st.Ino)
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		l.Close()
		os.Remove(tmp)
		return nil, err
	}
	return s, nil
}

func (d *Daemon) serve(user string, l *net.UnixListener) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		go d.handle(user, c)
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
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	n, rcbUser, err := d.resolve(ctx, req)
	if err != nil {
		log.Printf("user %s, node %s: %v", user, req.Node, err)
		reply(Reply{Error: err.Error()}, -1)
		return
	}
	rt, err := d.ensure(ctx, runtimeKey{user: user, rcbUser: rcbUser, node: n.ID}, n)
	if err != nil {
		log.Printf("user %s, node %s: %v", user, req.Node, err)
		reply(Reply{Error: err.Error()}, -1)
		return
	}
	f, err := os.Open(filepath.Join(netnsDir, rt.layout.NS))
	if err != nil {
		reply(Reply{Error: "namespace unavailable"}, -1)
		return
	}
	defer f.Close()
	log.Printf("%s: session (%s)", rt, rt.node.Name)
	reply(Reply{OK: true, Node: rt.node.ID, Name: rt.node.Name, DNS: DNSAddress}, int(f.Fd()))
}

func (d *Daemon) shutdown() {
	d.mu.Lock()
	d.closing = true
	for u, s := range d.sockets {
		s.l.Close()
		if s.inPlace() {
			os.Remove(s.path)
		}
		delete(d.sockets, u)
	}
	rts := make([]*nodeRuntime, 0, len(d.subnets))
	for _, rt := range d.subnets {
		rts = append(rts, rt)
	}
	d.mu.Unlock()
	for _, rt := range rts {
		d.stop(rt)
	}
}

// Dial asks the egress daemon at sock for a node's namespace and returns its
// file descriptor (the caller closes it).
func Dial(sock, node, grant string, timeout time.Duration) (int, Reply, error) {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return -1, Reply{}, fmt.Errorf("no egress host at %s (deploy it with 'server egress'): %w", sock, err)
	}
	defer c.Close()
	uc := c.(*net.UnixConn)
	uc.SetDeadline(time.Now().Add(timeout))
	b, _ := json.Marshal(Request{Node: node, Grant: grant})
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
