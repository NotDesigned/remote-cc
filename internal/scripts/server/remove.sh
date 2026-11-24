# shellcheck shell=sh
# Removes the gateway and all user containers (data kept unless PURGE=1).
: "${DIR:?}" "${PURGE:?}"
load_server
cd "$DIR" || exit 3
echo "port=$PORT"
for d in users/*/; do
  if [ -d "$d" ]; then (cd "$d" && docker compose down >&2); fi
done
if [ -d egress ]; then (cd egress && docker compose down >&2); fi
docker compose down --rmi all >&2 || docker compose down >&2
docker network rm "$NET" >/dev/null 2>&1 || true
cd / && rm -rf "$DIR"
echo "rcc: removed the gateway, all user containers and $DIR" >&2
if [ "$PURGE" = 1 ] && [ "$DATA" != / ]; then
  rm -rf "$DATA"; echo "rcc: deleted $DATA (all users' logins and history)" >&2
else
  echo "rcc: kept $DATA (all users' data)" >&2
fi
