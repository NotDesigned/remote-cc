# shellcheck shell=sh
# Admin invite: authorizes the one-time key PUB for NAME, in two halves.
: "${DIR:?}" "${NAME:?}" "${PUB:?}" "${ID:?}" "${TTL:?}"
load_server
[ -f "$DIR/users/$NAME/.env" ] || { echo "rcc: no user $NAME (create it with 'server user add')" >&2; exit 3; }
k="$DATA/keys/$NAME/authorized_keys"; inv="$DATA/invites/$NAME"
touch "$inv"; chmod 644 "$inv"
prune_expired "$k"; prune_expired "$inv"
exp=$(date -d "+$TTL hours" +%Y%m%d%H%M)
# Gateway half (not visible in the user's container): only hop to rcc-NAME.
printf 'restrict,port-forwarding,permitopen="rcc-%s:22",expiry-time="%s" %s rcc-invite-%s\n' "$NAME" "$exp" "$PUB" "$ID" >> "$inv"
# Container half: may only run rcc-enroll; no forwarding at all.
printf 'restrict,command="/usr/local/bin/rcc-enroll %s",expiry-time="%s" %s rcc-invite-%s\n' "$ID" "$exp" "$PUB" "$ID" >> "$k"
echo "port=$PORT"
echo "exp=$exp"
echo "gwkey=$(pubkey "$DATA/gateway/ssh/ssh_host_ed25519_key.pub")"
echo "ukey=$(pubkey "$DATA/users/$NAME/ssh/ssh_host_ed25519_key.pub")"
