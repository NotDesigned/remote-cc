package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"remote-cc/deploy"
)

// SetupConfig describes `rcc setup-server`.
type SetupConfig struct {
	Config                 // ssh destination/options of the host (root or a docker-capable user)
	Port          int      // public port of the container's sshd
	Bind          string   // address the port binds on the host
	DataDir       string   // persistent data on the host
	InstallDir    string   // where compose.yml/.env go on the host
	Image         string   // server image
	KeyFiles      []string // public keys (on A) to authorize; default: agent / ~/.ssh/id_*.pub
	KeysFromHost  bool     // also authorize the host user's own authorized_keys
	MigrateHistory bool    // copy rcc history profiles from a previous non-docker install
	Name           string  // container name
	InstallDocker  bool    // install Docker with get.docker.com if missing
}

// setupScript runs on the host. As a non-root user it re-runs itself as root
// via sudo (passwordless, or with the password on stdin), after reading the
// login user's own files that --keys-from-host / --migrate-history need.
const setupScript = `
set -e
if [ "$(id -u)" != 0 ]; then
  command -v sudo >/dev/null || { echo "rcc: not logged in as root and sudo is not installed" >&2; exit 3; }
  host_keys=""
  if [ "$KEYS_FROM_HOST" = 1 ] && [ -f "$HOME/.ssh/authorized_keys" ]; then
    host_keys=$(grep -vE '^[[:space:]]*(#|$)' "$HOME/.ssh/authorized_keys" | base64 | tr -d '\n')
  fi
  if sudo -n true 2>/dev/null; then
    set -- sudo -n
  elif [ "$HAVE_PASS" = 1 ]; then
    set -- sudo -S -p ""
  else
    echo "rcc-needs-sudo-password"
    exit 10
  fi
  exec "$@" env ORIG_HOME="$HOME" HOST_KEYS_B64="$host_keys" sh -c "$(printf %s "$SELF_B64" | base64 -d)"
fi
ORIG_HOME=${ORIG_HOME:-$HOME}

if ! command -v docker >/dev/null; then
  if [ "$INSTALL_DOCKER" = 1 ]; then
    echo "rcc: installing Docker with the official script (get.docker.com) …" >&2
    command -v curl >/dev/null || { apt-get update -qq && apt-get install -y -qq curl; } >&2
    curl -fsSL https://get.docker.com | sh >&2
  else
    echo "rcc: docker is not installed on this host; rerun with --install-docker, or see https://docs.docker.com/engine/install/" >&2
    exit 3
  fi
fi
docker compose version >/dev/null 2>&1 || { echo "rcc: the docker compose plugin is missing on this host" >&2; exit 3; }
[ -e /dev/fuse ] || modprobe fuse 2>/dev/null || true
[ -e /dev/fuse ] || { echo "rcc: /dev/fuse is missing on this host (the fuse kernel module is required)" >&2; exit 3; }

[ -f "$DIR/multi-user" ] && { echo "rcc: $DIR is a multi-user server; manage it with 'user add' / 'update-server'" >&2; exit 3; }
umask 077
mkdir -p "$DIR" "$DATA/root/.ssh" "$DATA/ssh"
printf '%s' "$COMPOSE_B64" | base64 -d > "$DIR/compose.yml"
keys=$(printf '%s' "$KEYS_B64" | base64 -d)
if [ "$KEYS_FROM_HOST" = 1 ]; then
  if [ -n "${HOST_KEYS_B64:-}" ]; then
    keys=$(printf '%s\n%s\n' "$keys" "$(printf '%s' "$HOST_KEYS_B64" | base64 -d)")
  elif [ -f "$ORIG_HOME/.ssh/authorized_keys" ]; then
    keys=$(printf '%s\n%s\n' "$keys" "$(grep -vE '^[[:space:]]*(#|$)' "$ORIG_HOME/.ssh/authorized_keys")")
  fi
fi
# Keys go straight into the volume (not into .env), one per line, deduplicated.
printf '%s\n' "$keys" | while IFS= read -r k; do
  [ -n "$k" ] || continue
  grep -qxF "$k" "$DATA/root/.ssh/authorized_keys" 2>/dev/null || printf '%s\n' "$k" >> "$DATA/root/.ssh/authorized_keys"
done
[ -s "$DATA/root/.ssh/authorized_keys" ] || { echo "rcc: no public key to authorize" >&2; exit 3; }
case "$BIND" in ""|0.0.0.0|"*") BIND=""; publish="$PORT:22" ;; *) publish="$BIND:$PORT:22" ;; esac
cat > "$DIR/.env" <<ENV
RCC_NAME=$NAME
RCC_PORT=$PORT
RCC_BIND=$BIND
RCC_PUBLISH=$publish
RCC_DATA=$DATA
RCC_IMAGE=$IMAGE
ENV

if [ "$MIGRATE" = 1 ] && [ -d "$ORIG_HOME/.local/share/rcc/profiles" ]; then
  mkdir -p "$DATA/root/.local/share/rcc/profiles"
  for p in "$ORIG_HOME/.local/share/rcc/profiles"/*/; do
    [ -d "$p" ] || continue
    n=$(basename "$p")
    if [ -e "$DATA/root/.local/share/rcc/profiles/$n" ]; then echo "rcc: history profile $n already present, kept" >&2
    else cp -a "$p" "$DATA/root/.local/share/rcc/profiles/$n" && echo "rcc: copied history profile $n" >&2; fi
  done
fi

cd "$DIR"
docker compose up -d --pull missing --quiet-pull >&2
i=0
until docker exec "$NAME" test -s /etc/ssh/keys/ssh_host_ed25519_key.pub 2>/dev/null; do
  i=$((i+1)); [ $i -gt 60 ] && { docker compose logs --tail 30 >&2; echo "rcc: container did not come up" >&2; exit 4; }
  sleep 1
done
echo "hostkey=$(docker exec "$NAME" cat /etc/ssh/keys/ssh_host_ed25519_key.pub | awk '{print $1" "$2}')"
echo "fingerprint=$(docker exec "$NAME" ssh-keygen -lf /etc/ssh/keys/ssh_host_ed25519_key.pub | awk '{print $2}')"
echo "claude=$(docker exec "$NAME" claude --version 2>/dev/null | head -1)"
`

