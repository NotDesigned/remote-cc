//go:build linux

package shim

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-cc/internal/agent"
	"remote-cc/internal/fsserver"
)

// The test binary doubles as the shim: run as "rg" (a symlink to it), it is
// shim.Rg, as rcc-shim is.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "rg" {
		os.Exit(Rg(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type rgEnv struct {
	root, run, shim string
	log             *syncBuffer
	env             []string
	conns           atomic.Int32 // connections to A
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newRgEnv starts an agent (A) on RUN/s, reached through a link that delays
// each direction by oneWay, and makes the test binary the shim as rg/rg.
func newRgEnv(t testing.TB, root string, oneWay time.Duration) *rgEnv {
	realRg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("no rg")
	}
	e := &rgEnv{root: root, run: t.TempDir(), log: &syncBuffer{}}
	a, err := agent.New(agent.Options{Token: "tok", TempDir: t.TempDir(), Logger: log.New(e.log, "", 0)})
	if err != nil {
		t.Skipf("no shell: %v", err)
	}
	direct := filepath.Join(e.run, "agent.sock")
	l, err := net.Listen("unix", direct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go a.Serve(l)
	sock := filepath.Join(e.run, "s")
	pl, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pl.Close() })
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			e.conns.Add(1)
			go func() {
				d, err := net.Dial("unix", direct)
				if err != nil {
					c.Close()
					return
				}
				go delayCopy(d, c, oneWay)
				delayCopy(c, d, oneWay)
			}()
		}
	}()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(e.run, "rg"), 0o755)
	e.shim = filepath.Join(e.run, "rg", "rg")
	if err := os.Symlink(self, e.shim); err != nil {
		t.Fatal(err)
	}
	e.env = append(os.Environ(), EnvSock+"=unix:"+sock, EnvToken+"=tok", EnvRoot+"="+root, EnvRealRg+"="+realRg)
	return e
}

// delayCopy copies src to dst, delivering each chunk d after it was read.
func delayCopy(dst io.WriteCloser, src io.Reader, d time.Duration) {
	defer dst.Close()
	if d == 0 {
		io.Copy(dst, src)
		return
	}
	type chunk struct {
		at time.Time
		b  []byte
	}
	ch := make(chan chunk, 1<<12)
	go func() {
		defer close(ch)
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				ch <- chunk{time.Now().Add(d), append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range ch {
		time.Sleep(time.Until(c.at))
		if _, err := dst.Write(c.b); err != nil {
			return
		}
	}
}

type rgRun struct {
	out, errOut string
	code        int
}

func runRg(t testing.TB, bin, dir string, env []string, args ...string) rgRun {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = dir, env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return rgRun{out.String(), errOut.String(), code}
}

// normalize sorts rg's output lines (its parallel walk has no fixed order)
// and drops --json's timing lines.
func normalize(out string) string {
	lines := strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == 0 })
	keep := lines[:0]
	for _, l := range lines {
		if !strings.Contains(l, `"elapsed"`) {
			keep = append(keep, l)
		}
	}
	sort.Strings(keep)
	return strings.Join(keep, "\n")
}

func makeProject(t testing.TB, dirs, files int) string {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	for d := 0; d < dirs; d++ {
		p := filepath.Join(root, "pkg"+strconv.Itoa(d), "sub")
		os.MkdirAll(p, 0o755)
		for f := 0; f < files; f++ {
			body := fmt.Sprintf("package p%d\n\nfunc F%d() int { return %d }\n", d, f, f)
			if d == dirs/2 && f == files/2 {
				body += "// NeedleFunc is here\n"
			}
			os.WriteFile(filepath.Join(p, fmt.Sprintf("f%d.go", f)), []byte(body), 0o644)
		}
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644)
	return root
}

// Claude Code 2.1.325's own argument lists (see the rg shim commit).
var (
	claudeGrep = []string{"--hidden", "--glob", "!.git", "--glob", "!.svn", "--glob", "!.hg", "--glob", "!.bzr",
		"--glob", "!.jj", "--glob", "!.sl", "--max-columns", "500", "-l", "--null", "NeedleFunc", "."}
	claudeContent = []string{"--hidden", "--glob", "!.git", "--max-columns", "500", "--json", "-n", "-C", "1", "Needle", "."}
	claudeGlob    = []string{"--files", "--null", "--glob", "**/*.go", "--sort=modified", "--no-ignore", "--hidden", "."}
)

