package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

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
cat > "$DIR/.env" <<ENV
RCC_NAME=$NAME
RCC_PORT=$PORT
RCC_BIND=$BIND
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
	// The script embeds itself (as root it skips the sudo step) so it can
	// re-run under sudo with the same parameters.
	inner := params + setupScript
	run := func(password string) ([]byte, error) {
		script := params + vars("SELF_B64", base64Std(inner), "HAVE_PASS", boolStr(password != "")) + setupScript
		c := s.sshCmd(false, nil, script)
		c.Stderr = os.Stderr
		if password != "" {
			c.Stdin = strings.NewReader(password + "\n")
		}
		return c.Output()
	}
	fmt.Fprintf(os.Stderr, "rcc: installing remote-cc server on %s (port %d, data %s) …\n", sc.Destination, sc.Port, sc.DataDir)
	out, err := run("")
	if bytes.Contains(out, []byte("rcc-needs-sudo-password")) {
		pw, perr := readPassword(fmt.Sprintf("[sudo] password for %s: ", sc.Destination))
		if perr != nil {
			return perr
		}
		out, err = run(pw)
	}
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
	fmt.Printf(`
remote-cc server is running: %s
  host key   %s  (added to your known_hosts, fetched over the verified ssh connection)

Add this to your ssh config (~/.ssh/config, on Windows %%USERPROFILE%%\.ssh\config):

  Host cc
      HostName %s
      Port %d
      User root
      ServerAliveInterval 15

then:
  rcc login cc        # sign in to Claude inside the container (once per account)
  rcc cc              # in a project directory
`, facts["claude"], facts["fingerprint"], host, sc.Port)
	return nil
}

// removeScript stops and deletes a deployment; with PURGE=1 also its data.
// Same sudo handling as setupScript.
const removeScript = `
set -e
if [ "$(id -u)" != 0 ]; then
  command -v sudo >/dev/null || { echo "rcc: not logged in as root and sudo is not installed" >&2; exit 3; }
  if sudo -n true 2>/dev/null; then set -- sudo -n
  elif [ "$HAVE_PASS" = 1 ]; then set -- sudo -S -p ""
  else echo "rcc-needs-sudo-password"; exit 10; fi
  exec "$@" sh -c "$(printf %s "$SELF_B64" | base64 -d)"
fi
[ -f "$DIR/compose.yml" ] || { echo "rcc: no remote-cc deployment in $DIR" >&2; exit 3; }
cd "$DIR"
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
	params := vars("DIR", installDir, "PURGE", boolStr(purge))
	inner := params + removeScript
	run := func(password string) ([]byte, error) {
		c := s.sshCmd(false, nil, params+vars("SELF_B64", base64Std(inner), "HAVE_PASS", boolStr(password != ""))+removeScript)
		c.Stderr = os.Stderr
		if password != "" {
			c.Stdin = strings.NewReader(password + "\n")
		}
		return c.Output()
	}
	out, err := run("")
	if bytes.Contains(out, []byte("rcc-needs-sudo-password")) {
		pw, perr := readPassword(fmt.Sprintf("[sudo] password for %s: ", cfg.Destination))
		if perr != nil {
			return perr
		}
		out, err = run(pw)
	}
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
