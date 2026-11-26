# shellcheck shell=sh
# Recreates one user's container from the image it is set to use: a fresh
# container; the user's data (Claude logins, history), keys and settings are
# kept. Refuses while the user has sessions, unless FORCE=1. Applies a
# pending update (see lib/docker.sh) too.
: "${DIR:?}" "${NAME:?}" "${FORCE:?}"
load_server
valid_name "$NAME" || { echo "rcc: invalid user name $NAME" >&2; exit 2; }
[ -d "$DIR/users/$NAME" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
owned_by "rcc-$NAME" "$DIR/users/$NAME"
if [ "$FORCE" != 1 ] && ! active=$(claim_user "$NAME"); then
  echo "rcc: $NAME has ${active:-some} active session(s); retry when they end, or pass --force" >&2; exit 5
fi
recreate_user "$NAME"
wait_up "rcc-$NAME"
echo "image=$(docker inspect -f '{{.Config.Image}}' "rcc-$NAME" 2>/dev/null)"