// Setup installs or updates the server container on a host.
func Setup(sc SetupConfig) error {
	if sc.SSH == "" {
		sc.SSH = "ssh"
	}
	keys, err := collectKeys(sc.KeyFiles)
	if err != nil && !sc.KeysFromHost {
		return err
	}
	s, cleanup, err := newSetupSession(sc.Config)
	if err != nil {
		return err
	}
	defer cleanup()

	if sc.Name == "" {
		sc.Name = "remote-cc"
	}
	if !validProfile(sc.Name) {
		return fmt.Errorf("invalid container name %q", sc.Name)
	}
	params := vars(
		"DIR", sc.InstallDir, "DATA", sc.DataDir, "PORT", strconv.Itoa(sc.Port), "BIND", sc.Bind, "IMAGE", sc.Image,
		"NAME", sc.Name, "COMPOSE_B64", base64Std(deploy.Compose), "KEYS_B64", base64Std(strings.Join(keys, "\n")),
		"KEYS_FROM_HOST", boolStr(sc.KeysFromHost), "MIGRATE", boolStr(sc.MigrateHistory),
		"INSTALL_DOCKER", boolStr(sc.InstallDocker),
	)
	fmt.Fprintf(os.Stderr, "rcc: installing remote-cc server on %s (port %d, data %s) …\n", sc.Destination, sc.Port, sc.DataDir)
	out, err := s.runElevated(params, setupScript)
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	facts := map[string]string{}
	sc2 := bufio.NewScanner(bytes.NewReader(out))
	for sc2.Scan() {
		if k, v, ok := strings.Cut(sc2.Text(), "="); ok && k != "" {
			facts[k] = v
		}
	}
	host := resolveHostname(sc.SSH, sc.SSHArgs, sc.Destination)
	if facts["hostkey"] != "" && host != "" {
		if err := addKnownHost(host, sc.Port, facts["hostkey"]); err != nil {
			fmt.Fprintf(os.Stderr, "rcc: warning: could not update known_hosts: %v\n", err)
		}
	}
	fmt.Printf("\nremote-cc server is running: %s\n  host key   %s  (added to your known_hosts, fetched over the verified ssh connection)\n\n",
		facts["claude"], facts["fingerprint"])

	direct := sc.Bind != "127.0.0.1" && sc.Bind != "localhost" && sc.Bind != "::1" && reachable(host, sc.Port)
	switch {
	case direct:
		fmt.Printf("Port %d is reachable from here. Add this to your ssh config (%s):\n\n"+
			"  Host cc\n      HostName %s\n      Port %d\n      User root\n      ServerAliveInterval 15\n",
			sc.Port, sshConfigPath(), host, sc.Port)
	default:
		if sc.Bind != "127.0.0.1" && sc.Bind != "localhost" && sc.Bind != "::1" {
			fmt.Printf("Port %d on %s is NOT reachable from here (a cloud firewall / security group usually blocks it).\n"+
				"Either open TCP %d there, or reach the container through the host's own ssh, as below.\n\n", sc.Port, host, sc.Port)
		}
		// HostKeyAlias keeps using the pinned "[host]:port" known_hosts entry.
		fmt.Printf("Add this to your ssh config (%s):\n\n"+
			"  Host cc\n      HostName 127.0.0.1\n      Port %d\n      User root\n      ProxyJump %s\n      HostKeyAlias [%s]:%d\n      ServerAliveInterval 15\n",
			sshConfigPath(), sc.Port, sc.Destination, host, sc.Port)
		if len(sc.SSHArgs) > 0 {
			fmt.Printf("  (ProxyJump uses %q as written: put the ssh options you passed here into a Host entry for it.)\n", sc.Destination)
		}
	}
	fmt.Printf("\nCheck it with:  ssh cc hostname     (should print the container name, %s)\nthen:\n"+
		"  %s login cc        # sign in to Claude inside the container (once per machine)\n"+
		"  %s cc              # in a project directory\n", sc.Name, Prog, Prog)
	return nil
}