func TestRgShimEndToEnd(t *testing.T) {
	root := makeProject(t, 3, 4)
	e := newRgEnv(t, root, 0)
	realRg, _ := exec.LookPath("rg")
	runs := func() int { return strings.Count(e.log.String(), "cmd=rg ") }

	check := func(name, dir string, onA bool, args ...string) {
		t.Helper()
		before := runs()
		got := runRg(t, e.shim, dir, e.env, args...)
		want := runRg(t, realRg, dir, os.Environ(), args...)
		if got.code != want.code || got.errOut != want.errOut || normalize(got.out) != normalize(want.out) {
			t.Errorf("%s: shim %+v, rg %+v", name, got, want)
		}
		if ran := runs() > before; ran != onA {
			t.Errorf("%s: ran on A = %v, want %v\n%s", name, ran, onA, e.log.String())
		}
	}
	check("grep", root, true, claudeGrep...)
	check("grep in a subdirectory", filepath.Join(root, "pkg1"), true, claudeGrep...)
	check("content", root, true, claudeContent...)
	check("glob", root, true, claudeGlob...)
	check("absolute target", "/", true, "--files", "--follow", "--hidden", root)
	check("no match", root, true, "-l", "nothing-matches-this", ".")
	check("bad regex", root, true, "-l", "(", ".")
	check("outside the project", root, false, "-l", "NeedleFunc", t.TempDir())
	check("version", root, false, "--version")

	// No rg on A: the search runs here, and later ones do not ask A again.
	t.Setenv("PATH", "/nonexistent")
	check("no rg on A", root, false, claudeGrep...)
	if _, err := os.Stat(filepath.Join(e.run, "rg-missing")); err != nil {
		t.Fatalf("not remembered: %v", err)
	}
	n := e.conns.Load()
	check("no rg on A, again", root, false, claudeGrep...)
	if e.conns.Load() != n {
		t.Error("asked A again")
	}
}

// TestRgShimWithClaude runs real Claude Code (RCC_TEST_CLAUDE) against
// test/mockapi.py with the environment launch.sh sets, and checks that its
// Grep and Glob tools ran rg on A. The native build offers these tools only
// when asked for (--allowedTools).
func TestRgShimWithClaude(t *testing.T) {
	claude := os.Getenv("RCC_TEST_CLAUDE")
	if claude == "" {
		t.Skip("set RCC_TEST_CLAUDE")
	}
	root := makeProject(t, 3, 4)
	e := newRgEnv(t, root, 0)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	results := filepath.Join(t.TempDir(), "results")
	mock := exec.Command("python3", "-I", "../../test/mockapi.py", "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--log", results,
		"--tool", `Grep {"pattern": "NeedleFunc"}`,
		"--tool", `Grep {"pattern": "Needle", "output_mode": "content", "path": "`+root+`/pkg1"}`,
		"--tool", `Glob {"pattern": "pkg2/**/*.go"}`,
		"--tool", `Grep {"pattern": "NeedleFunc", "path": "`+root+`/pkg1/sub/f2.go"}`)
	if err := mock.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mock.Process.Kill(); mock.Wait() })
	time.Sleep(500 * time.Millisecond)
	cmd := exec.Command(claude, "-p", "go", "--dangerously-skip-permissions", "--allowedTools", "Grep,Glob")
	cmd.Dir = root
	cmd.Env = append(e.env, "HOME="+t.TempDir(), "USE_BUILTIN_RIPGREP=0",
		"PATH="+filepath.Dir(e.shim)+":"+os.Getenv("PATH"),
		fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port), "ANTHROPIC_API_KEY=sk-ant-test",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claude: %v\n%s", err, out)
	}
	res, _ := os.ReadFile(results)
	t.Logf("results:\n%s\nagent log:\n%s", res, e.log.String())
	lines := strings.Split(strings.TrimSpace(string(res)), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d results", len(lines))
	}
	for i, want := range []string{`pkg1/sub/f2.go`, `f2.go:4:// NeedleFunc is here`, `pkg2/sub/f0.go`, `Found 1 file`} {
		if !strings.Contains(lines[i], want) || strings.Contains(lines[i], `"is_error": true`) {
			t.Errorf("result %d: %s, want %q", i+1, lines[i], want)
		}
	}
	// Grep, Grep and Glob ran on A; the single file (passed as fd 3) here.
	if n := strings.Count(e.log.String(), "cmd=rg "); n != 3 {
		t.Errorf("%d searches on A, want 3", n)
	}
}

