package session

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"remote-cc/deploy"
)

// Multi-user mode: one gateway container owns the server's public port; each
// user has a container rcc-<user> on the private "remote-cc" network, reached
// with the gateway as ProxyJump. A user's public keys live once on the host
// (<data>/keys/<user>/authorized_keys): the gateway authenticates the user's
// account with them, and the user's container mounts its own copy read-only.

var userNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// MultiConfig is shared by the server/user admin commands.
type MultiConfig struct {
	Config            // ssh destination/options of the host (root or a sudo account)
	InstallDir string // /opt/remote-cc
	DataDir    string // /var/lib/remote-cc (init only; later read from .env)
	Port       int
	Bind       string
	Image      string
	// PublicHost is the address users connect to; default: what ssh resolves
	// the admin destination to (wrong when administering via localhost).
	PublicHost string
	// Name of this multi-user instance: gateway "<name>-gateway", network
	// "<name>". Default "remote-cc"; only needed for several instances per host.
	Name string
	// InstallDocker installs Docker (get.docker.com) during init if missing.
	InstallDocker bool
}

func (mc *MultiConfig) names() (gw, net string) {
	n := mc.Name
	if n == "" {
		n = "remote-cc"
	}
	return n + "-gateway", n
}

func (mc *MultiConfig) publicHost() string {
	if mc.PublicHost != "" {
		return mc.PublicHost
	}
	return resolveHostname(mc.SSH, mc.SSHArgs, mc.Destination)
}

const multiPrelude = sudoPrelude + `
if ! command -v docker >/dev/null; then
  if [ "${INSTALL_DOCKER:-0}" = 1 ]; then
    echo "rcc: installing Docker with the official script (get.docker.com) …" >&2
    command -v curl >/dev/null || { apt-get update -qq && apt-get install -y -qq curl; } >&2
    curl -fsSL https://get.docker.com | sh >&2
  else
    echo "rcc: docker is not installed on this host (rerun with --install-docker)" >&2; exit 3
  fi
fi
docker compose version >/dev/null 2>&1 || { echo "rcc: the docker compose plugin is missing on this host" >&2; exit 3; }
# owned_by CONTAINER DIR: fail if CONTAINER exists but was created from another
# directory (another deployment, or an unrelated compose project). Never
# replace containers we do not own.
owned_by() {
  wd=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$1" 2>/dev/null || true)
  if [ -n "$wd" ] && [ "$wd" != "$2" ]; then
    echo "rcc: a container named $1 already exists and belongs to $wd; refusing to touch it (choose another name)" >&2
    exit 3
  fi
  if docker inspect "$1" >/dev/null 2>&1 && [ -z "$wd" ]; then
    echo "rcc: a container named $1 already exists (not created by remote-cc); refusing to touch it" >&2
    exit 3
  fi
}
`

const serverInitScript = multiPrelude + `
if [ -f "$DIR/compose.yml" ] && [ ! -f "$DIR/multi-user" ]; then
  echo "rcc: $DIR holds a single-user remote-cc deployment; migrate it with 'server migrate' (or use --install-dir)" >&2
  exit 3
fi
[ -e /dev/fuse ] || modprobe fuse 2>/dev/null || true
[ -e /dev/fuse ] || { echo "rcc: /dev/fuse is missing on this host (users' containers need it)" >&2; exit 3; }
umask 022
mkdir -p "$DIR" "$DATA/gateway/ssh" "$DATA/gateway/etc" "$DATA/keys" "$DATA/users"
chmod 700 "$DATA/gateway/ssh" "$DATA/users"
touch "$DIR/multi-user" "$DATA/gateway/etc/users"
printf '%s' "$COMPOSE_B64" | base64 -d > "$DIR/compose.yml"
case "$BIND" in ""|0.0.0.0|"*") publish="$PORT:22" ;; *) publish="$BIND:$PORT:22" ;; esac
cat > "$DIR/.env" <<ENV
RCC_PORT=$PORT
RCC_PUBLISH=$publish
RCC_DATA=$DATA
RCC_IMAGE=$IMAGE
RCC_GATEWAY=$GW
RCC_NETWORK=$NET
ENV
owned_by "$GW" "$DIR"
echo "COMPOSE_PROJECT_NAME=$NET" >> "$DIR/.env"
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
cd "$DIR" && docker compose up -d --pull missing --quiet-pull >&2
i=0
until docker exec "$GW" test -s /etc/ssh/keys/ssh_host_ed25519_key.pub 2>/dev/null; do
  i=$((i+1)); [ $i -gt 60 ] && { docker compose logs --tail 30 >&2; echo "rcc: gateway did not come up" >&2; exit 4; }
  sleep 1
done
echo "gwkey=$(docker exec "$GW" cat /etc/ssh/keys/ssh_host_ed25519_key.pub | awk '{print $1" "$2}')"
echo "port=$PORT"
`

