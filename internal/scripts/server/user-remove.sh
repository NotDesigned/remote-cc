# shellcheck shell=sh
# Revokes a user's access and removes their container (data kept unless PURGE=1).
: "${DIR:?}" "${NAME:?}" "${PURGE:?}" "${FORCE:?}"
load_server
grep -qx "$NAME" "$DATA/gateway/etc/users" || [ -d "$DIR/users/$NAME" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
owned_by "rcc-$NAME" "$DIR/users/$NAME"
active=$(active_sessions "rcc-$NAME")
if [ "$active" -gt 0 ] && [ "$FORCE" != 1 ]; then
  echo "rcc: $NAME has $active active session(s); retry when they end, or pass --force" >&2; exit 5
fi
# Revoke first: gateway account, keys, pending invites.
sed -i "/^$NAME\$/d" "$DATA/gateway/etc/users"
docker exec "$GW" rcc-gateway-sync >&2 || true
rm -rf "$DATA/keys/$NAME" "$DATA/keys/$NAME.invites" "$DATA/invites/$NAME" "$DATA/egress/users/$NAME"
if [ -d "$DIR/users/$NAME" ]; then (cd "$DIR/users/$NAME" && docker compose down >&2); rm -rf "$DIR/users/$NAME"; fi
if [ "$PURGE" = 1 ]; then
  rm -rf "$DATA/users/$NAME"; echo "rcc: deleted $NAME's data (Claude logins, history)" >&2
else
  echo "rcc: kept $NAME's data in $DATA/users/$NAME (delete with --purge; 'user add' reuses it)" >&2
fi
