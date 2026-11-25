//go:build linux

package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeHost records the system commands and runs a stand-in for Mihomo, so
// runtimes start, become ready and stop without root.
type fakeHost struct {
	mu   sync.Mutex
	cmds []string
}

func (h *fakeHost) run(name string, args ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmds = append(h.cmds, strings.Join(append([]string{name}, args...), " "))
	return nil
}

func (h *fakeHost) count(cmd string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

func sleepSpawn(ctx context.Context, ns string, argv ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "sleep", "60")
}

func testDaemon(t *testing.T, o Options) (*Daemon, *fakeHost) {
	t.Helper()
	h := &fakeHost{}
	o.StateDir, o.Run, o.Spawn = t.TempDir(), h.run, sleepSpawn
	d := New(o)
	t.Cleanup(d.shutdown)
	return d, h
}

func node(id, server string) Node {
	return Node{ID: id, Name: id, Proxy: json.RawMessage(fmt.Sprintf(`{"type":"vless","server":%q,"port":443,"uuid":"u"}`, server))}
}

func mustEnsure(t *testing.T, d *Daemon, k runtimeKey, n Node) *nodeRuntime {
	t.Helper()
	rt, err := d.ensure(context.Background(), k, n)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func stopped(rt *nodeRuntime) bool {
	select {
	case <-rt.done:
		return true
	default:
		return false
	}
}

func TestLayoutPerGeneration(t *testing.T) {
	k := runtimeKey{user: "alice", rcbUser: "u1", node: "proxy-a"}
	a, b := LayoutFor(layoutSeed(k, 1), 0), LayoutFor(layoutSeed(k, 2), 0)
	if a.NS == b.NS || a.HostIf == b.HostIf {
		t.Errorf("generations must not share names: %+v %+v", a, b)
	}
	other := LayoutFor(layoutSeed(runtimeKey{user: "bob", rcbUser: "u1", node: "proxy-a"}, 1), 0)
	if other.NS == a.NS || other.HostIf == a.HostIf {
		t.Errorf("users must not share names: %+v %+v", a, other)
	}
}

func TestRuntimesPerTenant(t *testing.T) {
	d, _ := testDaemon(t, Options{})
	n := node("proxy-a", "a.example")
	alice := mustEnsure(t, d, runtimeKey{"alice", "u1", n.ID}, n)
	bob := mustEnsure(t, d, runtimeKey{"bob", "u2", n.ID}, n)
	if alice == bob || alice.layout.NS == bob.layout.NS || alice.layout.Subnet == bob.layout.Subnet || alice.dir == bob.dir {
		t.Fatalf("tenants share a runtime: %+v %+v", alice.layout, bob.layout)
	}
	if again := mustEnsure(t, d, runtimeKey{"alice", "u1", n.ID}, n); again != alice {
		t.Error("the same tenant and node must reuse the runtime")
	}
}

// A node restarted while an old session holds its namespace gets new names,
// and teardown deletes the host end of the veth explicitly.
func TestRestartGetsNewNames(t *testing.T) {
	d, h := testDaemon(t, Options{})
	k, n := runtimeKey{"alice", "u1", "proxy-a"}, node("proxy-a", "a.example")
	first := mustEnsure(t, d, k, n)
	d.stop(first)
	if h.count("ip link del "+first.layout.HostIf) == 0 {
		t.Error("teardown must delete the host-side link")
	}
	second := mustEnsure(t, d, k, n)
	if second.layout.NS == first.layout.NS || second.layout.HostIf == first.layout.HostIf {
		t.Errorf("restart reused names: %+v", second.layout)
	}
}

// A stale stop (a refresh that raced a restart) must not touch the new runtime.
func TestStaleStopKeepsNewRuntime(t *testing.T) {
	d, h := testDaemon(t, Options{})
	k := runtimeKey{"alice", "u1", "proxy-a"}
	old := mustEnsure(t, d, k, node("proxy-a", "a.example"))
	fresh := mustEnsure(t, d, k, node("proxy-a", "b.example")) // changed: replaces old
	if fresh == old || !stopped(old) {
		t.Fatal("a changed node must restart")
	}
	dels := h.count("ip netns del " + old.layout.NS)
	d.stop(old)
	if h.count("ip netns del "+old.layout.NS) != dels {
		t.Error("stopping twice tore down twice")
	}
	if stopped(fresh) || d.runtimes[k] != fresh || d.subnets[fresh.layout.Subnet] != fresh {
		t.Error("a stale stop removed the new runtime")
	}
}

func TestConcurrentEnsureAndStop(t *testing.T) {
	d, _ := testDaemon(t, Options{})
	k, n := runtimeKey{"alice", "u1", "proxy-a"}, node("proxy-a", "a.example")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rt, err := d.ensure(context.Background(), k, n); err == nil {
				d.stop(rt)
			}
		}()
	}
	wg.Wait()
	rt := mustEnsure(t, d, k, n)
	if stopped(rt) || d.runtimes[k] != rt {
		t.Error("runtime not serving after concurrent stops")
	}
	if len(d.subnets) != 1 {
		t.Errorf("leaked %d address reservations", len(d.subnets)-1)
	}
}

