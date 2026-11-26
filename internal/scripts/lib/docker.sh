# shellcheck shell=sh
# Docker helpers shared by the server scripts (run as root on the host).

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

# owned_by CONTAINER DIR: fail unless CONTAINER is absent or was created by
# the compose project in DIR. Never touch containers of other deployments or
# unrelated projects.
owned_by() {
  wd=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$1" 2>/dev/null || true)
  if [ -n "$wd" ] && [ "$wd" != "$2" ]; then
    echo "rcc: a container named $1 already exists and belongs to $wd; refusing to touch it (choose another name)" >&2
    exit 3
  fi
  if [ -z "$wd" ] && docker inspect "$1" >/dev/null 2>&1; then
    echo "rcc: a container named $1 already exists (not created by remote-cc); refusing to touch it" >&2
    exit 3
  fi
}

# active_sessions CONTAINER: number of live rcc sessions in it (0 if absent).
active_sessions() {
  # A daemon pid counts only while its command line still names the run dir
  # (pids are reused after a restart); older rcc left only Claude's pid.
  # shellcheck disable=SC2016 # expanded by the container's sh
  docker exec "$1" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do
    if [ -f "$d/daemon.pid" ]; then p=$(cat "$d/daemon.pid"); kill -0 "$p" 2>/dev/null && tr "\0" "\n" < "/proc/$p/cmdline" 2>/dev/null | grep -qxF "${d%/}" && n=$((n+1))
    elif [ -f "$d/pid" ]; then kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); fi
  done; echo $n' 2>/dev/null || echo 0
}

# wait_up CONTAINER...: wait until each one's sshd answers (60s). A host key
# alone is not enough: the entrypoint generates more keys before sshd starts,
# and the gateway's sshd re-executes itself after rcc-gateway-sync.
wait_up() {
  i=0
  for c in "$@"; do
    until [ -n "$(docker exec "$c" ssh-keyscan -T 2 -t ed25519 127.0.0.1 2>/dev/null)" ]; do
      i=$((i+1)); [ $i -gt 60 ] && { echo "rcc: container $c did not come up" >&2; return 1; }
      sleep 1
    done
  done
}

# Some host dirs are mounted writable into containers: a user's key dir,
# /root and /etc/ssh/keys (the user is root in their container), and the
# gateway's dirs. Root on the host never opens, writes or chmods a path
# inside them: a symlink, FIFO or device node planted there would aim it at
# the host's own files. Their files are read and written inside the container
# (docker exec), where a symlink reaches only that container's own files.

# valid_name NAME: a server user name (names become paths below $DATA).
valid_name() { printf '%s' "$1" | grep -Eq '^[a-z][a-z0-9-]{0,30}$'; }

# user_names: the users in the gateway's list, one per line. Lines that are
# not user names are skipped: the gateway can write that file.
user_names() {
  [ -f "$DATA/gateway/etc/users" ] || return 0
  while IFS= read -r n; do
    if valid_name "$n"; then echo "$n"; fi
  done < "$DATA/gateway/etc/users"
}

# replace_file FILE: replace FILE with stdin by renaming a new file over it;
# the rename replaces a symlink at FILE instead of writing through it. For
# the gateway's list of users, in a dir the gateway can write.
replace_file() {
  if [ -d "$1" ] && [ ! -L "$1" ]; then echo "rcc: $1 is a directory" >&2; return 1; fi
  rf_tmp=$(mktemp "$1.rcc.XXXXXX") && cat > "$rf_tmp" && chmod 644 "$rf_tmp" && mv -f "$rf_tmp" "$1"
}

# running CONTAINER: whether it is running.
running() { [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = true ]; }

# ensure_up CONTAINER: start it if it exists but is stopped.
ensure_up() {
  running "$1" && return 0
  docker inspect "$1" >/dev/null 2>&1 || { echo "rcc: container $1 is missing (run server update)" >&2; return 1; }
  echo "rcc: starting $1 …" >&2
  docker start "$1" >/dev/null && wait_up "$1"
}

