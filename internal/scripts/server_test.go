package scripts

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeDocker answers the docker calls of the server scripts from files in
// $FAKE/c/<container>/<field> and $FAKE/img/<image>, and logs every
// "compose up" with its directory to $FAKE/compose.log.
const fakeDocker = `#!/bin/sh
st="$FAKE/c"
case "$1" in
compose)
  [ "$2" = version ] && exit 0
  echo "$(pwd) $*" >> "$FAKE/compose.log"; exit 0 ;;
inspect)
  if [ "$2" = -f ]; then f=$3; c=$4; else f=""; c=$2; fi
  [ -d "$st/$c" ] || exit 1
  case "$f" in
    "") echo '[]' ;;
    *working_dir*) cat "$st/$c/wd" 2>/dev/null || true ;;
    *compose.project*) cat "$st/$c/project" 2>/dev/null || true ;;
    *State.Running*) cat "$st/$c/running" 2>/dev/null || echo true ;;
    *Mounts*) cat "$st/$c/egress" 2>/dev/null || true ;;
    "{{.Image}}") cat "$st/$c/image" ;;
    "{{.Id}}") echo "id-$c" ;;
  esac ;;
image)
  [ "$2" = inspect ] || exit 0
  cat "$FAKE/img/$(echo "$5" | tr '/:' '__')" 2>/dev/null || exit 1 ;;
pull) echo "$2 $3" >> "$FAKE/pull.log" ;;
start) echo true > "$st/$2/running" ;;
exec)
  shift; in=""; [ "$1" = -i ] && { in=1; shift; }
  c=$1; shift
  [ "$(cat "$st/$c/running" 2>/dev/null || echo true)" = true ] || { echo "fake: $c is not running" >&2; exit 1; }
  echo "$c $*" >> "$FAKE/exec.log"
  case "$1" in
  claude) echo 2.0.0 ;;
  ssh-keyscan) echo "127.0.0.1 ssh-ed25519 AAAA" ;;
  date) shift; date "$@" ;;
  sh)
    # sh -c SCRIPT [ARG0 ARGS…]: active_sessions answers from a file; other
    # scripts run here, with the container's paths moved into $st/<c>/fs
    # (never into the host dirs the tests attack).
    script=$3; shift 3
    case "$script" in *"kill -0"*) cat "$st/$c/sessions" 2>/dev/null || echo 0; exit 0 ;; esac
    fs="$st/$c/fs"
    mkdir -p "$fs/userkeys" "$fs/sshkeys" "$fs/invites"
    [ -f "$fs/sshkeys/ssh_host_ed25519_key.pub" ] ||
      echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE root@$c" > "$fs/sshkeys/ssh_host_ed25519_key.pub"
    script=$(printf '%s' "$script" | sed -e "s#/etc/remote-cc/userkeys#$fs/userkeys#g" -e "s#/etc/ssh/keys#$fs/sshkeys#g" -e "s#/etc/remote-cc/invites#$fs/invites#g")
    for a in "$@"; do
      set -- "$@" "$(printf '%s' "$a" | sed -e "s#/etc/remote-cc/userkeys#$fs/userkeys#g" -e "s#/etc/remote-cc/invites#$fs/invites#g")"
      shift
    done
    if [ -z "$in" ]; then exec sh -c "$script" "$@" < /dev/null; fi
    exec sh -c "$script" "$@" ;;
  esac ;;
esac
exit 0
`

type fakeHost struct {
	t                    *testing.T
	fake, dir, data, bin string
}

func newFakeHost(t *testing.T, users ...string) *fakeHost {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	root := t.TempDir()
	h := &fakeHost{t: t, fake: filepath.Join(root, "fake"), dir: filepath.Join(root, "opt"),
		data: filepath.Join(root, "data"), bin: filepath.Join(root, "bin")}
	h.write(filepath.Join(h.bin, "docker"), fakeDocker)
	os.Chmod(filepath.Join(h.bin, "docker"), 0o755)
	h.write(filepath.Join(h.dir, "multi-user"), "")
	h.write(filepath.Join(h.dir, ".env"), "RCC_DATA="+h.data+"\nRCC_IMAGE=srv:1\nRCC_GATEWAY=gw\nRCC_NETWORK=net\nCOMPOSE_PROJECT_NAME=net\n")
	h.write(filepath.Join(h.dir, "compose.yml"), "gw compose\n")
	h.write(filepath.Join(h.data, "gateway/etc/users"), strings.Join(users, "\n")+"\n")
	h.write(filepath.Join(h.fake, "img/srv_1"), "sha:1")
	h.container("gw", map[string]string{"image": "sha:1", "wd": h.dir})
	for _, u := range users {
		h.write(filepath.Join(h.dir, "users", u, "compose.yml"), "user compose\n")
		h.write(filepath.Join(h.dir, "users", u, ".env"), "RCC_NAME=rcc-"+u+"\nRCC_NETWORK=net\nCOMPOSE_PROJECT_NAME=rcc-"+u+"\n")
		h.container("rcc-"+u, map[string]string{"image": "sha:1", "project": "rcc-" + u,
			"wd": filepath.Join(h.dir, "users", u), "egress": h.egressDir(u)})
	}
	return h
}

