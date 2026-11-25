# shellcheck shell=sh
# Recreates one user's container from the image it is set to use: a fresh
# container; the user's data (Claude logins, history), keys and settings are
# kept. Refuses while the user has sessions, unless FORCE=1.
: "${DIR:?}" "${NAME:?}" "${FORCE:?}"
load_server
[ -d "$DIR/users/$NAME" ] || { echo "rcc: no user $NAME" >&2; exit 3; }
owned_by "rcc-$NAME" "$DIR/users/$NAME"
active=$(active_sessions "rcc-$NAME")
if [ "$active" -gt 0 ] && [ "$FORCE" != 1 ]; then
  echo "rcc: $NAME has $active active session(s); retry when they end, or pass --force" >&2; exit 5
fi
(cd "$DIR/users/$NAME" && docker compose up -d --force-recreate >&2)
wait_up "rcc-$NAME"
echo "image=$(docker inspect -f '{{.Config.Image}}' "rcc-$NAME" 2>/dev/null)"
