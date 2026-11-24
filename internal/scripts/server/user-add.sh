# shellcheck shell=sh
# Creates a user (or adds keys to one): container rcc-NAME, gateway account.
: "${DIR:?}" "${NAME:?}" "${KEYS_B64?}" "${COMPOSE_B64:?}"
load_server
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
# rcc <= 0.4.0 ran user containers under the project name "NAME": recreate.
proj=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "rcc-$NAME" 2>/dev/null || true)
if [ -n "$proj" ] && [ "$proj" != "rcc-$NAME" ]; then docker rm -f "rcc-$NAME" >/dev/null; fi
grep -qx "$NAME" "$DATA/gateway/etc/users" || echo "$NAME" >> "$DATA/gateway/etc/users"
docker exec "$GW" rcc-gateway-sync >&2
cd "$DIR/users/$NAME" && docker compose up -d --pull missing --quiet-pull >&2
wait_up "rcc-$NAME" || { docker compose logs --tail 30 >&2; exit 4; }
echo "port=$PORT"
echo "gwkey=$(pubkey "$DATA/gateway/ssh/ssh_host_ed25519_key.pub")"
echo "ukey=$(pubkey "$u/ssh/ssh_host_ed25519_key.pub")"
echo "keys=$(grep -c . "$k/authorized_keys")"