// multiEnv reads the server settings; used by every user command.
const multiEnv = multiPrelude + `
[ -f "$DIR/multi-user" ] || { echo "rcc: no multi-user remote-cc server in $DIR (run 'server init' first)" >&2; exit 3; }
DATA=$(sed -n 's/^RCC_DATA=//p' "$DIR/.env"); IMAGE=$(sed -n 's/^RCC_IMAGE=//p' "$DIR/.env"); PORT=$(sed -n 's/^RCC_PORT=//p' "$DIR/.env")
GW=$(sed -n 's/^RCC_GATEWAY=//p' "$DIR/.env"); GW=${GW:-remote-cc-gateway}
NET=$(sed -n 's/^RCC_NETWORK=//p' "$DIR/.env"); NET=${NET:-remote-cc}
echo "port=$PORT"
echo "gwkey=$(cat "$DATA/gateway/ssh/ssh_host_ed25519_key.pub" 2>/dev/null | awk '{print $1" "$2}')"
`

const userAddScript = multiEnv + `
u="$DATA/users/$NAME"; k="$DATA/keys/$NAME"
owned_by "rcc-$NAME" "$DIR/users/$NAME"
umask 022
mkdir -p "$u/root/.ssh" "$u/ssh" "$k" "$DIR/users/$NAME"
chmod 700 "$u" "$u/root" "$u/root/.ssh" "$u/ssh"
touch "$k/authorized_keys"; chmod 755 "$k"; chmod 644 "$k/authorized_keys"
printf '%s\n' "$(printf '%s' "$KEYS_B64" | base64 -d)" | while IFS= read -r key; do
  [ -n "$key" ] || continue
  grep -qxF "$key" "$k/authorized_keys" || printf '%s\n' "$key" >> "$k/authorized_keys"
done
printf '%s' "$COMPOSE_B64" | base64 -d > "$DIR/users/$NAME/compose.yml"
cat > "$DIR/users/$NAME/.env" <<ENV
RCC_NAME=rcc-$NAME
RCC_DATA=$u
RCC_KEYS=$k
RCC_IMAGE=$IMAGE
RCC_NETWORK=$NET
COMPOSE_PROJECT_NAME=rcc-$NAME
ENV
# A container of this deployment from rcc <= 0.4.0 runs under the project
# name "$NAME"; it is recreated under the new, collision-free name.
proj=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "rcc-$NAME" 2>/dev/null || true)
[ -n "$proj" ] && [ "$proj" != "rcc-$NAME" ] && docker rm -f "rcc-$NAME" >/dev/null
grep -qx "$NAME" "$DATA/gateway/etc/users" || echo "$NAME" >> "$DATA/gateway/etc/users"
docker exec "$GW" rcc-gateway-sync >&2
cd "$DIR/users/$NAME" && docker compose up -d --pull missing --quiet-pull >&2
i=0
until docker exec "rcc-$NAME" test -s /etc/ssh/keys/ssh_host_ed25519_key.pub 2>/dev/null; do
  i=$((i+1)); [ $i -gt 60 ] && { docker compose logs --tail 30 >&2; echo "rcc: container rcc-$NAME did not come up" >&2; exit 4; }
  sleep 1
done
echo "ukey=$(docker exec "rcc-$NAME" cat /etc/ssh/keys/ssh_host_ed25519_key.pub | awk '{print $1" "$2}')"
echo "keys=$(grep -c . "$k/authorized_keys")"
`

