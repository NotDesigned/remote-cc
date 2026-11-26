package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A user is root in their container, and some host dirs are mounted into it
// writable. These tests plant symlinks there, aimed at host files, and check
// that the server scripts (root on the host) never write, chmod or read
// through them.

// attackHost is a host where alice planted symlinks in every dir of hers
// that is mounted into her container.
type attackHost struct {
	*fakeHost
	secret, victim, victimDir string
}

func newAttackHost(t *testing.T) *attackHost {
	h := &attackHost{fakeHost: newFakeHost(t, "alice")}
	root := filepath.Dir(h.data)
	h.secret = filepath.Join(root, "shadow")
	h.victim = filepath.Join(root, "victim")
	h.victimDir = filepath.Join(root, "victimdir")
	h.write(h.secret, "root:$6$hostshadowhash:20000:0:99999:7:::\n")
	os.Chmod(h.secret, 0o600)
	h.write(h.victim, "host file\n")
	os.Chmod(h.victim, 0o600)
	os.MkdirAll(h.victimDir, 0o755)
	os.Chmod(h.victimDir, 0o755)
	link := func(target, at string) {
		os.MkdirAll(filepath.Dir(at), 0o755)
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	link(h.victim, filepath.Join(h.data, "keys/alice/authorized_keys"))
	link(h.victimDir, filepath.Join(h.data, "users/alice/root/.ssh"))
	link(h.secret, filepath.Join(h.data, "users/alice/ssh/ssh_host_ed25519_key.pub"))
	return h
}

// untouched fails if anything was written, chmod-ed or leaked through the
// planted symlinks.
func (h *attackHost) untouched(out string) {
	h.t.Helper()
	if got := h.read(h.victim); got != "host file\n" {
		h.t.Errorf("wrote through a symlink into a host file: %q", got)
	}
	for p, mode := range map[string]os.FileMode{h.victim: 0o600, h.secret: 0o600, h.victimDir: 0o755} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != mode {
			h.t.Errorf("%s: mode changed through a symlink: %v", p, err)
		}
	}
	if strings.Contains(out, "hostshadowhash") {
		h.t.Errorf("a host file leaked into the output: %q", out)
	}
}

// inContainer reads a file as the fake container sees it.
func (h *attackHost) inContainer(c, p string) string {
	return h.read(filepath.Join(h.fake, "c", c, "fs", p))
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMaUqq7Jwb3cQ8BLc1cmqXEx2lrnm3w6LOCwH8xBRdF"

func TestUserAddDoesNotFollowSymlinks(t *testing.T) {
	h := newAttackHost(t)
	key := testKey + " laptop"
	params := []string{"NAME=alice", "KEYS_B64=" + b64(key), "COMPOSE_B64=" + b64("user compose\n")}
	out, err := h.run("server/user-add.sh", "", params...)
	if err != nil {
		t.Fatal(err)
	}
	h.untouched(out)
	if got := h.inContainer("rcc-alice", "userkeys/authorized_keys"); got != key+"\n" {
		t.Fatalf("key not added inside the container: %q", got)
	}
	if !strings.Contains(out, "keys=1\n") || !strings.Contains(out, "ukey=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFAKE") {
		t.Fatalf("output: %q", out)
	}
	// Again: nothing added twice.
	if _, err := h.run("server/user-add.sh", "", params...); err != nil {
		t.Fatal(err)
	}
	if got := h.inContainer("rcc-alice", "userkeys/authorized_keys"); got != key+"\n" {
		t.Fatalf("key added twice: %q", got)
	}
}

func TestUserInviteInfoListDoNotFollowSymlinks(t *testing.T) {
	h := newAttackHost(t)
	// The expired invite is dropped on the way.
	h.write(filepath.Join(h.fake, "c/rcc-alice/fs/userkeys/authorized_keys"),
		"ssh-ed25519 AAAAkept me\nrestrict,expiry-time=\"200001010000\" ssh-ed25519 AAAAold rcc-invite-0000000000000000\n")
	out, err := h.run("server/user-invite.sh", "", "NAME=alice", "PUB="+testKey, "ID=0123456789abcdef", "TTL=24")
	if err != nil {
		t.Fatal(err)
	}
	h.untouched(out)
	ak := h.inContainer("rcc-alice", "userkeys/authorized_keys")
	if !strings.HasPrefix(ak, "ssh-ed25519 AAAAkept me\nrestrict,command=\"/usr/local/bin/rcc-enroll 0123456789abcdef\",expiry-time=\"") ||
		strings.Contains(ak, "AAAAold") || !strings.HasSuffix(ak, testKey+" rcc-invite-0123456789abcdef\n") {
		t.Fatalf("container half: %q", ak)
	}
	if gw := h.inContainer("gw", "invites/alice"); !strings.Contains(gw, `permitopen="rcc-alice:22"`) || !strings.Contains(gw, testKey) {
		t.Fatalf("gateway half: %q", gw)
	}
	for _, s := range []string{"server/user-info.sh", "server/user-list.sh"} {
		out, err := h.run(s, "", "NAME=alice")
		if err != nil {
			t.Fatal(s, err)
		}
		h.untouched(out)
		if s == "server/user-list.sh" && !strings.Contains(out, "user=alice||1|0|") {
			t.Fatalf("user list: %q", out)
		}
	}
	// Bad parameters never reach a key line.
	if _, err := h.run("server/user-invite.sh", "", "NAME=alice", "PUB="+testKey, "ID=../../x", "TTL=24"); err == nil {
		t.Fatal("bad invite id accepted")
	}
}

func TestInviteStartsStoppedContainer(t *testing.T) {
	h := newFakeHost(t, "alice")
	h.container("rcc-alice", map[string]string{"running": "false"})
	out, err := h.run("server/user-invite.sh", "", "NAME=alice", "PUB="+testKey, "ID=0123456789abcdef", "TTL=1")
	if err != nil || !strings.Contains(out, "exp=") || h.read(filepath.Join(h.fake, "c/rcc-alice/running")) != "true\n" {
		t.Fatalf("stopped container not started: %v %q", err, out)
	}
}

func TestGatewayUserListIsNotFollowedOrTrusted(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	victim := filepath.Join(filepath.Dir(h.data), "victim")
	h.write(victim, "alice\n../../etc\nbob\n")
	users := filepath.Join(h.data, "gateway/etc/users")
	os.Remove(users)
	if err := os.Symlink(victim, users); err != nil {
		t.Fatal(err)
	}
	out, err := h.run("server/user-list.sh", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "etc") || !strings.Contains(out, "user=bob|") {
		t.Fatalf("user list: %q", out)
	}
	if _, err := h.run("server/user-remove.sh", "", "NAME=bob", "PURGE=0", "FORCE=0"); err != nil {
		t.Fatal(err)
	}
	if h.read(victim) != "alice\n../../etc\nbob\n" {
		t.Fatalf("wrote through the symlink: %q", h.read(victim))
	}
	if fi, err := os.Lstat(users); err != nil || fi.Mode()&os.ModeSymlink != 0 || h.read(users) != "alice\n" {
		t.Fatalf("users file: %v %q", err, h.read(users))
	}
}
