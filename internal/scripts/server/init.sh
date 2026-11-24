# shellcheck shell=sh
# Installs or refreshes the gateway of a multi-user server.
: "${DIR:?}" "${DATA:?}" "${PORT:?}" "${BIND?}" "${IMAGE:?}" "${GW:?}" "${NET:?}" "${COMPOSE_B64:?}"
if [ -f "$DIR/compose.yml" ] && [ ! -f "$DIR/multi-user" ]; then
  echo "rcc: $DIR holds a single-container deployment; convert it with 'server migrate --user NAME'" >&2
  exit 3
fi
[ -e /dev/fuse ] || modprobe fuse 2>/dev/null || true
[ -e /dev/fuse ] || { echo "rcc: /dev/fuse is missing on this host (users' containers need it)" >&2; exit 3; }
owned_by "$GW" "$DIR"
umask 022
mkdir -p "$DIR" "$DATA/gateway/ssh" "$DATA/gateway/etc" "$DATA/keys" "$DATA/users" "$DATA/invites"
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
COMPOSE_PROJECT_NAME=$NET
ENV
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
cd "$DIR" && docker compose up -d --pull missing --quiet-pull >&2
wait_up "$GW" || { docker compose logs --tail 30 >&2; exit 4; }
echo "gwkey=$(pubkey "$DATA/gateway/ssh/ssh_host_ed25519_key.pub")"
echo "port=$PORT"
