# shellcheck shell=sh
# Installs or updates the egress host: one container that redeems session
# grants with remote-claude-browser (URL) using the host token (first stdin
# line) and runs each granted node in a network namespace of its own. Each
# user gets <data>/egress/users/<user>, where it serves that user's socket.
: "${DIR:?}" "${URL:?}" "${EIMAGE?}" "${COMPOSE_B64:?}" "${COMPOSE_USER_B64:?}"
load_server
IFS= read -r TOKEN || true
case "$TOKEN" in
  rcch_*) ;;
  '') echo "rcc: no host token given" >&2; exit 2 ;;
  *) echo "rcc: that is not a host token (they start with rcch_)" >&2; exit 2 ;;
esac
e="$DIR/egress"
owned_by "$NET-egress" "$e"
umask 077
mkdir -p "$e" "$DATA/egress/state" "$DATA/egress/users"
eimage=${EIMAGE:-ghcr.io/notdesigned/remote-cc-egress:latest}
# The daemon reads its token, URL and image only at start: recreate it when
# any of them, the compose file or the image behind the tag changed.
old=$(cat "$e/token" "$e/compose.yml" "$e/.env" 2>/dev/null || true)
# Limits an admin set in .env (RCC_EGRESS_MAX_NODES…) survive a reinstall.
limits=$(grep '^RCC_EGRESS_MAX_NODES' "$e/.env" 2>/dev/null || true)
printf '%s\n' "$TOKEN" > "$e/token"
printf '%s' "$COMPOSE_B64" | base64 -d > "$e/compose.yml"
cat > "$e/.env" <<ENV
RCC_EGRESS_NAME=$NET-egress
RCC_EGRESS_DATA=$DATA/egress
RCC_EGRESS_RCB_URL=$URL
RCC_EGRESS_IMAGE=$eimage
COMPOSE_PROJECT_NAME=$NET-egress
ENV
[ -z "$limits" ] || printf '%s\n' "$limits" >> "$e/.env"
for n in $(grep -v '^$' "$DATA/gateway/etc/users" 2>/dev/null || true); do
  mkdir -p "$DATA/egress/users/$n"
done
# compose --pull missing never fetches a newer :latest; pull it here.
docker pull -q "$eimage" >/dev/null 2>&1 || echo "rcc: warning: could not pull $eimage; using the local image" >&2
latest=$(docker image inspect -f '{{.Id}}' "$eimage" 2>/dev/null || true)
recreate=1
if [ "$old" = "$(cat "$e/token" "$e/compose.yml" "$e/.env")" ] &&
  [ "$(docker inspect -f '{{.Image}}' "$NET-egress" 2>/dev/null)" = "$latest" ] &&
  [ "$(docker inspect -f '{{.State.Running}}' "$NET-egress" 2>/dev/null)" = true ]; then
  recreate=0
fi
cd "$e" || exit 3
if [ "$recreate" = 1 ]; then
  docker compose up -d --pull missing --quiet-pull --force-recreate >&2
  echo "recreated=1"
  # The daemon exits at once on a bad configuration; give it the time to.
  sleep 2
fi
if [ "$(docker inspect -f '{{.State.Running}}' "$NET-egress" 2>/dev/null)" != true ]; then
  docker logs --tail 20 "$NET-egress" >&2 || true
  echo "rcc: the egress container is not running" >&2; exit 4
fi
# Users whose container lacks this user's own egress mount (it predates the
# egress host, or an older setup mounted another dir) get it now, unless a
# session is running there (recreating would cut it off).
egress_src() { docker inspect -f '{{range .Mounts}}{{if eq .Destination "/run/rcc-egress"}}{{.Source}}{{end}}{{end}}' "$1" 2>/dev/null || true; }
attached=""; busy=""
user_compose=$(printf '%s' "$COMPOSE_USER_B64" | base64 -d)
for n in $(grep -v '^$' "$DATA/gateway/etc/users" 2>/dev/null || true); do
  want="$DATA/egress/users/$n"
  [ "$(egress_src "rcc-$n")" = "$want" ] && continue
  [ -d "$DIR/users/$n" ] || continue
  # .env first, even while busy: the next recreate (update-server) uses it.
  if ! grep -qxF "RCC_EGRESS_DIR=$want" "$DIR/users/$n/.env"; then
    sed -i '/^RCC_EGRESS_DIR=/d' "$DIR/users/$n/.env"; echo "RCC_EGRESS_DIR=$want" >> "$DIR/users/$n/.env"
  fi
  if [ "$(active_sessions "rcc-$n")" -gt 0 ]; then busy="$busy $n"; continue; fi
  printf '%s\n' "$user_compose" > "$DIR/users/$n/compose.yml"
  (cd "$DIR/users/$n" && docker compose up -d >&2) && attached="$attached $n"
done
echo "attached=$attached"
echo "busy=$busy"