func TestRuntimeLimits(t *testing.T) {
	d, _ := testDaemon(t, Options{MaxPerUser: 2, MaxRuntimes: 3})
	a1 := mustEnsure(t, d, runtimeKey{"alice", "", "n1"}, node("n1", "x"))
	a2 := mustEnsure(t, d, runtimeKey{"alice", "", "n2"}, node("n2", "x"))
	mustEnsure(t, d, runtimeKey{"alice", "", "n1"}, node("n1", "x")) // n2 is now the least recently used
	a3 := mustEnsure(t, d, runtimeKey{"alice", "", "n3"}, node("n3", "x"))
	if !stopped(a2) || stopped(a1) || stopped(a3) {
		t.Error("the user's least recently used node must make room")
	}
	mustEnsure(t, d, runtimeKey{"bob", "", "n1"}, node("n1", "x"))
	if _, err := d.ensure(context.Background(), runtimeKey{"carol", "", "n1"}, node("n1", "x")); err == nil {
		t.Error("the host limit must refuse a new runtime")
	}
}

// rcbStub answers node checks from a table keyed by "<node>?<user>".
type rcbStub struct {
	mu      sync.Mutex
	answers map[string]func(w http.ResponseWriter)
	seen    []string
}

func (s *rcbStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer rcch_test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/remote-cc/v1/egress/redeem":
		w.Write([]byte(`{"node":{"id":"proxy-a","name":"LA","proxy":{"type":"vless","server":"a.example"}},"userId":"u1"}`))
	case strings.HasPrefix(r.URL.Path, "/api/remote-cc/v1/egress/nodes/"):
		q := strings.TrimPrefix(r.URL.Path, "/api/remote-cc/v1/egress/nodes/") + "?" + r.URL.Query().Get("user")
		s.seen = append(s.seen, q)
		if f := s.answers[q]; f != nil {
			f(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *rcbStub) set(q string, f func(w http.ResponseWriter)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[q] = f
}

func answerNode(n Node) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { json.NewEncoder(w).Encode(map[string]Node{"node": n}) }
}

func answerStatus(code int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

func rcbDaemon(t *testing.T) (*Daemon, *rcbStub) {
	stub := &rcbStub{answers: map[string]func(http.ResponseWriter){}}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	d, _ := testDaemon(t, Options{RCBURL: srv.URL, Token: "rcch_test"})
	return d, stub
}

func TestResolveKeepsGrantUser(t *testing.T) {
	d, _ := rcbDaemon(t)
	n, user, err := d.resolve(context.Background(), Request{Node: "proxy-a", Grant: "g"})
	if err != nil || n.ID != "proxy-a" || user != "u1" {
		t.Fatalf("resolve = %v %q %v", n.ID, user, err)
	}
}

// Refresh re-checks each runtime for the user whose grant started it.
func TestRefreshPerUser(t *testing.T) {
	d, stub := rcbDaemon(t)
	n := node("proxy-a", "a.example")
	kept := mustEnsure(t, d, runtimeKey{"alice", "u1", n.ID}, n)
	revoked := mustEnsure(t, d, runtimeKey{"bob", "u2", n.ID}, n)
	changed := mustEnsure(t, d, runtimeKey{"carol", "u3", n.ID}, n)
	down := mustEnsure(t, d, runtimeKey{"dave", "u4", n.ID}, n)
	stub.set("proxy-a?u1", answerNode(n))
	stub.set("proxy-a?u3", answerNode(node("proxy-a", "b.example")))
	stub.set("proxy-a?u4", answerStatus(http.StatusBadGateway))
	d.Refresh(context.Background())
	if stopped(kept) || stopped(down) {
		t.Error("a still-granted node, or an unclear answer, must keep running")
	}
	if !stopped(revoked) || !stopped(changed) {
		t.Error("a revoked or changed node must stop")
	}
	if _, ok := d.runtimes[runtimeKey{"bob", "u2", n.ID}]; ok {
		t.Error("stopped runtime still registered")
	}
}

func TestRefreshStopsAllOnRepeatedUnauthorized(t *testing.T) {
	d, stub := rcbDaemon(t)
	n := node("proxy-a", "a.example")
	rt := mustEnsure(t, d, runtimeKey{"alice", "u1", n.ID}, n)
	stub.set("proxy-a?u1", answerStatus(http.StatusUnauthorized))
	d.Refresh(context.Background())
	if stopped(rt) {
		t.Fatal("one 401 (a restart) must be ridden out")
	}
	stub.set("proxy-a?u1", answerNode(n))
	d.Refresh(context.Background())
	stub.set("proxy-a?u1", answerStatus(http.StatusUnauthorized))
	d.Refresh(context.Background())
	if stopped(rt) {
		t.Fatal("401s that are not consecutive must not stop the node")
	}
	d.Refresh(context.Background())
	if !stopped(rt) || len(d.runtimes) != 0 {
		t.Error("a host token rejected twice in a row must stop every node")
	}
}

func socketIno(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket: %v", path, fi.Mode())
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s mode %v", path, fi.Mode().Perm())
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func dials(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

func TestScanUsersKeepsSockets(t *testing.T) {
	users := t.TempDir()
	os.Mkdir(filepath.Join(users, "alice"), 0o755)
	os.Mkdir(filepath.Join(users, "Not-a-user"), 0o755)
	d, _ := testDaemon(t, Options{UsersDir: users})
	sock := filepath.Join(users, "alice", SocketName)

	d.ScanUsers()
	ino := socketIno(t, sock)
	if !dials(sock) {
		t.Fatal("no socket served")
	}
	d.ScanUsers()
	if socketIno(t, sock) != ino {
		t.Error("a socket in place must be kept")
	}

	// Removed: made again, and closing the old listener leaves the new file.
	os.Remove(sock)
	d.ScanUsers()
	d.ScanUsers()
	if !dials(sock) {
		t.Error("the user was left without a socket")
	}

	// Replaced by a symlink: replaced again, its target untouched.
	target := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(target, []byte("x"), 0o644)
	os.Remove(sock)
	if err := os.Symlink(target, sock); err != nil {
		t.Fatal(err)
	}
	d.ScanUsers()
	socketIno(t, sock)
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Errorf("followed the symlink: target mode %v", fi.Mode().Perm())
	}
	if !dials(sock) {
		t.Error("no socket after replacing the symlink")
	}

	// A directory there is refused, not removed.
	os.Remove(sock)
	os.Mkdir(sock, 0o755)
	d.ScanUsers()
	if fi, err := os.Lstat(sock); err != nil || !fi.IsDir() {
		t.Error("a directory at the socket path must be left alone")
	}
	os.Remove(sock)
	d.ScanUsers()
	if !dials(sock) {
		t.Error("no socket once the directory is gone")
	}

	// The user's directory goes: their listener closes.
	os.RemoveAll(filepath.Join(users, "alice"))
	d.ScanUsers()
	if len(d.sockets) != 0 {
		t.Error("listener kept for a removed user")
	}
	if entries, _ := os.ReadDir(filepath.Join(users, privateSockDir)); len(entries) != 0 {
		t.Errorf("temporary sockets left behind: %v", entries)
	}
}