// removeScript stops and deletes a deployment; with PURGE=1 also its data.
// Same sudo handling as setupScript.
const removeScript = sudoPrelude + `
[ -f "$DIR/compose.yml" ] || { echo "rcc: no remote-cc deployment in $DIR" >&2; exit 3; }
cd "$DIR"
if [ -f multi-user ]; then
  data=$(sed -n 's/^RCC_DATA=//p' .env); port=$(sed -n 's/^RCC_PORT=//p' .env)
  echo "port=${port:-2222}"
  for d in users/*/; do [ -d "$d" ] && (cd "$d" && docker compose down --remove-orphans >&2); done
  docker compose down --rmi all --remove-orphans >&2 || docker compose down --remove-orphans >&2
  docker network rm remote-cc >/dev/null 2>&1 || true
  cd / && rm -rf "$DIR"
  echo "rcc: removed the gateway, all user containers and $DIR" >&2
  if [ "$PURGE" = 1 ] && [ -n "$data" ] && [ "$data" != / ]; then rm -rf "$data"; echo "rcc: deleted $data (all users' logins and history)" >&2
  else echo "rcc: kept $data (all users' data)" >&2; fi
  exit 0
fi
data=$(sed -n 's/^RCC_DATA=//p' .env 2>/dev/null); port=$(sed -n 's/^RCC_PORT=//p' .env 2>/dev/null)
echo "port=${port:-2222}"
docker compose down --rmi all --remove-orphans >&2 || docker compose down --remove-orphans >&2
cd / && rm -rf "$DIR"
echo "rcc: removed the container and $DIR (the image too, unless another container still uses it)" >&2
if [ "$PURGE" = 1 ] && [ -n "$data" ] && [ "$data" != / ]; then
  rm -rf "$data" && echo "rcc: deleted the data directory $data (Claude logins, conversation history, host keys)" >&2
elif [ -n "$data" ]; then
  echo "rcc: kept the data directory $data (logins, history, host keys); delete it with --purge, or reuse it with setup-server --data $data" >&2
fi
`