# hostkey CONTAINER: "ssh-ed25519 base64" of its ssh host key, read inside it.
hostkey() {
  # shellcheck disable=SC2016 # expanded by the container's sh
  docker exec "$1" sh -c 'awk '\''{print $1" "$2; exit}'\'' /etc/ssh/keys/ssh_host_ed25519_key.pub' 2>/dev/null |
    grep -E '^ssh-ed25519 [A-Za-z0-9+/]{20,}={0,2}$' || true
}

# keys_sh: the script add_keys runs inside a container.
keys_sh() {
  cat <<'EOF'
set -eu
f=$1
[ -d "$(dirname "$f")" ] || { echo "rcc: $(dirname "$f") is missing in this container" >&2; exit 3; }
[ -f "$f" ] || : > "$f"
now=$(date +%Y%m%d%H%M)
tmp=$(mktemp "$f.XXXXXX")
awk -v now="$now" '{ if (match($0, /expiry-time="[0-9]+"/)) { t=substr($0, RSTART+13, 12); if (t < now) next } print }' "$f" > "$tmp"
while IFS= read -r l || [ -n "$l" ]; do
  [ -n "$l" ] || continue
  grep -qxF "$l" "$tmp" || printf '%s\n' "$l" >> "$tmp"
done
chmod 644 "$tmp"
mv -f "$tmp" "$f"
echo "keys=$({ grep -v rcc-invite- "$f" || true; } | grep -c ' ' || true)"
EOF
}

# add_keys CONTAINER FILE: inside CONTAINER (as its root), drop the lines of
# the authorized_keys file FILE whose expiry-time has passed, add the lines
# on stdin it lacks, and print "keys=<plain keys left>". Atomic, like
# rcc-enroll and keys-put.sh, which write the same file.
add_keys() { docker exec -i "$1" sh -c "$(keys_sh)" sh "$2"; }

# userkeys: a user's key file, as their container sees it.
userkeys=/etc/remote-cc/userkeys/authorized_keys

# add_user_keys NAME: add_keys on NAME's key file, in rcc-NAME.
add_user_keys() { add_keys "rcc-$1" "$userkeys"; }

# count_keys CONTAINER: the plain keys (not invites) in its user key file.
count_keys() {
  # shellcheck disable=SC2016 # expanded by the container's sh
  docker exec "$1" sh -c '{ grep -v rcc-invite- "$1" || true; } | grep -c " " || true' sh "$userkeys" 2>/dev/null || echo -
}

# load_server: read the multi-user deployment in $DIR into DATA IMAGE PORT GW NET.
# shellcheck disable=SC2034 # the variables are for the calling script
load_server() {
  if [ ! -f "$DIR/multi-user" ]; then
    if [ -f "$DIR/compose.yml" ]; then
      echo "rcc: $DIR holds a single-container deployment, which is no longer supported; convert it with 'server migrate --user NAME'" >&2
    else
      echo "rcc: no remote-cc server in $DIR (run server setup first)" >&2
    fi
    exit 3
  fi
  DATA=$(sed -n 's/^RCC_DATA=//p' "$DIR/.env")
  IMAGE=$(sed -n 's/^RCC_IMAGE=//p' "$DIR/.env")
  PORT=$(sed -n 's/^RCC_PORT=//p' "$DIR/.env"); PORT=${PORT:-2222}
  GW=$(sed -n 's/^RCC_GATEWAY=//p' "$DIR/.env"); GW=${GW:-remote-cc-gateway}
  NET=$(sed -n 's/^RCC_NETWORK=//p' "$DIR/.env"); NET=${NET:-remote-cc}
  [ -n "$DATA" ] || { echo "rcc: $DIR/.env has no RCC_DATA" >&2; exit 3; }
  mkdir -p "$DATA/invites"; chmod 755 "$DATA/invites"
}