func (h *fakeHost) egressDir(u string) string { return filepath.Join(h.data, "egress/users", u) }

func (h *fakeHost) write(p, s string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func (h *fakeHost) container(name string, fields map[string]string) {
	for k, v := range fields {
		h.write(filepath.Join(h.fake, "c", name, k), v)
	}
}

// run runs a server script (as root.sh would: set -e) with params as env.
func (h *fakeHost) run(name, stdin string, params ...string) (string, error) {
	h.t.Helper()
	os.Remove(filepath.Join(h.fake, "compose.log"))
	cmd := exec.Command("sh", "-c", "set -e\n"+Join("lib/docker.sh", name))
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "FAKE="+h.fake, "DIR="+h.dir)
	cmd.Env = append(cmd.Env, params...)
	cmd.Stdin = strings.NewReader(stdin)
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		h.t.Logf("stderr: %s", errb.String())
	}
	return string(out), err
}

func (h *fakeHost) composeLog() string { return h.read(filepath.Join(h.fake, "compose.log")) }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func updateParams() []string {
	return []string{"IMAGE=", "FORCE=0", "COMPOSE_GW_B64=" + b64("gw compose\n"),
		"COMPOSE_USER_B64=" + b64("user compose\n"), "COMPOSE_EGRESS_B64=" + b64("egress compose\n")}
}

func TestUpdateFixesEgressDirEvenWhenUpToDate(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	// bob's .env names a dir the egress host never serves (an older setup).
	h.write(filepath.Join(h.dir, "users/bob/.env"), h.read(filepath.Join(h.dir, "users/bob/.env"))+"RCC_EGRESS_DIR=/var/lib/remote-cc/egress/none\n")
	out, err := h.run("server/update.sh", "", updateParams()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "uptodate=1") || h.composeLog() != "" {
		t.Fatalf("want up to date without compose, got %q / %q", out, h.composeLog())
	}
	for _, u := range []string{"alice", "bob"} {
		env := h.read(filepath.Join(h.dir, "users", u, ".env"))
		if strings.Count(env, "RCC_EGRESS_DIR=") != 1 || !strings.Contains(env, "RCC_EGRESS_DIR="+h.egressDir(u)+"\n") {
			t.Errorf("%s .env: %q", u, env)
		}
		if fi, err := os.Stat(h.egressDir(u)); err != nil || !fi.IsDir() {
			t.Errorf("%s egress dir missing", u)
		}
	}
}

func TestUpdateRecreatesUserWithWrongEgressMount(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	h.container("rcc-bob", map[string]string{"egress": "/var/lib/remote-cc/egress/none"})
	out, err := h.run("server/update.sh", "", updateParams()...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "uptodate=1") || !strings.Contains(h.composeLog(), filepath.Join(h.dir, "users/bob")+" compose up") {
		t.Fatalf("bob not recreated: %q / %q", out, h.composeLog())
	}
}

func egressHost(t *testing.T, h *fakeHost, imageID string) {
	h.write(filepath.Join(h.dir, "egress/.env"), "RCC_EGRESS_NAME=net-egress\nRCC_EGRESS_DATA="+h.data+"/egress\nRCC_EGRESS_RCB_URL=https://rcb\nRCC_EGRESS_IMAGE=eg:latest\nCOMPOSE_PROJECT_NAME=net-egress\n")
	h.write(filepath.Join(h.dir, "egress/token"), "rcch_old\n")
	h.write(filepath.Join(h.dir, "egress/compose.yml"), "egress compose\n")
	h.write(filepath.Join(h.fake, "img/eg_latest"), imageID)
	h.container("net-egress", map[string]string{"image": "sha:e1", "wd": filepath.Join(h.dir, "egress")})
}

func TestUpdateEgressHost(t *testing.T) {
	h := newFakeHost(t, "alice")
	egressHost(t, h, "sha:e1")
	out, err := h.run("server/update.sh", "", updateParams()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "uptodate=1") || !strings.Contains(h.read(filepath.Join(h.fake, "pull.log")), "eg:latest") {
		t.Fatalf("unchanged egress: %q, pulls %q", out, h.read(filepath.Join(h.fake, "pull.log")))
	}
	// A newer :latest was pulled: only the egress host is recreated, with its
	// token and .env kept.
	h.write(filepath.Join(h.fake, "img/eg_latest"), "sha:e2")
	env := h.read(filepath.Join(h.dir, "egress/.env"))
	out, err = h.run("server/update.sh", "", updateParams()...)
	if err != nil {
		t.Fatal(err)
	}
	log := h.composeLog()
	if !strings.Contains(out, "egress=updated") || !strings.Contains(log, filepath.Join(h.dir, "egress")+" compose up -d --pull missing --quiet-pull --force-recreate") {
		t.Fatalf("egress not recreated: %q / %q", out, log)
	}
	if h.read(filepath.Join(h.dir, "egress/token")) != "rcch_old\n" || h.read(filepath.Join(h.dir, "egress/.env")) != env {
		t.Fatal("update changed the egress token or .env")
	}
	// A busy user blocks it like any recreate.
	h.write(filepath.Join(h.fake, "img/eg_latest"), "sha:e3")
	h.write(filepath.Join(h.fake, "c/rcc-alice/sessions"), "1\n")
	if _, err := h.run("server/update.sh", "", updateParams()...); err == nil || h.composeLog() != "" {
		t.Fatal("recreated the egress host during a session")
	}
}

