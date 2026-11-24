# shellcheck shell=sh
# Installs or updates the egress host: one container syncing proxy nodes from
# remote-claude-browser (URL) with the host token (first stdin line). Each
# user gets <data>/egress/users/<user>, where it serves that user's socket.
: "${DIR:?}" "${URL:?}" "${EIMAGE?}" "${COMPOSE_B64:?}"
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
printf '%s\n' "$TOKEN" > "$e/token"
printf '%s' "$COMPOSE_B64" | base64 -d > "$e/compose.yml"
cat > "$e/.env" <<ENV
RCC_EGRESS_NAME=$NET-egress
RCC_EGRESS_DATA=$DATA/egress
RCC_EGRESS_RCB_URL=$URL
RCC_EGRESS_IMAGE=${EIMAGE:-ghcr.io/notdesigned/remote-cc-egress:latest}
COMPOSE_PROJECT_NAME=$NET-egress
ENV
for n in $(grep -v '^$' "$DATA/gateway/etc/users" 2>/dev/null || true); do
  mkdir -p "$DATA/egress/users/$n"
done
cd "$e" && docker compose up -d --pull missing --quiet-pull --force-recreate >&2
# Report the first sync (the daemon logs either line within a few seconds).
i=0; state=""
while [ $i -lt 30 ]; do
  state=$(docker logs "$NET-egress" 2>&1 | grep -E 'synced [0-9]+ nodes|sync: ' | tail -1 || true)
  [ -n "$state" ] && break
  i=$((i+1)); sleep 1
done
echo "sync=$state"
# Users whose container predates the egress mount need it recreated.
missing=""
for n in $(grep -v '^$' "$DATA/gateway/etc/users" 2>/dev/null || true); do
  docker inspect -f '{{range .Mounts}}{{.Destination}} {{end}}' "rcc-$n" 2>/dev/null | grep -q /run/rcc-egress || missing="$missing $n"
done
echo "missing=$missing"
