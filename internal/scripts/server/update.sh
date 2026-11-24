# shellcheck shell=sh
# Pulls the image and recreates the gateway / user containers only when the
# image or compose files changed; refuses while sessions run (unless FORCE).
: "${DIR:?}" "${IMAGE?}" "${FORCE:?}" "${COMPOSE_GW_B64:?}" "${COMPOSE_USER_B64:?}"
load_server
cd "$DIR" || exit 3
if [ -n "$IMAGE" ]; then sed -i '/^RCC_IMAGE=/d' .env; echo "RCC_IMAGE=$IMAGE" >> .env; fi
image=$(sed -n 's/^RCC_IMAGE=//p' .env)
grep -q '^COMPOSE_PROJECT_NAME=' .env || echo "COMPOSE_PROJECT_NAME=$NET" >> .env
users=$(grep -v '^$' "$DATA/gateway/etc/users" 2>/dev/null || true)
first=$(printf '%s\n' "$users" | head -1)
echo "before=$(docker exec "rcc-$first" claude --version 2>/dev/null | head -1)"
docker pull -q "$image" >/dev/null 2>&1 || echo "rcc: warning: could not pull $image; comparing with the local image" >&2
latest=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
gw_new=$(printf '%s' "$COMPOSE_GW_B64" | base64 -d); user_new=$(printf '%s' "$COMPOSE_USER_B64" | base64 -d)
project() { docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$1" 2>/dev/null; }
changed=""
{ [ "$(docker inspect -f '{{.Image}}' "$GW" 2>/dev/null)" = "$latest" ] && [ "$gw_new" = "$(cat compose.yml)" ]; } || changed="gateway"
for n in $users; do
  { [ "$(docker inspect -f '{{.Image}}' "rcc-$n" 2>/dev/null)" = "$latest" ] &&
    [ "$user_new" = "$(cat "users/$n/compose.yml" 2>/dev/null)" ] && [ "$(project "rcc-$n")" = "rcc-$n" ]; } || changed="$changed $n"
done
[ -n "$changed" ] || { echo "uptodate=1"; exit 0; }
busy=""
for n in $users; do
  a=$(active_sessions "rcc-$n")
  [ "$a" -gt 0 ] && busy="$busy $n($a)"
done
if [ -n "$busy" ] && [ "$FORCE" != 1 ]; then
  echo "rcc: active sessions would be disconnected:$busy; retry when they end, or pass --force" >&2; exit 5
fi
ids() { for c in "$GW" $(for n in $users; do echo "rcc-$n"; done); do docker inspect -f '{{.Id}}' "$c" 2>/dev/null; done; }
ids0=$(ids)
printf '%s\n' "$gw_new" > compose.yml && docker compose up -d >&2
for n in $users; do
  printf '%s\n' "$user_new" > "users/$n/compose.yml"
  grep -q '^COMPOSE_PROJECT_NAME=' "users/$n/.env" || echo "COMPOSE_PROJECT_NAME=rcc-$n" >> "users/$n/.env"
  grep -q '^RCC_NETWORK=' "users/$n/.env" || echo "RCC_NETWORK=$NET" >> "users/$n/.env"
  grep -q '^RCC_EGRESS_DIR=' "users/$n/.env" || echo "RCC_EGRESS_DIR=$DATA/egress/users/$n" >> "users/$n/.env"
  mkdir -p "$DATA/egress/users/$n"
  p=$(project "rcc-$n")
  if [ -n "$p" ] && [ "$p" != "rcc-$n" ]; then docker rm -f "rcc-$n" >/dev/null; fi   # rcc <= 0.4.0 project name
  (cd "users/$n" && docker compose up -d >&2)
done
docker image prune -f --filter "label=org.opencontainers.image.source=https://github.com/NotDesigned/remote-cc" >/dev/null 2>&1 || true
echo "after=$(docker exec "rcc-$first" claude --version 2>/dev/null | head -1)"
if [ "$ids0" != "$(ids)" ]; then echo "recreated=1"; fi