const userInfoScript = multiEnv + `
[ -f "$DIR/users/$NAME/.env" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
echo "ukey=$(cat "$DATA/users/$NAME/ssh/ssh_host_ed25519_key.pub" 2>/dev/null | awk '{print $1" "$2}')"
`

const userListScript = multiEnv + `
while IFS= read -r n; do
  [ -n "$n" ] || continue
  st=$(docker inspect -f '{{.State.Status}}' "rcc-$n" 2>/dev/null || echo missing)
  keys=$(grep -c . "$DATA/keys/$n/authorized_keys" 2>/dev/null); keys=${keys:-0}
  sess=$(docker exec "rcc-$n" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); done; echo $n' 2>/dev/null || echo -)
  size=$(du -sh "$DATA/users/$n" 2>/dev/null | cut -f1)
  echo "user=$n|$st|$keys|$sess|$size"
done < "$DATA/gateway/etc/users"
`

const userRemoveScript = multiEnv + `
grep -qx "$NAME" "$DATA/gateway/etc/users" || [ -d "$DIR/users/$NAME" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
owned_by "rcc-$NAME" "$DIR/users/$NAME"
active=$(docker exec "rcc-$NAME" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); done; echo $n' 2>/dev/null || echo 0)
if [ "${active:-0}" -gt 0 ] && [ "$FORCE" != 1 ]; then
  echo "rcc: $NAME has $active active session(s); retry when they end, or pass --force" >&2; exit 5
fi
# Revoke access first: gateway account and keys.
sed -i "/^$NAME\$/d" "$DATA/gateway/etc/users"
docker exec "$GW" rcc-gateway-sync >&2 || true
rm -rf "$DATA/keys/$NAME" "$DATA/keys/$NAME.invites"
if [ -d "$DIR/users/$NAME" ]; then (cd "$DIR/users/$NAME" && docker compose down >&2); rm -rf "$DIR/users/$NAME"; fi
if [ "$PURGE" = 1 ]; then rm -rf "$DATA/users/$NAME"; echo "rcc: deleted $NAME's data (Claude login, history)" >&2
else echo "rcc: kept $NAME's data in $DATA/users/$NAME (delete with --purge; 'user add' reuses it)" >&2; fi
`

