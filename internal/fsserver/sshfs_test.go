//go:build linux

package fsserver

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mountScript runs `sshfs -o slave` over fds 3/4 and mounts root *onto the
// same path* inside a private mount namespace — exactly the B-side layout —
// then runs body inside the mount and unmounts.
func runInMount(t *testing.T, root, body string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for unshare -m")
	}
	for _, b := range []string{"sshfs", "unshare", "fusermount3"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}

	script := `
set -e
R="$1"
sshfs -f -o slave,uid=$(id -u),gid=$(id -g),attr_timeout=0,entry_timeout=0,negative_timeout=0 ":$R" "$R" <&3 >&4 3<&- 4>&- &
for i in $(seq 100); do grep -q " $R fuse.sshfs " /proc/self/mounts && break; sleep 0.05; done
grep -q " $R fuse.sshfs " /proc/self/mounts || { echo "mount failed" >&2; exit 99; }
exec 3<&- 4>&-
cd "$R"
set +e
( set -e; ` + body + ` )
rc=$?
cd /
fusermount3 -u "$R"
wait
exit $rc
`
	// fd3: sshfs reads SFTP responses; fd4: sshfs writes SFTP requests.
	toSshfsR, toSshfsW, _ := os.Pipe()
	fromSshfsR, fromSshfsW, _ := os.Pipe()
	cmd := exec.Command("unshare", "-m", "--propagation", "private", "bash", "-c", script, "bash", root)
	cmd.ExtraFiles = []*os.File{toSshfsR, fromSshfsW}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	toSshfsR.Close()
	fromSshfsW.Close()

	done := make(chan error, 1)
	go func() { done <- Serve(pipeRW{fromSshfsR, toSshfsW}, root) }()
	err := cmd.Wait()
	toSshfsW.Close()
	if err != nil {
		t.Fatalf("mount script failed: %v\n%s", err, out.String())
	}
	if serr := <-done; serr != nil {
		t.Logf("server: %v", serr)
	}
	return out.String()
}

type pipeRW struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (p pipeRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeRW) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeRW) Close() error                { p.w.Close(); return p.r.Close() }

func TestSameMountOps(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(root, "from-a.txt"), []byte("hello from A\n"), 0o644)
	big := make([]byte, 3<<20)
	rand.Read(big)
	os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644)

	out := runInMount(t, root, `
cat from-a.txt
cp big.bin big-copy.bin
echo written-on-b > new.txt
mkdir -p d/e
echo v1 > d/e/f && echo v2 > d/e/g && mv -f d/e/g d/e/f
ln -s e/f d/link
cat d/link
printf 'abcdef' > trunc && truncate -s 3 trunc
chmod 755 new.txt
echo appended >> new.txt
touch -d '2001-02-03 04:05:06' new.txt
rm from-a.txt
ls d/e
df . >/dev/null && echo df-ok
`)
	for _, want := range []string{"hello from A", "v2", "f\n", "df-ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Every change must be visible on the real directory (A's view).
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(root, n)); return string(b) }
	if read("new.txt") != "written-on-b\nappended\n" {
		t.Errorf("new.txt = %q", read("new.txt"))
	}
	if got, _ := os.ReadFile(filepath.Join(root, "big-copy.bin")); !bytes.Equal(got, big) {
		t.Error("big file corrupted")
	}
	if read("d/e/f") != "v2\n" || read("trunc") != "abc" {
		t.Errorf("rename/truncate wrong: f=%q trunc=%q", read("d/e/f"), read("trunc"))
	}
	if tgt, _ := os.Readlink(filepath.Join(root, "d/link")); tgt != "e/f" {
		t.Errorf("symlink target = %q (must stay relative)", tgt)
	}
	if fi, _ := os.Stat(filepath.Join(root, "new.txt")); fi.Mode().Perm() != 0o755 || fi.ModTime().Year() != 2001 {
		t.Errorf("chmod/utimes not applied: %v %v", fi.Mode(), fi.ModTime())
	}
	if _, err := os.Stat(filepath.Join(root, "from-a.txt")); !os.IsNotExist(err) {
		t.Error("rm not applied")
	}
}

func TestGitInMount(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	runInMount(t, root, `
git init -q -b main .
echo x > a && git add a
git -c user.name=t -c user.email=t@t commit -qm init
echo y >> a && git commit -qam second -c user.name=t -c user.email=t@t 2>/dev/null || git -c user.name=t -c user.email=t@t commit -qam second
`)
	out, err := exec.Command("git", "-C", root, "log", "--oneline").CombinedOutput()
	if err != nil || strings.Count(string(out), "\n") != 2 {
		t.Fatalf("git log on A: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "fsck").CombinedOutput(); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

func TestOutsideRootRefused(t *testing.T) {
	h := &handler{root: "/srv/proj", goos: "linux"}
	for _, p := range []string{"/srv", "/srv/projX", "/srv/proj/../other", "/etc/passwd"} {
		if _, err := h.local(p); err == nil {
			t.Errorf("%s should be refused", p)
		}
	}
	for _, p := range []string{"/srv/proj", "/srv/proj/a/b", "/srv/proj/a/../b"} {
		if _, err := h.local(p); err != nil {
			t.Errorf("%s should be allowed: %v", p, err)
		}
	}
}
