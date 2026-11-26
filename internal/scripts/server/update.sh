# shellcheck shell=sh
# Starts stopped containers, pulls the images and recreates the gateway, user
# and egress containers only when the image or compose files changed (or a
# user's egress mount is missing or wrong). Never cuts a session off (unless
# FORCE): the gateway is recreated anyway (sessions reconnect), busy users
# and a busy egress host are marked pending and recreated by the worker
# (WORKER_B64, server/pending.sh) once their sessions end.
: "${DIR:?}" "${IMAGE?}" "${FORCE:?}" "${COMPOSE_GW_B64:?}" "${COMPOSE_USER_B64:?}" "${COMPOSE_EGRESS_B64:?}"
load_server
cd "$DIR" || exit 3
if [ -n "$IMAGE" ]; then sed -i '/^RCC_IMAGE=/d' .env; echo "RCC_IMAGE=$IMAGE" >> .env; fi
image=$(sed -n 's/^RCC_IMAGE=//p' .env)
grep -q '^COMPOSE_PROJECT_NAME=' .env || echo "COMPOSE_PROJECT_NAME=$NET" >> .env
users=$(user_names)
first=$(printf '%s\n' "$users" | head -1)
# egress_src CONTAINER: the host dir mounted at /run/rcc-egress ("" if none).
egress_src() { docker inspect -f '{{range .Mounts}}{{if eq .Destination "/run/rcc-egress"}}{{.Source}}{{end}}{{end}}' "$1" 2>/dev/null || true; }
# Fix every user's .env before deciding anything: older versions and
# migrations left out lines, and an up-to-date server must not skip this.
# Only the files change; running containers are left alone.
for n in $users; do
  [ -d "users/$n" ] || continue
  grep -q '^COMPOSE_PROJECT_NAME=' "users/$n/.env" || echo "COMPOSE_PROJECT_NAME=rcc-$n" >> "users/$n/.env"
  grep -q '^RCC_NETWORK=' "users/$n/.env" || echo "RCC_NETWORK=$NET" >> "users/$n/.env"
  # The egress host serves each user only in their own dir: anything else
  # would be shared or dead.
  if ! grep -qxF "RCC_EGRESS_DIR=$DATA/egress/users/$n" "users/$n/.env"; then
    sed -i '/^RCC_EGRESS_DIR=/d' "users/$n/.env"; echo "RCC_EGRESS_DIR=$DATA/egress/users/$n" >> "users/$n/.env"
  fi
  mkdir -p "$DATA/egress/users/$n"
done
# The egress host (server egress), if installed, keeps its token, URL and .env.
ename=""
if [ -f egress/.env ]; then
  ename=$(sed -n 's/^RCC_EGRESS_NAME=//p' egress/.env); ename=${ename:-$NET-egress}
  eimage=$(sed -n 's/^RCC_EGRESS_IMAGE=//p' egress/.env); eimage=${eimage:-ghcr.io/notdesigned/remote-cc-egress:latest}