// serverMigrateScript turns a single-user deployment into a multi-user one
// whose only user is the previous owner. Same data, same public port, and the
// server's host key moves to the gateway, so existing known_hosts entries for
// [host]:port stay valid. Rolls back to the single deployment on failure.
const serverMigrateScript = multiPrelude + `
[ -f "$DIR/compose.yml" ] && [ -f "$DIR/.env" ] || { echo "rcc: no remote-cc deployment in $DIR" >&2; exit 3; }
[ -f "$DIR/multi-user" ] && { echo "rcc: $DIR is already a multi-user server" >&2; exit 3; }
envget() { sed -n "s/^$1=//p" "$DIR/.env"; }
old=$(envget RCC_NAME); old=${old:-remote-cc}
DATA=$(envget RCC_DATA); PORT=$(envget RCC_PORT); PORT=${PORT:-2222}
BIND=$(envget RCC_BIND); PUBLISH=$(envget RCC_PUBLISH)
case "$BIND" in ""|0.0.0.0) pub_default="$PORT:22" ;; *) pub_default="$BIND:$PORT:22" ;; esac
PUBLISH=${PUBLISH:-$pub_default}
[ -n "$DATA" ] && [ -d "$DATA/root" ] && [ -d "$DATA/ssh" ] || { echo "rcc: unexpected layout in $DATA" >&2; exit 3; }
[ -e "$DATA/users/$NAME" ] && { echo "rcc: $DATA/users/$NAME already exists" >&2; exit 3; }

active=$(docker exec "$old" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); done; echo $n' 2>/dev/null || echo 0)
if [ "${active:-0}" -gt 0 ] && [ "$FORCE" != 1 ]; then
  echo "rcc: $active active session(s) in $old would be disconnected; retry when they end, or pass --force" >&2; exit 5
fi

owned_by "$GW" "$DIR"
owned_by "rcc-$NAME" "$DIR/users/$NAME"
echo "rcc: fetching $IMAGE before stopping anything …" >&2
docker pull -q "$IMAGE" >&2 2>/dev/null || docker image inspect "$IMAGE" >/dev/null 2>&1 || { echo "rcc: cannot get image $IMAGE" >&2; exit 4; }
docker run --rm --entrypoint test "$IMAGE" -x /usr/local/bin/rcc-gateway-sync || { echo "rcc: $IMAGE has no multi-user support; update the image first" >&2; exit 4; }

ts=$(date +%Y%m%d%H%M%S)
backup="$(dirname "$DATA")/remote-cc-before-multi-user-$ts.tgz"
need=$(du -sk "$DATA/root" "$DATA/ssh" | awk '{s+=$1} END {print s}')
avail=$(df -Pk "$(dirname "$DATA")" | awk 'NR==2 {print $4}')
[ "$avail" -gt $((need * 2 + 102400)) ] || { echo "rcc: not enough disk for a backup ($need KB needed)" >&2; exit 4; }
tar -C "$DATA" -czf "$backup" root ssh
chmod 600 "$backup"
mkdir -p "$DIR/.single-$ts" && cp -a "$DIR/compose.yml" "$DIR/.env" "$DIR/.single-$ts/"
echo "backup=$backup"

u="$DATA/users/$NAME"
rollback() {
  echo "rcc: migration failed, restoring the single-user deployment …" >&2
  (cd "$DIR" && docker compose down >/dev/null 2>&1) || true
  [ -d "$DIR/users/$NAME" ] && (cd "$DIR/users/$NAME" && docker compose down >/dev/null 2>&1) || true
  [ -d "$u/root" ] && mv "$u/root" "$DATA/root"
  [ -d "$u/ssh" ] && mv "$u/ssh" "$DATA/ssh"
  rm -rf "$u" "$DATA/keys/$NAME" "$DATA/gateway" "$DIR/users" "$DIR/multi-user"
  rmdir "$DATA/users" "$DATA/keys" 2>/dev/null || true
  cp -a "$DIR/.single-$ts/compose.yml" "$DIR/.single-$ts/.env" "$DIR/"
  (cd "$DIR" && docker compose up -d >&2)
  exit 6
}

(cd "$DIR" && docker compose down >&2) || { echo "rcc: could not stop $old" >&2; exit 4; }
umask 022
{
  mkdir -p "$u" "$DATA/keys/$NAME" "$DATA/gateway/etc" "$DATA/gateway/ssh" "$DIR/users/$NAME" &&
  chmod 700 "$DATA/users" "$u" "$DATA/gateway/ssh" &&
  mv "$DATA/root" "$u/root" && mv "$DATA/ssh" "$u/ssh" &&
  cp -a "$u/ssh/." "$DATA/gateway/ssh/" &&
  { grep -vE '^[[:space:]]*(#|$)' "$u/root/.ssh/authorized_keys" 2>/dev/null || true; } > "$DATA/keys/$NAME/authorized_keys" &&
  chmod 755 "$DATA/keys" "$DATA/keys/$NAME" && chmod 644 "$DATA/keys/$NAME/authorized_keys" &&
  echo "$NAME" > "$DATA/gateway/etc/users" &&
  printf '%s' "$COMPOSE_GW_B64" | base64 -d > "$DIR/compose.yml" &&
  printf 'RCC_PORT=%s\nRCC_PUBLISH=%s\nRCC_DATA=%s\nRCC_IMAGE=%s\nRCC_GATEWAY=%s\nRCC_NETWORK=%s\n' "$PORT" "$PUBLISH" "$DATA" "$IMAGE" "$GW" "$NET" > "$DIR/.env" && echo "COMPOSE_PROJECT_NAME=$NET" >> "$DIR/.env" &&
  touch "$DIR/multi-user" &&
  printf '%s' "$COMPOSE_USER_B64" | base64 -d > "$DIR/users/$NAME/compose.yml" &&
  printf 'RCC_NAME=rcc-%s\nRCC_DATA=%s\nRCC_KEYS=%s\nRCC_IMAGE=%s\nRCC_NETWORK=%s\n' "$NAME" "$u" "$DATA/keys/$NAME" "$IMAGE" "$NET" > "$DIR/users/$NAME/.env" && echo "COMPOSE_PROJECT_NAME=rcc-$NAME" >> "$DIR/users/$NAME/.env" &&
  { docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null; } &&
  (cd "$DIR" && docker compose up -d >&2) &&
  (cd "$DIR/users/$NAME" && docker compose up -d >&2)
} || rollback
i=0
until docker exec "$GW" test -s /etc/ssh/keys/ssh_host_ed25519_key.pub 2>/dev/null && docker exec "rcc-$NAME" test -s /etc/ssh/keys/ssh_host_ed25519_key.pub 2>/dev/null; do
  i=$((i+1)); [ $i -gt 60 ] && rollback
  sleep 1
done
echo "port=$PORT"
echo "gwkey=$(awk '{print $1" "$2}' "$DATA/gateway/ssh/ssh_host_ed25519_key.pub")"
echo "ukey=$(awk '{print $1" "$2}' "$u/ssh/ssh_host_ed25519_key.pub")"
echo "keys=$(grep -c . "$DATA/keys/$NAME/authorized_keys")"
`

