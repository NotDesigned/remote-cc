package scripts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Deferred updates (lib/docker.sh): server update never cuts a session off.

func newImage(h *fakeHost) { h.write(filepath.Join(h.fake, "img/srv_1"), "sha:2") }

// withWorker adds WORKER_B64 and fake launchers that log the start.
func withWorker(h *fakeHost, params []string) []string {
	for _, l := range []string{"systemd-run", "setsid"} {
		h.write(filepath.Join(h.bin, l), "#!/bin/sh\necho \"$*\" >> \"$FAKE/worker.log\"\n")
		os.Chmod(filepath.Join(h.bin, l), 0o755)
	}
	return append(params, "WORKER_B64="+b64("echo worker\n"))
}

func TestUpdateDefersBusyUsers(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	h.write(filepath.Join(h.fake, "c/rcc-alice/sessions"), "2\n")
	newImage(h)
	out, err := h.run("server/update.sh", "", withWorker(h, updateParams())...)
	if err != nil {
		t.Fatalf("update failed (it must not refuse): %v", err)
	}
	log := h.composeLog()
	for _, want := range []string{"gateway=updated", "updated=bob\n", "pending=alice(2)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if !strings.Contains(log, h.dir+" compose up -d") {
		t.Errorf("gateway not recreated while a session runs: %q", log)
	}
	if !strings.Contains(log, filepath.Join(h.dir, "users/bob")+" compose up -d --force-recreate") || strings.Contains(log, "users/alice") {
		t.Errorf("want bob recreated and alice left alone: %q", log)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "users/alice/update-pending")); err != nil {
		t.Error("alice not marked pending")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "users/bob/update-pending")); err == nil {
		t.Error("bob marked pending")
	}
	if !strings.Contains(h.read(filepath.Join(h.fake, "exec.log")), "rcc-alice touch /run/rcc-update-pending") {
		t.Error("alice's container not told about the pending update")
	}
	if !strings.Contains(h.read(filepath.Join(h.fake, "claim.log")), "rcc-bob") {
		t.Error("bob recreated without claiming his container first")
	}
	if w := h.read(filepath.Join(h.fake, "worker.log")); !strings.Contains(w, "pending-worker.sh") {
		t.Errorf("worker not started: %q", w)
	}
	if h.read(filepath.Join(h.dir, "pending-worker.sh")) != "echo worker\n" {
		t.Error("worker script not written")
	}
}