// TestRgLatency compares a search through sshfs (what Claude Code did
// before) with the shim, over a link of RCC_BENCH_RTT (e.g. 100ms). It
// needs root (a mount namespace), sshfs and fusermount3; run it in the
// server image:
//
//	go test -c -o rgtest ./internal/shim
//	docker run --rm --device /dev/fuse --cap-add SYS_ADMIN -e RCC_BENCH_RTT=100ms \
//	  -v $PWD/rgtest:/rgtest IMAGE /rgtest -test.run RgLatency -test.v
func TestRgLatency(t *testing.T) {
	rtt, err := time.ParseDuration(os.Getenv("RCC_BENCH_RTT"))
	if err != nil {
		t.Skip("set RCC_BENCH_RTT")
	}
	for _, b := range []string{"sshfs", "unshare", "fusermount3", "rg"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dirs, files := 50, 40 // 2000 files in 100 directories
	if v := os.Getenv("RCC_BENCH_FILES"); v != "" {
		files, _ = strconv.Atoi(v)
	}
	root := makeProject(t, dirs, files)
	searches := []struct {
		name string
		args []string
	}{{"Grep", claudeGrep}, {"Grep-again", claudeGrep}, {"Grep-content", claudeContent}, {"Glob", claudeGlob}}

	// Before: rg walks the sshfs mount.
	var script strings.Builder
	script.WriteString(`set -e
R="$1"
sshfs -f -o slave,dir_cache=yes,dcache_timeout=5,attr_timeout=1,entry_timeout=1,negative_timeout=0 ":$R" "$R" <&3 >&4 3<&- 4>&- &
for i in $(seq 200); do grep -q " $R fuse.sshfs " /proc/self/mounts && break; sleep 0.05; done
exec 3<&- 4>&-
cd "$R"
`)
	for _, s := range searches {
		q := make([]string, len(s.args))
		for i, a := range s.args {
			q[i] = "'" + a + "'"
		}
		fmt.Fprintf(&script, "t0=$(date +%%s%%N); rg %s > /dev/null; echo %s $(( ($(date +%%s%%N) - t0) / 1000000 ))\n",
			strings.Join(q, " "), s.name)
	}
	script.WriteString("cd /\nfusermount3 -u \"$R\"\nwait\n")
	toR, toW, _ := os.Pipe()
	fromR, fromW, _ := os.Pipe()
	cmd := exec.Command("unshare", "-m", "--propagation", "private", "bash", "-c", script.String(), "bash", root)
	cmd.ExtraFiles = []*os.File{toR, fromW}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	toR.Close()
	fromW.Close()
	// Server side of the link, delayed rtt/2 each way.
	sr, sw := io.Pipe() // requests after the delay
	rr, rw := io.Pipe() // responses before the delay
	go delayCopy(sw, fromR, rtt/2)
	go delayCopy(toW, rr, rtt/2)
	go fsserver.Serve(pipeRWC{sr, rw}, root)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("sshfs run: %v\n%s", err, out.String())
	}
	t.Logf("through sshfs, RTT %v, %d files:\n%s", rtt, dirs*files, out.String())

	// After: the shim runs rg on A over the same link.
	e := newRgEnv(t, root, rtt/2)
	for _, s := range searches {
		t0 := time.Now()
		r := runRg(t, e.shim, root, e.env, s.args...)
		if r.code != 0 {
			t.Fatalf("%s: %+v", s.name, r)
		}
		t.Logf("shim: %s %d ms", s.name, time.Since(t0).Milliseconds())
	}
}

type pipeRWC struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (p pipeRWC) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeRWC) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeRWC) Close() error                { p.w.Close(); return p.r.Close() }