fi
# Containers that exist but were stopped (docker stop, or they crashed past
# their restart policy) are started first: that needs no recreate.
started=""; up=""
for c in "$GW" $(for n in $users; do echo "rcc-$n"; done) $ename; do
  [ "$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = false ] || continue
  if docker start "$c" >/dev/null; then
    started="$started $c"
    [ "$c" = "$ename" ] || up="$up $c"   # the egress host runs no sshd
  fi
done
if [ -n "$started" ]; then
  # shellcheck disable=SC2086 # one name per word
  wait_up $up || exit 4
  echo "started=${started# }"
fi
echo "before=$(docker exec "rcc-$first" claude --version 2>/dev/null | head -1)"
docker pull -q "$image" >/dev/null 2>&1 || echo "rcc: warning: could not pull $image; comparing with the local image" >&2
latest=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
gw_new=$(printf '%s' "$COMPOSE_GW_B64" | base64 -d); user_new=$(printf '%s' "$COMPOSE_USER_B64" | base64 -d)
project() { docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$1" 2>/dev/null; }
gw_changed=0
{ [ "$(docker inspect -f '{{.Image}}' "$GW" 2>/dev/null)" = "$latest" ] && [ "$gw_new" = "$(cat compose.yml)" ]; } || gw_changed=1
# A user marked pending by an earlier update still needs it, even when its
# compose file is already the new one.
changed=""
for n in $users; do
  { [ ! -f "users/$n/update-pending" ] && [ "$(docker inspect -f '{{.Image}}' "rcc-$n" 2>/dev/null)" = "$latest" ] &&
    [ "$user_new" = "$(cat "users/$n/compose.yml" 2>/dev/null)" ] && [ "$(project "rcc-$n")" = "rcc-$n" ] &&
    [ "$(egress_src "rcc-$n")" = "$DATA/egress/users/$n" ]; } || changed="$changed $n"
done
egress_changed=0
if [ -n "$ename" ]; then
  docker pull -q "$eimage" >/dev/null 2>&1 || echo "rcc: warning: could not pull $eimage; comparing with the local image" >&2
  elatest=$(docker image inspect -f '{{.Id}}' "$eimage" 2>/dev/null || true)
  egress_new=$(printf '%s' "$COMPOSE_EGRESS_B64" | base64 -d)
  { [ ! -f egress/update-pending ] && [ "$(docker inspect -f '{{.Image}}' "$ename" 2>/dev/null)" = "$elatest" ] &&
    [ "$egress_new" = "$(cat egress/compose.yml 2>/dev/null)" ]; } || egress_changed=1
fi
if [ "$gw_changed" = 0 ] && [ -z "$changed" ] && [ "$egress_changed" = 0 ]; then echo "uptodate=1"; exit 0; fi
ids() { for c in "$GW" $(for n in $users; do echo "rcc-$n"; done) $ename; do docker inspect -f '{{.Id}}' "$c" 2>/dev/null; done; }
ids0=$(ids)
# The gateway is recreated even while sessions run: it keeps no session
# state and its host keys live in <data>/gateway/ssh, so sessions passing
# through it reconnect to the new one by themselves.
if [ "$gw_changed" = 1 ]; then
  printf '%s\n' "$gw_new" > compose.yml && docker compose up -d >&2
  echo "gateway=updated"
fi
# Users: recreated now when no session runs in them (claim_user turns new
# sessions away meanwhile), else marked pending; --force recreates them all.
updated=""; pending=""
for n in $changed; do
  printf '%s\n' "$user_new" > "users/$n/compose.yml"
  if [ "$FORCE" = 1 ]; then
    recreate_user "$n" && updated="$updated $n"
  elif a=$(claim_user "$n"); then
    recreate_user "$n" && updated="$updated $n"
  else
    mark_pending "$n"; pending="$pending $n(${a:-?})"
  fi
done
[ -z "$updated" ] || echo "updated=${updated# }"
[ -z "$pending" ] || echo "pending=${pending# }"
# The egress host: recreating it cuts --rcb sessions off their node.
if [ "$egress_changed" = 1 ]; then
  printf '%s\n' "$egress_new" > egress/compose.yml
  busy=0
  [ "$FORCE" = 1 ] || busy=$(egress_busy)
  if [ "$busy" = 0 ]; then
    # compose alone misses a re-pulled :latest; the image ID said it changed.
    (cd egress && docker compose up -d --pull missing --quiet-pull --force-recreate >&2)
    rm -f egress/update-pending
    echo "egress=updated"
  else
    date +%s > egress/update-pending
    echo "egress=pending($busy)"
  fi
fi
start_worker
# Images still used by a pending container are kept.
docker image prune -f --filter "label=org.opencontainers.image.source=https://github.com/NotDesigned/remote-cc" >/dev/null 2>&1 || true
echo "after=$(docker exec "$GW" claude --version 2>/dev/null | head -1)"
if [ "$ids0" != "$(ids)" ]; then echo "recreated=1"; fi