func egressParams() []string {
	return []string{"URL=https://rcb", "EIMAGE=eg:latest", "COMPOSE_B64=" + b64("egress compose\n"),
		"COMPOSE_USER_B64=" + b64("user compose\n")}
}

func TestEgressRecreatesOnlyOnChange(t *testing.T) {
	h := newFakeHost(t, "alice")
	egressHost(t, h, "sha:e1")
	// Same token, image, compose and .env: left running.
	h.run("server/egress.sh", "rcch_old\n", egressParams()...) // writes the current .env
	out, err := h.run("server/egress.sh", "rcch_old\n", egressParams()...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "recreated=1") || h.composeLog() != "" {
		t.Fatalf("recreated an unchanged egress host: %q / %q", out, h.composeLog())
	}
	// A newer image behind the tag, then a new token: recreated each time.
	h.write(filepath.Join(h.fake, "img/eg_latest"), "sha:e2")
	if out, _ := h.run("server/egress.sh", "rcch_old\n", egressParams()...); !strings.Contains(out, "recreated=1") {
		t.Fatalf("new image not picked up: %q", out)
	}
	h.container("net-egress", map[string]string{"image": "sha:e2"})
	if out, _ := h.run("server/egress.sh", "rcch_new\n", egressParams()...); !strings.Contains(out, "recreated=1") {
		t.Fatalf("new token not picked up: %q", out)
	}
}

func TestEgressAttachesByMountSource(t *testing.T) {
	h := newFakeHost(t, "alice", "bob")
	egressHost(t, h, "sha:e1")
	// bob's container has the mount, but of a dir shared with nobody's socket.
	h.container("rcc-bob", map[string]string{"egress": "/var/lib/remote-cc/egress/none"})
	h.write(filepath.Join(h.dir, "users/bob/.env"), h.read(filepath.Join(h.dir, "users/bob/.env"))+"RCC_EGRESS_DIR=/var/lib/remote-cc/egress/none\n")
	out, err := h.run("server/egress.sh", "rcch_old\n", egressParams()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "attached= bob\n") {
		t.Fatalf("bob not attached: %q", out)
	}
	if env := h.read(filepath.Join(h.dir, "users/bob/.env")); strings.Count(env, "RCC_EGRESS_DIR=") != 1 || !strings.Contains(env, "RCC_EGRESS_DIR="+h.egressDir("bob")+"\n") {
		t.Fatalf("bob .env: %q", env)
	}
	if strings.Contains(h.composeLog(), "users/alice") {
		t.Fatal("alice recreated though her mount was right")
	}
}

func TestAuthCleansUpAndKeepsStatus(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	root := t.TempDir()
	bin, tmp := filepath.Join(root, "bin"), filepath.Join(root, "tmp")
	os.MkdirAll(bin, 0o755)
	os.MkdirAll(tmp, 0o755)
	stub := func(name, body string) string {
		p := filepath.Join(bin, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755)
		return p
	}
	stub("unshare", "shift 3; exec \"$@\"\n")
	shim := stub("shim", "shift 2; exec \"$@\"\n")
	// claude: the resolver dir exists while it runs; a ^C reaching the
	// shell too must not make it skip the cleanup. (Were the shell exec'd
	// away, PPID would be this test.)
	claude := stub("claude", "[ -d \"$(dirname \"$RCC_EGRESS_RESOLV\")\" ] || exit 99\n"+
		"[ \"$PPID\" = \"$TEST_PID\" ] || kill -INT $PPID\nsleep 0.2\nexit 7\n")
	run := func(extra ...string) int {
		cmd := exec.Command("sh", "-c", Client("client/auth.sh"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+tmp, "CLAUDE="+claude, "SHIM="+shim, "ACCT_DIR=",
			"TEST_PID="+strconv.Itoa(os.Getpid()))
		cmd.Env = append(cmd.Env, extra...)
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	if code := run("RCB_NODE=n1"); code != 7 {
		t.Fatalf("exit %d, want claude's 7", code)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "rcc-auth.*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	stub("claude", "exit 3\n")
	if code := run(); code != 3 {
		t.Fatalf("without a node: exit %d, want 3", code)
	}
}
