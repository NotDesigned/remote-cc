# shellcheck shell=sh
# One "user=name|state|keys|sessions|size" line per user.
: "${DIR:?}"
load_server
while IFS= read -r n; do
  [ -n "$n" ] || continue
  st=$(docker inspect -f '{{.State.Status}}' "rcc-$n" 2>/dev/null || echo missing)
  keys=$(grep -v 'rcc-invite-' "$DATA/keys/$n/authorized_keys" 2>/dev/null | grep -c ' ')
  size=$(du -sh "$DATA/users/$n" 2>/dev/null | cut -f1)
  echo "user=$n|$st|$keys|$(active_sessions "rcc-$n")|$size"
done < "$DATA/gateway/etc/users"
