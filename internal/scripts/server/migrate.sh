# shellcheck shell=sh
# Converts a single-container deployment (rcc <= 0.4.0) into a multi-user
# server whose only user NAME is the previous owner: same data and port, and
# the server's host key moves to the gateway so known_hosts entries stay
# valid. Rolls back to the single deployment on failure.
: "${DIR:?}" "${NAME:?}" "${IMAGE:?}" "${FORCE:?}" "${GW:?}" "${NET:?}" "${COMPOSE_GW_B64:?}" "${COMPOSE_USER_B64:?}"
if [ ! -f "$DIR/compose.yml" ] || [ ! -f "$DIR/.env" ]; then echo "rcc: no remote-cc deployment in $DIR" >&2; exit 3; fi
[ -f "$DIR/multi-user" ] && { echo "rcc: $DIR is already a multi-user server" >&2; exit 3; }
envget() { sed -n "s/^$1=//p" "$DIR/.env"; }
old=$(envget RCC_NAME); old=${old:-remote-cc}
DATA=$(envget RCC_DATA); PORT=$(envget RCC_PORT); PORT=${PORT:-2222}
BIND=$(envget RCC_BIND); PUBLISH=$(envget RCC_PUBLISH)
case "$BIND" in ""|0.0.0.0) pub_default="$PORT:22" ;; *) pub_default="$BIND:$PORT:22" ;; esac
PUBLISH=${PUBLISH:-$pub_default}
if [ -z "$DATA" ] || [ ! -d "$DATA/root" ] || [ ! -d "$DATA/ssh" ]; then echo "rcc: unexpected layout in $DATA" >&2; exit 3; fi
[ -e "$DATA/users/$NAME" ] && { echo "rcc: $DATA/users/$NAME already exists" >&2; exit 3; }

active=$(active_sessions "$old")
if [ "$active" -gt 0 ] && [ "$FORCE" != 1 ]; then
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
  if [ -d "$DIR/users/$NAME" ]; then (cd "$DIR/users/$NAME" && docker compose down >/dev/null 2>&1) || true; fi
  [ -d "$u/root" ] && mv "$u/root" "$DATA/root"
  [ -d "$u/ssh" ] && mv "$u/ssh" "$DATA/ssh"
  rm -rf "$u" "$DATA/keys/$NAME" "$DATA/gateway" "$DIR/users" "$DIR/multi-user" "$DATA/egress/users/$NAME"
  rmdir "$DATA/users" "$DATA/keys" "$DATA/invites" "$DATA/egress/users" "$DATA/egress" 2>/dev/null || true
  cp -a "$DIR/.single-$ts/compose.yml" "$DIR/.single-$ts/.env" "$DIR/"
  (cd "$DIR" && docker compose up -d >&2)
  exit 6
}

(cd "$DIR" && docker compose down >&2) || { echo "rcc: could not stop $old" >&2; exit 4; }
umask 022
{
  mkdir -p "$u" "$DATA/keys/$NAME" "$DATA/gateway/etc" "$DATA/gateway/ssh" "$DATA/invites" "$DIR/users/$NAME" "$DATA/egress/users/$NAME" &&
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
  printf 'RCC_NAME=rcc-%s\nRCC_DATA=%s\nRCC_KEYS=%s\nRCC_IMAGE=%s\nRCC_NETWORK=%s\nRCC_EGRESS_DIR=%s\n' "$NAME" "$u" "$DATA/keys/$NAME" "$IMAGE" "$NET" "$DATA/egress/users/$NAME" > "$DIR/users/$NAME/.env" && echo "COMPOSE_PROJECT_NAME=rcc-$NAME" >> "$DIR/users/$NAME/.env" &&
  { docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null; } &&
  (cd "$DIR" && docker compose up -d >&2) &&
  (cd "$DIR/users/$NAME" && docker compose up -d >&2)
} || rollback
wait_up "$GW" "rcc-$NAME" || rollback
echo "port=$PORT"
echo "gwkey=$(pubkey "$DATA/gateway/ssh/ssh_host_ed25519_key.pub")"
echo "ukey=$(pubkey "$u/ssh/ssh_host_ed25519_key.pub")"
echo "keys=$(grep -c . "$DATA/keys/$NAME/authorized_keys")"
