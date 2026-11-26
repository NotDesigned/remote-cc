# shellcheck shell=sh
# One "user=name|state|keys|sessions|size|update" line per user, then
# "egress=pending" while the egress host waits for an update. Restarts the
# pending-update worker (WORKER_B64, see lib/docker.sh) if updates wait and
# it is not running (after a reboot, say).
: "${DIR:?}"
load_server
for n in $(user_names); do
  st=$(docker inspect -f '{{.State.Status}}' "rcc-$n" 2>/dev/null || echo missing)
  # Counted inside the container (see lib/docker.sh); "-" while it is stopped.
  keys=-
  if running "rcc-$n"; then keys=$(count_keys "rcc-$n"); fi
  size=$(du -sh "$DATA/users/$n" 2>/dev/null | cut -f1)
  upd=-
  if [ -f "$DIR/users/$n/update-pending" ]; then upd=pending; fi
  echo "user=$n|$st|$keys|$(active_sessions "rcc-$n")|$size|$upd"
done
if [ -f "$DIR/egress/update-pending" ]; then echo "egress=pending"; fi
start_worker
