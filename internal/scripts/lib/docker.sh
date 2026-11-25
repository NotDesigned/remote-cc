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
  docker exec "$1" sh -c 'n=0; for d in /root/.cache/rcc/run/*/; do [ -f "$d/pid" ] && kill -0 "$(cat "$d/pid")" 2>/dev/null && n=$((n+1)); done; echo $n' 2>/dev/null || echo 0
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

# pubkey FILE: "type base64" of a public key file.
pubkey() { awk '{print $1" "$2}' "$1" 2>/dev/null; }

# prune_expired FILE: drop authorized_keys lines whose expiry-time has passed.
prune_expired() {
  [ -f "$1" ] || return 0
  now=$(date +%Y%m%d%H%M)
  awk -v now="$now" '{ if (match($0, /expiry-time="[0-9]+"/)) { t=substr($0, RSTART+13, 12); if (t < now) next } print }' "$1" > "$1.tmp" &&
    cat "$1.tmp" > "$1" && rm -f "$1.tmp"
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