// updateScript pulls the image and recreates the container only when the
// image or compose file changed; refuses (unless FORCE) while sessions run.
const updateScript = sudoPrelude + `
[ -f "$DIR/compose.yml" ] && [ -f "$DIR/.env" ] || { echo "rcc: no remote-cc deployment in $DIR (use setup-server)" >&2; exit 3; }
cd "$DIR"
if [ -f multi-user ]; then
  # Multi-user server: gateway + one container per user, all on one image.
  if [ -n "$IMAGE" ]; then sed -i '/^RCC_IMAGE=/d' .env; echo "RCC_IMAGE=$IMAGE" >> .env; fi
  image=$(sed -n 's/^RCC_IMAGE=//p' .env); data=$(sed -n 's/^RCC_DATA=//p' .env)
  users=$(grep -v '^$' "$data/gateway/etc/users" 2>/dev/null || true)
  first=$(printf '%s\n' "$users" | head -1)
  echo "before=$(docker exec "rcc-$first" claude --version 2>/dev/null | head -1)"
  docker pull -q "$image" >&2 2>/dev/null || echo "rcc: warning: could not pull $image; comparing with the local image" >&2
  latest=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
  gw_new=$(printf '%s' "$COMPOSE_GW_B64" | base64 -d); user_new=$(printf '%s' "$COMPOSE_USER_B64" | base64 -d)
  changed=""
  [ "$(docker inspect -f '{{.Image}}' remote-cc-gateway 2>/dev/null)" = "$latest" ] && [ "$gw_new" = "$(cat compose.yml)" ] || changed="gateway"
  for n in $users; do
    [ "$(docker inspect -f '{{.Image}}' "rcc-$n" 2>/dev/null)" = "$latest" ] && [ "$user_new" = "$(cat "users/$n/compose.yml" 2>/dev/null)" ] || changed="$changed $n"
  done
  [ -n "$changed" ] || { echo "uptodate=1"; exit 0; }
  busy=""
  for n in $users; do
    a=$(docker exec "rcc-$n" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); done; echo $n' 2>/dev/null || echo 0)
    [ "${a:-0}" -gt 0 ] && busy="$busy $n($a)"
  done
  if [ -n "$busy" ] && [ "$FORCE" != 1 ]; then
    echo "rcc: active sessions would be disconnected:$busy; retry when they end, or pass --force" >&2; exit 5
  fi
  ids() { for c in remote-cc-gateway $(for n in $users; do echo "rcc-$n"; done); do docker inspect -f '{{.Id}}' "$c" 2>/dev/null; done; }
  ids0=$(ids)
  printf '%s\n' "$gw_new" > compose.yml && docker compose up -d >&2
  for n in $users; do printf '%s\n' "$user_new" > "users/$n/compose.yml"; (cd "users/$n" && docker compose up -d >&2); done
  docker image prune -f --filter "label=org.opencontainers.image.source=https://github.com/NotDesigned/remote-cc" >/dev/null 2>&1 || true
  echo "after=$(docker exec "rcc-$first" claude --version 2>/dev/null | head -1)"
  [ "$ids0" != "$(ids)" ] && echo "recreated=1"
  exit 0
fi
name=$(sed -n 's/^RCC_NAME=//p' .env); name=${name:-remote-cc}
grep -q '^RCC_NAME=' .env || echo "RCC_NAME=$name" >> .env
if [ -n "$IMAGE" ]; then sed -i '/^RCC_IMAGE=/d' .env; echo "RCC_IMAGE=$IMAGE" >> .env; fi
# .env from rcc <= 0.3.1: derive RCC_PUBLISH; "0.0.0.0" meant all interfaces
# but kept Docker from listening on IPv6.
if ! grep -q '^RCC_PUBLISH=' .env; then
  port=$(sed -n 's/^RCC_PORT=//p' .env); bind=$(sed -n 's/^RCC_BIND=//p' .env)
  case "$bind" in ""|0.0.0.0) bind=""; publish="${port:-2222}:22" ;; *) publish="$bind:${port:-2222}:22" ;; esac
  sed -i '/^RCC_BIND=/d' .env; printf 'RCC_BIND=%s\nRCC_PUBLISH=%s\n' "$bind" "$publish" >> .env
  echo "rcc: port now published on all interfaces (IPv4 and IPv6): $publish" >&2
  migrated=1
fi
image=$(sed -n 's/^RCC_IMAGE=//p' .env)
echo "before=$(docker exec "$name" claude --version 2>/dev/null | head -1)"
docker compose pull -q >&2 || echo "rcc: warning: could not pull $image; comparing with the local image" >&2
new_compose=$(printf '%s' "$COMPOSE_B64" | base64 -d)
cur=$(docker inspect -f '{{.Image}}' "$name" 2>/dev/null || true)
latest=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
if [ -n "$cur" ] && [ "$cur" = "$latest" ] && [ "$new_compose" = "$(cat compose.yml)" ] && [ -z "${migrated:-}" ]; then
  echo "uptodate=1"
  exit 0
fi
active=$(docker exec "$name" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); done; echo $n' 2>/dev/null || echo 0)
if [ "${active:-0}" -gt 0 ] && [ "$FORCE" != 1 ]; then
  echo "rcc: $active active session(s) in $name would be disconnected by the update; retry when they end, or pass --force" >&2
  exit 5
fi
cid0=$(docker inspect -f '{{.Id}}' "$name" 2>/dev/null || true)
printf '%s\n' "$new_compose" > compose.yml
docker compose up -d >&2
i=0
until docker exec "$name" test -s /etc/ssh/keys/ssh_host_ed25519_key.pub 2>/dev/null; do
  i=$((i+1)); [ $i -gt 60 ] && { docker compose logs --tail 30 >&2; echo "rcc: container did not come up" >&2; exit 4; }
  sleep 1
done
# Drop superseded remote-cc images (dangling, ours only) so updates don't fill the disk.
docker image prune -f --filter "label=org.opencontainers.image.source=https://github.com/NotDesigned/remote-cc" >/dev/null 2>&1 || true
echo "after=$(docker exec "$name" claude --version 2>/dev/null | head -1)"
[ "$cid0" != "$(docker inspect -f '{{.Id}}' "$name")" ] && echo "recreated=1" || true
`

