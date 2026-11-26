# shellcheck shell=sh
# Admin invite: authorizes the one-time key PUB for NAME, in two halves.
: "${DIR:?}" "${NAME:?}" "${PUB:?}" "${ID:?}" "${TTL:?}"
load_server
valid_name "$NAME" || { echo "rcc: invalid user name $NAME" >&2; exit 2; }
printf '%s' "$ID" | grep -Eq '^[0-9a-f]{16}$' || { echo "rcc: bad invite id" >&2; exit 2; }
printf '%s' "$TTL" | grep -Eq '^[0-9]{1,4}$' || { echo "rcc: bad invite lifetime" >&2; exit 2; }
printf '%s' "$PUB" | grep -Eq '^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$' || { echo "rcc: bad invite key" >&2; exit 2; }
[ -f "$DIR/users/$NAME/.env" ] || { echo "rcc: no user $NAME (create it with 'server user add')" >&2; exit 3; }
# Both halves are written inside the containers that read them (see
# lib/docker.sh). The expiry is in their clock, which their sshd uses.
ensure_up "$GW"; ensure_up "rcc-$NAME"
exp=$(docker exec "rcc-$NAME" date -d "+$TTL hours" +%Y%m%d%H%M)
printf '%s' "$exp" | grep -Eq '^[0-9]{12}$' || { echo "rcc: could not read the time in rcc-$NAME" >&2; exit 4; }
# Gateway half (not visible in the user's container): only hop to rcc-NAME.
printf 'restrict,port-forwarding,permitopen="rcc-%s:22",expiry-time="%s" %s rcc-invite-%s\n' "$NAME" "$exp" "$PUB" "$ID" |
  add_keys "$GW" "/etc/remote-cc/invites/$NAME" >/dev/null
# Container half: may only run rcc-enroll; no forwarding at all.
printf 'restrict,command="/usr/local/bin/rcc-enroll %s",expiry-time="%s" %s rcc-invite-%s\n' "$ID" "$exp" "$PUB" "$ID" |
  add_user_keys "$NAME" >/dev/null
echo "port=$PORT"
echo "exp=$exp"
echo "gwkey=$(hostkey "$GW")"
echo "ukey=$(hostkey "rcc-$NAME")"
