# shellcheck shell=sh
# One "user=name|state|keys|sessions|size" line per user.
: "${DIR:?}"
load_server
for n in $(user_names); do
  st=$(docker inspect -f '{{.State.Status}}' "rcc-$n" 2>/dev/null || echo missing)
  # Counted inside the container (see lib/docker.sh); "-" while it is stopped.
  keys=-
  if running "rcc-$n"; then keys=$(count_keys "rcc-$n"); fi
  size=$(du -sh "$DATA/users/$n" 2>/dev/null | cut -f1)
  echo "user=$n|$st|$keys|$(active_sessions "rcc-$n")|$size"
done