// UpdateServer pulls the latest server image and recreates the container if
// anything changed. Data, keys and settings are kept.
func UpdateServer(cfg Config, installDir, image string, force bool) error {
	if cfg.SSH == "" {
		cfg.SSH = "ssh"
	}
	s, cleanup, err := newSetupSession(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Fprintf(os.Stderr, "rcc: updating remote-cc server on %s …\n", cfg.Destination)
	out, err := s.runElevated(vars("DIR", installDir, "IMAGE", image, "FORCE", boolStr(force),
		"COMPOSE_B64", base64Std(deploy.Compose), "COMPOSE_GW_B64", base64Std(deploy.ComposeGateway),
		"COMPOSE_USER_B64", base64Std(deploy.ComposeUser)), updateScript)
	if err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	f := parseFacts(out)
	if f["uptodate"] == "1" {
		fmt.Printf("remote-cc server is up to date (%s)\n", f["before"])
		return nil
	}
	if f["recreated"] != "1" {
		fmt.Printf("remote-cc server is up to date (%s); its compose file was refreshed\n", f["after"])
		return nil
	}
	fmt.Printf("remote-cc server updated (container recreated): %s -> %s\n", dash(f["before"]), dash(f["after"]))
	return nil
}

// Remove stops and deletes a deployment created by Setup.
func Remove(cfg Config, installDir string, purge bool) error {
	if cfg.SSH == "" {
		cfg.SSH = "ssh"
	}
	s, cleanup, err := newSetupSession(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := s.runElevated(vars("DIR", installDir, "PURGE", boolStr(purge)), removeScript)
	if err != nil {
		return fmt.Errorf("remove failed: %w", err)
	}
	port := 2222
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "port="); ok {
			if p, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
				port = p
			}
		}
	}
	host := resolveHostname(cfg.SSH, cfg.SSHArgs, cfg.Destination)
	name := host
	if port != 22 {
		name = fmt.Sprintf("[%s]:%d", host, port)
	}
	if exec.Command("ssh-keygen", "-R", name).Run() == nil {
		fmt.Fprintf(os.Stderr, "rcc: removed %s from your known_hosts\n", name)
	}
	return nil
}