// ServerMigrate converts a single-user deployment into a multi-user server
// whose first user is the previous owner (data, logins, history kept).
func ServerMigrate(mc MultiConfig, name, alias string, force bool) error {
	if err := validUser(name); err != nil {
		return err
	}
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Fprintf(os.Stderr, "rcc: migrating %s to multi-user mode (first user: %s) …\n", mc.Destination, name)
	gw, nw := mc.names()
	out, err := s.runElevated(vars("DIR", mc.InstallDir, "NAME", name, "IMAGE", mc.Image, "FORCE", boolStr(force),
		"GW", gw, "NET", nw,
		"COMPOSE_GW_B64", base64Std(deploy.ComposeGateway), "COMPOSE_USER_B64", base64Std(deploy.ComposeUser)), serverMigrateScript)
	f := parseFacts(out)
	if f["backup"] != "" {
		fmt.Fprintf(os.Stderr, "rcc: backup of the previous data: %s (on the server)\n", f["backup"])
	}
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "rcc: done. %s is now user %s (%s key(s) carried over); the server keeps its host key.\n", mc.Destination, name, f["keys"])
	fmt.Fprintf(os.Stderr, "rcc: the old ssh config for this server (User root, Port %s) must be replaced by the one below.\n", f["port"])
	printUserConfig(mc, name, alias, f)
	return nil
}

