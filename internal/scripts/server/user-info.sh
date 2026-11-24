# shellcheck shell=sh
# Host keys and port for printing a user's ssh config.
: "${DIR:?}" "${NAME:?}"
load_server
[ -f "$DIR/users/$NAME/.env" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
echo "port=$PORT"
echo "gwkey=$(pubkey "$DATA/gateway/ssh/ssh_host_ed25519_key.pub")"
echo "ukey=$(pubkey "$DATA/users/$NAME/ssh/ssh_host_ed25519_key.pub")"