// sudoPrelude makes a host script re-run itself as root via sudo; see
// runElevated. Scripts with extra pre-elevation work (setupScript) carry
// their own variant.
const sudoPrelude = `
set -e
if [ "$(id -u)" != 0 ]; then
  command -v sudo >/dev/null || { echo "rcc: not logged in as root and sudo is not installed" >&2; exit 3; }
  if sudo -n true 2>/dev/null; then set -- sudo -n
  elif [ "$HAVE_PASS" = 1 ]; then set -- sudo -S -p ""
  else echo "rcc-needs-sudo-password"; exit 10; fi
  exec "$@" sh -c "$(printf %s "$SELF_B64" | base64 -d)"
fi
`

// runElevated runs params+body on the host. The body embeds itself (as
// SELF_B64) so that, when not root, it can re-run under sudo; if sudo asks
// for a password it is read here (hidden) and passed on stdin.
func (s *session) runElevated(params, body string) ([]byte, error) {
	inner := params + body
	run := func(password string) ([]byte, error) {
		c := s.sshCmd(false, nil, params+vars("SELF_B64", base64Std(inner), "HAVE_PASS", boolStr(password != ""))+body)
		c.Stderr = os.Stderr
		if password != "" {
			c.Stdin = strings.NewReader(password + "\n")
		}
		return c.Output()
	}
	out, err := run("")
	if bytes.Contains(out, []byte("rcc-needs-sudo-password")) {
		pw, perr := readPassword(fmt.Sprintf("[sudo] password for %s: ", s.cfg.Destination))
		if perr != nil {
			return nil, perr
		}
		out, err = run(pw)
	}
	return out, err
}

func parseFacts(out []byte) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && k != "" && !strings.ContainsAny(k, " \t") {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

func reachable(host string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 6*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func sshConfigPath() string {
	if isWindows() {
		return `%USERPROFILE%\.ssh\config`
	}
	return "~/.ssh/config"
}

func newSetupSession(cfg Config) (*session, func(), error) {
	s := &session{cfg: cfg, id: randHex(6)}
	s.logf = func(string, ...any) {}
	var err error
	if s.tmp, err = os.MkdirTemp("", "rcc-"); err != nil {
		return nil, nil, err
	}
	if !isWindows() {
		s.ctl = filepath.Join(s.tmp, "c")
	}
	return s, func() { s.closeMaster(); os.RemoveAll(s.tmp) }, nil
}

// collectKeys returns public keys to authorize: explicit files, else the
// ssh-agent's keys, else ~/.ssh/id_*.pub.
func collectKeys(files []string) ([]string, error) {
	var keys []string
	add := func(b []byte) {
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "ssh-") || strings.HasPrefix(l, "ecdsa-") || strings.HasPrefix(l, "sk-") {
				keys = append(keys, l)
			}
		}
	}
	if len(files) > 0 {
		for _, f := range files {
			b, err := os.ReadFile(expandHome(f))
			if err != nil {
				return nil, err
			}
			add(b)
		}
	} else {
		if out, err := exec.Command("ssh-add", "-L").Output(); err == nil {
			add(out)
		}
		if len(keys) == 0 {
			home, _ := os.UserHomeDir()
			for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_ed25519_sk", "id_ecdsa_sk"} {
				if b, err := os.ReadFile(filepath.Join(home, ".ssh", n+".pub")); err == nil {
					add(b)
				}
			}
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no public key found (ssh-agent, ~/.ssh/id_*.pub); pass --key FILE or --keys-from-host")
	}
	return keys, nil
}

// resolveHostname asks ssh which host name a destination really uses.
func resolveHostname(sshBin string, sshArgs []string, dest string) string {
	args := append(append([]string{}, sshArgs...), "-G", dest)
	out, err := exec.Command(sshBin, args...).Output()
	if err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(l, "hostname "); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	if i := strings.LastIndexByte(dest, '@'); i >= 0 {
		return dest[i+1:]
	}
	return dest
}

func addKnownHost(host string, port int, key string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	name := host
	if port != 22 {
		name = fmt.Sprintf("[%s]:%d", host, port)
	}
	line := name + " " + key
	f := filepath.Join(home, ".ssh", "known_hosts")
	if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte(line)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	h, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer h.Close()
	_, err = fmt.Fprintln(h, line)
	return err
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func boolStr(b bool) string { return map[bool]string{true: "1", false: "0"}[b] }
