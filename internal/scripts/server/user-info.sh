# shellcheck shell=sh
# Host keys and port for printing a user's ssh config.
: "${DIR:?}" "${NAME:?}"
load_server
valid_name "$NAME" || { echo "rcc: invalid user name $NAME" >&2; exit 2; }
[ -f "$DIR/users/$NAME/.env" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
# The host keys are read inside the containers (see lib/docker.sh).
ensure_up "$GW"; ensure_up "rcc-$NAME"
echo "port=$PORT"
echo "gwkey=$(hostkey "$GW")"
echo "ukey=$(hostkey "rcc-$NAME")"