// A user whose compose file is already the new one (written when it was
// marked) is still recreated on the next update once idle.
func TestUpdateAppliesPendingWhenIdle(t *testing.T) {
	h := newFakeHost(t, "alice")
	h.write(filepath.Join(h.fake, "c/rcc-alice/sessions"), "1\n")
	p := updateParams()
	p[3] = "COMPOSE_USER_B64=" + b64("user compose v2\n")
	if out, err := h.run("server/update.sh", "", p...); err != nil || !strings.Contains(out, "pending=alice(1)") {
		t.Fatalf("not pending: %q %v", out, err)
	}
	if h.read(filepath.Join(h.dir, "users/alice/compose.yml")) != "user compose v2\n" {
		t.Error("the new compose file was not written for the pending user")
	}
	h.write(filepath.Join(h.fake, "c/rcc-alice/sessions"), "0\n")
	out, err := h.run("server/update.sh", "", p...)
	if err != nil || !strings.Contains(out, "updated=alice") || strings.Contains(out, "uptodate=1") {
		t.Fatalf("pending user not applied: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "users/alice/update-pending")); err == nil {
		t.Error("pending mark left behind")
	}
	if out, _ := h.run("server/update.sh", "", p...); !strings.Contains(out, "uptodate=1") {
		t.Errorf("want up to date after applying: %q", out)
	}
}

// --image switches the gateway and every user (their .env names it too).
func TestUpdateSwitchesImage(t *testing.T) {
	h := newFakeHost(t, "alice")
	h.write(filepath.Join(h.fake, "img/srv_2"), "sha:2")
	p := updateParams()
	p[0] = "IMAGE=srv:2"
	out, err := h.run("server/update.sh", "", p...)
	if err != nil || !strings.Contains(out, "updated=alice") {
		t.Fatalf("%q %v", out, err)
	}
	for _, f := range []string{".env", "users/alice/.env"} {
		if env := h.read(filepath.Join(h.dir, f)); strings.Count(env, "RCC_IMAGE=") != 1 || !strings.Contains(env, "RCC_IMAGE=srv:2\n") {
			t.Errorf("%s: %q", f, env)
		}
	}
}

func TestUpdateForceRecreatesBusyUsers(t *testing.T) {
	h := newFakeHost(t, "alice")
	h.write(filepath.Join(h.fake, "c/rcc-alice/sessions"), "1\n")
	newImage(h)
	p := updateParams()
	p[1] = "FORCE=1"
	out, err := h.run("server/update.sh", "", p...)
	if err != nil || !strings.Contains(out, "updated=alice") || strings.Contains(out, "pending=") {
		t.Fatalf("--force: %q %v", out, err)
	}
}

func TestPendingWorker(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	egressHost(t, h, "sha:e1")
	for _, u := range []string{"alice", "bob"} {
		h.write(filepath.Join(h.dir, "users", u, "update-pending"), "1\n")
	}
	h.write(filepath.Join(h.dir, "egress/update-pending"), "1\n")
	h.write(filepath.Join(h.fake, "c/rcc-alice/sessions"), "1\n")
	h.write(filepath.Join(h.fake, "c/rcc-alice/rcb"), "1\n")
	// Each wait between rounds: alice's session ends after the first.
	h.write(filepath.Join(h.bin, "sleep"), "#!/bin/sh\necho \"$1\" >> \"$FAKE/sleep.log\"\n"+
		"echo 0 > \"$FAKE/c/rcc-alice/sessions\"; echo 0 > \"$FAKE/c/rcc-alice/rcb\"\n"+
		"[ \"$(wc -l < \"$FAKE/sleep.log\")\" -lt 5 ] || kill $PPID\n")
	os.Chmod(filepath.Join(h.bin, "sleep"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "set -e\n"+Join("lib/docker.sh", "server/pending.sh"))
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "FAKE="+h.fake, "DIR="+h.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
	if n := strings.Count(h.read(filepath.Join(h.fake, "sleep.log")), "\n"); n != 1 {
		t.Errorf("want one wait (alice busy, then done), got %d", n)
	}
	log := h.composeLog()
	for _, d := range []string{"users/alice", "users/bob", "egress"} {
		if !strings.Contains(log, filepath.Join(h.dir, d)+" compose up -d") {
			t.Errorf("%s not recreated: %q", d, log)
		}
	}
	// bob (idle) in the first round, alice only after her session ended.
	if i, j := strings.Index(log, "users/bob"), strings.Index(log, "users/alice"); i > j {
		t.Errorf("order: %q", log)
	}
	for _, f := range []string{"users/alice/update-pending", "users/bob/update-pending", "egress/update-pending"} {
		if _, err := os.Stat(filepath.Join(h.dir, f)); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
	if !strings.Contains(string(out), "rcc-alice: updated") {
		t.Errorf("worker log: %s", out)
	}
}

func TestUserListShowsPending(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	h.write(filepath.Join(h.dir, "users/alice/update-pending"), "1\n")
	h.write(filepath.Join(h.dir, "egress/update-pending"), "1\n")
	out, err := h.run("server/user-list.sh", "", withWorker(h, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "user=alice|") || !strings.HasSuffix(strings.SplitN(out[strings.Index(out, "user=alice|"):], "\n", 2)[0], "|pending") {
		t.Errorf("alice not pending: %q", out)
	}
	if !strings.Contains(out, "|-\n") || !strings.Contains(out, "egress=pending\n") {
		t.Errorf("bob / egress: %q", out)
	}
	if !strings.Contains(h.read(filepath.Join(h.fake, "worker.log")), "pending-worker.sh") {
		t.Error("user list does not restart the worker")
	}
}

// runDir is a container's /run and /root/.cache/rcc/run moved into temp dirs.
type runDir struct{ run, cache string }

func newRunDir(t *testing.T) runDir {
	return runDir{t.TempDir(), t.TempDir()}
}

func (r runDir) rewrite(s string) string {
	s = strings.ReplaceAll(s, "/root/.cache/rcc/run/", r.cache+"/run/")
	s = strings.ReplaceAll(s, "/run/rcc-", r.run+"/rcc-")
	return strings.ReplaceAll(s, " /run ]", " "+r.run+" ]")
}

// session starts a live session daemon stand-in (its command line names
// its run dir) and returns the run dir.
func (r runDir) session(t *testing.T, name, launch string) string {
	d := filepath.Join(r.cache, "run", name)
	os.MkdirAll(d, 0o700)
	c := exec.Command("sh", "-c", "sleep 30; :", "rcc-shim", d)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	os.WriteFile(filepath.Join(d, "daemon.pid"), []byte(strconv.Itoa(c.Process.Pid)+"\n"), 0o600)
	os.WriteFile(filepath.Join(d, "launch.sh"), []byte(launch), 0o600)
	return d
}

func needsFlock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("needs flock")
	}
}

func shOut(script string) (string, int) {
	out, err := exec.Command("sh", "-c", script).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

func TestClaimCountsSessions(t *testing.T) {
	needsFlock(t)
	r := newRunDir(t)
	claim := r.rewrite(scriptFn(t, "claim_sh"))
	r.session(t, "a", "RCB_NODE='n1'\n")
	r.session(t, "b", "R='/x'\n")
	os.MkdirAll(filepath.Join(r.cache, "run", "dead"), 0o700)
	os.WriteFile(filepath.Join(r.cache, "run", "dead", "daemon.pid"), []byte("999999\n"), 0o600)
	if out, code := shOut(claim); code != 1 || out != "2\n" {
		t.Fatalf("claim with 2 sessions: %q exit %d", out, code)
	}
	if _, err := os.Stat(filepath.Join(r.run, "rcc-updating")); err == nil {
		t.Fatal("claimed a busy container")
	}
	if out, _ := shOut(r.rewrite(scriptFn(t, "count_sh")) + "\necho $n $r"); out != "2 1\n" {
		t.Errorf("count: %q, want 2 sessions, 1 with --rcb", out)
	}
	r2 := newRunDir(t)
	if out, code := shOut(r2.rewrite(scriptFn(t, "claim_sh"))); code != 0 || out != "" {
		t.Fatalf("claim of an idle container: %q exit %d", out, code)
	}
	if _, err := os.Stat(filepath.Join(r2.run, "rcc-updating")); err != nil {
		t.Fatal("no updating mark")
	}
}

// scriptFn prints a lib/docker.sh function's output (count_sh, claim_sh).
func scriptFn(t *testing.T, fn string) string {
	t.Helper()
	// docker.sh checks for docker first; any command will do.
	cmd := exec.Command("sh", "-c", "docker() { :; }\n"+Get("lib/docker.sh")+"\n"+fn)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// sessionStart runs client/session.sh until it fails for want of rcc-shim
// (exit 1) or is turned away by an update (exit 75).
func (r runDir) sessionStart(t *testing.T) (string, int) {
	cmd := exec.Command("sh", "-c", r.rewrite(Client("client/session.sh")))
	cmd.Env = append(os.Environ(), "CACHE="+r.cache, "RUN="+filepath.Join(r.cache, "run", "new"), "R=/tmp/x",
		"SHIM_SHA=none", "LAUNCH_B64=eA==", "PROF=", "CFG="+t.TempDir(), "HOME="+t.TempDir())
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return errb.String(), ee.ExitCode()
	}
	return errb.String(), 0
}

func TestSessionStartWaitsForAndHonoursAClaim(t *testing.T) {
	needsFlock(t)
	r := newRunDir(t)
	// No claim: the start goes on (and fails later: there is no rcc-shim).
	if e, code := r.sessionStart(t); code != 1 || !strings.Contains(e, "rcc-shim is missing") {
		t.Fatalf("unclaimed start: exit %d %q", code, e)
	}
	os.RemoveAll(filepath.Join(r.cache, "run", "new"))
	// An update holds the lock, then claims: the start waits, then stays away.
	mark := filepath.Join(r.run, "rcc-updating")
	hold := exec.Command("sh", "-c", "exec 9>\"$1/rcc-start.lock\"; flock 9; touch \"$1/locked\"; sleep 1; date > \"$2\"", "sh", r.run, mark)
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer hold.Wait()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(r.run, "locked")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	e, code := r.sessionStart(t)
	if code != 75 || !strings.Contains(e, "rcc-container-updating") {
		t.Fatalf("claimed container: exit %d %q", code, e)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Error("the start did not wait for the lock")
	}
	if _, err := os.Stat(filepath.Join(r.cache, "run", "new")); err == nil {
		t.Error("a turned-away start created its run dir")
	}
	// A mark left by a failed update (over 5 minutes old) is ignored.
	old := time.Now().Add(-10 * time.Minute)
	os.Chtimes(mark, old, old)
	if e, code := r.sessionStart(t); code != 1 || !strings.Contains(e, "rcc-shim is missing") {
		t.Fatalf("stale mark: exit %d %q", code, e)
	}
}
