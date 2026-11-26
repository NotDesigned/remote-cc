# shellcheck shell=sh
# Creates a user (or adds keys to one): container rcc-NAME, gateway account.
: "${DIR:?}" "${NAME:?}" "${KEYS_B64?}" "${COMPOSE_B64:?}"
load_server
valid_name "$NAME" || { echo "rcc: invalid user name $NAME" >&2; exit 2; }
u="$DATA/users/$NAME"; k="$DATA/keys/$NAME"
owned_by "rcc-$NAME" "$DIR/users/$NAME"
umask 022
# Only the mount points themselves are made here: what is inside them belongs
# to the user's container (see lib/docker.sh). /root/.ssh is made by its
# entrypoint, the key file by add_keys below.
mkdir -p "$u/root" "$u/ssh" "$k" "$DIR/users/$NAME"
chmod 700 "$u" "$u/root" "$u/ssh"; chmod 755 "$k"
printf '%s' "$COMPOSE_B64" | base64 -d > "$DIR/users/$NAME/compose.yml"
cat > "$DIR/users/$NAME/.env" <<ENV
RCC_NAME=rcc-$NAME
RCC_DATA=$u
RCC_KEYS=$k
RCC_IMAGE=$IMAGE
RCC_NETWORK=$NET
RCC_EGRESS_DIR=$DATA/egress/users/$NAME
COMPOSE_PROJECT_NAME=rcc-$NAME
ENV
mkdir -p "$DATA/egress/users/$NAME"
# rcc <= 0.4.0 ran user containers under the project name "NAME": recreate.
proj=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "rcc-$NAME" 2>/dev/null || true)
if [ -n "$proj" ] && [ "$proj" != "rcc-$NAME" ]; then docker rm -f "rcc-$NAME" >/dev/null; fi
if ! user_names | grep -qx "$NAME"; then
  { user_names; echo "$NAME"; } | replace_file "$DATA/gateway/etc/users"
fi
docker exec "$GW" rcc-gateway-sync >&2
cd "$DIR/users/$NAME" && docker compose up -d --pull missing --quiet-pull >&2
wait_up "rcc-$NAME" "$GW" || { docker compose logs --tail 30 >&2; exit 4; }
keys=$(printf '%s' "$KEYS_B64" | base64 -d | add_user_keys "$NAME")
echo "port=$PORT"
echo "gwkey=$(hostkey "$GW")"
echo "ukey=$(hostkey "rcc-$NAME")"
echo "$keys"