// setupMulti is the default `setup-server`: gateway, a user for the admin,
// a dedicated key on this machine authorized for it, and the ssh config —
// i.e. server init + user add + join in one step.
func setupMulti(sc SetupConfig) error {
	user := sc.User
	if user == "" {
		user = "owner"
	}
	if err := validUser(user); err != nil {
		return err
	}
	mc := MultiConfig{Config: sc.Config, InstallDir: sc.InstallDir, DataDir: sc.DataDir, Port: sc.Port, Bind: sc.Bind,
		Image: sc.Image, PublicHost: sc.PublicHost, Name: sc.Name, InstallDocker: sc.InstallDocker}
	if sc.Name == "remote-cc" {
		mc.Name = ""
	}
	host := mc.publicHost()
	alias := sc.Alias
	if alias == "" {
		alias = defaultAlias(host)
	}
	if !aliasRe.MatchString(alias) {
		return fmt.Errorf("invalid alias %q", alias)
	}
	if sc.MigrateHistory || sc.KeysFromHost {
		return errors.New("--migrate-history and --keys-from-host only apply to --single; to convert an existing single-user server use 'server migrate'")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".ssh", "remote-cc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pub, err := ensureKey(filepath.Join(dir, alias+"_ed25519"), alias)
	if err != nil {
		return err
	}
	keys := []string{pub}
	if len(sc.KeyFiles) > 0 {
		extra, err := collectKeys(sc.KeyFiles)
		if err != nil {
			return err
		}
		keys = append(keys, extra...)
	}

	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	gw, nw := mc.names()
	fmt.Fprintf(os.Stderr, "rcc: setting up remote-cc on %s (gateway port %d, user %s) …\n", mc.Destination, mc.Port, user)
	out, err := s.runElevated(vars("DIR", mc.InstallDir, "DATA", mc.DataDir, "PORT", strconv.Itoa(mc.Port),
		"BIND", mc.Bind, "IMAGE", mc.Image, "GW", gw, "NET", nw, "INSTALL_DOCKER", boolStr(mc.InstallDocker),
		"COMPOSE_B64", base64Std(deploy.ComposeGateway)), serverInitScript)
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	f := parseFacts(out)
	out, err = s.runElevated(vars("DIR", mc.InstallDir, "NAME", user, "KEYS_B64", base64Std(strings.Join(keys, "\n")),
		"COMPOSE_B64", base64Std(deploy.ComposeUser)), userAddScript)
	if err != nil {
		return fmt.Errorf("setup failed while creating user %s: %w", user, err)
	}
	for k, v := range parseFacts(out) {
		f[k] = v
	}
	port, _ := strconv.Atoi(f["port"])
	if port == 0 {
		port = mc.Port
	}

	jump := ""
	if mc.Bind == "127.0.0.1" || mc.Bind == "localhost" || mc.Bind == "::1" || !reachable(host, port) {
		jump = mc.Destination
		if mc.Bind != "127.0.0.1" && mc.Bind != "localhost" && mc.Bind != "::1" {
			fmt.Fprintf(os.Stderr, "rcc: port %d on %s is not reachable from here (cloud firewall?); going through %s instead.\n"+
				"     Open TCP %d to connect directly, then rerun setup-server.\n", port, host, jump, port)
		}
		if len(sc.SSHArgs) > 0 {
			fmt.Fprintf(os.Stderr, "rcc: note: the jump uses %q as written; put the ssh options you passed into a Host entry for it.\n", jump)
		}
	}
	if err := writeClientConfig(alias, host, port, user, f["gwkey"], f["ukey"], jump); err != nil {
		return err
	}
	if err := verifyAlias(alias, user); err != nil {
		return fmt.Errorf("server is up, but %w", err)
	}
	fmt.Printf(`
remote-cc is ready on %[1]s: you are user %[2]s, reachable as "%[3]s" (ssh config in ~/.ssh/remote-cc/config).
  %[4]s login %[3]s          # sign in to Claude (once)
  %[4]s %[3]s                # in a project directory
Add people:  %[4]s user add %[5]s <name>   (prints a one-time invite for them)
`, host, user, alias, Prog, mc.Destination)
	return nil
}

func validUser(n string) error {
	if !userNameRe.MatchString(n) {
		return fmt.Errorf("invalid user name %q: lowercase letters, digits and '-', starting with a letter, at most 31 characters", n)
	}
	return nil
}

func (mc *MultiConfig) session() (*session, func(), error) {
	if mc.SSH == "" {
		mc.SSH = "ssh"
	}
	if mc.InstallDir == "" {
		mc.InstallDir = "/opt/remote-cc"
	}
	return newSetupSession(mc.Config)
}

// ServerInit installs the gateway of a multi-user server.
func ServerInit(mc MultiConfig) error {
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	if mc.Port == 0 {
		mc.Port = 2222
	}
	fmt.Fprintf(os.Stderr, "rcc: setting up a multi-user remote-cc gateway on %s (port %d) …\n", mc.Destination, mc.Port)
	gw, nw := mc.names()
	out, err := s.runElevated(vars("DIR", mc.InstallDir, "DATA", mc.DataDir, "PORT", strconv.Itoa(mc.Port),
		"BIND", mc.Bind, "IMAGE", mc.Image, "GW", gw, "NET", nw, "INSTALL_DOCKER", boolStr(mc.InstallDocker),
		"COMPOSE_B64", base64Std(deploy.ComposeGateway)), serverInitScript)
	if err != nil {
		return fmt.Errorf("server init failed: %w", err)
	}
	f := parseFacts(out)
	fmt.Printf("gateway is running on port %d, host key %s\nadd users with:  %s user add %s <name> --key their_key.pub\n",
		mc.Port, fingerprint(f["gwkey"]), Prog, mc.Destination)
	return nil
}

// UserAdd creates (or adds keys to) a user's container.
func UserAdd(mc MultiConfig, name string, keyFiles []string, alias string) error {
	if err := validUser(name); err != nil {
		return err
	}
	var keys []string
	if len(keyFiles) > 0 {
		var err error
		if keys, err = collectKeys(keyFiles); err != nil {
			return err
		}
	}
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := s.runElevated(vars("DIR", mc.InstallDir, "NAME", name, "KEYS_B64", base64Std(strings.Join(keys, "\n")),
		"COMPOSE_B64", base64Std(deploy.ComposeUser)), userAddScript)
	if err != nil {
		return fmt.Errorf("user add failed: %w", err)
	}
	f := parseFacts(out)
	fmt.Fprintf(os.Stderr, "rcc: user %s is ready (container rcc-%s, %s authorized key(s))\n", name, name, f["keys"])
	if len(keys) == 0 {
		// No key given: hand out an invite instead.
		return userInvite(s, mc, name, alias, 7*24*time.Hour)
	}
	printUserConfig(mc, name, alias, f)
	return nil
}

// UserConfig prints the ssh config a user needs.
func UserConfig(mc MultiConfig, name, alias string) error {
	if err := validUser(name); err != nil {
		return err
	}
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := s.runElevated(vars("DIR", mc.InstallDir, "NAME", name), userInfoScript)
	if err != nil {
		return err
	}
	printUserConfig(mc, name, alias, parseFacts(out))
	return nil
}

func printUserConfig(mc MultiConfig, name, alias string, f map[string]string) {
	host := mc.publicHost()
	if alias == "" {
		alias = defaultAlias(host)
	}
	port, _ := strconv.Atoi(f["port"])
	if port == 0 {
		port = 2222
	}
	gwName := host
	if port != 22 {
		gwName = fmt.Sprintf("[%s]:%d", host, port)
	}
	ukAlias := fmt.Sprintf("rcc-%s@%s", name, host)
	fmt.Printf(`
For user %[1]s: add to ~/.ssh/config (Windows: %%USERPROFILE%%\.ssh\config), with
IdentityFile pointing at the private key whose public key was authorized:

  Host %[2]s-gw
      HostName %[3]s
      Port %[4]d
      User %[1]s
      IdentityFile ~/.ssh/id_ed25519
  Host %[2]s
      HostName rcc-%[1]s
      User root
      ProxyJump %[2]s-gw
      HostKeyAlias %[5]s
      IdentityFile ~/.ssh/id_ed25519
      ServerAliveInterval 15

and to ~/.ssh/known_hosts (host keys of the gateway and of the user's container):

  %[6]s %[7]s
  %[5]s %[8]s

then:  %[9]s login %[2]s   and   %[9]s %[2]s
`, name, alias, host, port, ukAlias, gwName, f["gwkey"], f["ukey"], Prog)
}

// UserList prints the users of a multi-user server.
func UserList(mc MultiConfig) error {
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	out, err := s.runElevated(vars("DIR", mc.InstallDir), userListScript)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tCONTAINER\tKEYS\tACTIVE SESSIONS\tDATA")
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "user="); ok {
			fmt.Fprintln(tw, strings.ReplaceAll(v, "|", "\t"))
			n++
		}
	}
	tw.Flush()
	if n == 0 {
		fmt.Printf("no users yet: %s user add %s <name> --key their_key.pub\n", Prog, mc.Destination)
	}
	return nil
}

// UserRemove revokes a user's access and removes their container.
func UserRemove(mc MultiConfig, name string, purge, force bool) error {
	if err := validUser(name); err != nil {
		return err
	}
	s, cleanup, err := mc.session()
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := s.runElevated(vars("DIR", mc.InstallDir, "NAME", name, "PURGE", boolStr(purge), "FORCE", boolStr(force)), userRemoveScript); err != nil {
		return fmt.Errorf("user remove failed: %w", err)
	}
	fmt.Printf("user %s removed (access revoked)\n", name)
	return nil
}

// fingerprint formats an "type base64" public key like ssh-keygen -l.
func fingerprint(key string) string {
	f := strings.Fields(key)
	if len(f) < 2 {
		return "?"
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "?"
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
