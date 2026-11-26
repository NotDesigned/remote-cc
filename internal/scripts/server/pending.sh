# shellcheck shell=sh
# The pending-update worker (see "Deferred updates" in lib/docker.sh): runs
# in the background on the host, started by server update. Every few
# seconds it recreates each pending user container once no session runs in
# it, and the egress host once no --rcb session runs; it exits when nothing
# is pending.
: "${DIR:?}"
load_server
cd "$DIR" || exit 3
if command -v flock >/dev/null; then
  exec 8>"$DIR/pending-worker.lock"
  flock -n 8 || exit 0   # another worker runs
fi
say() { echo "$(date '+%F %T') $*"; }
EVERY=${EVERY:-5}
while :; do
  failed=0
  for n in $(user_names); do
    [ -f "users/$n/update-pending" ] || continue
    # Cheap first look: a session daemon's process in the container.
    if docker top "rcc-$n" -eo args 2>/dev/null | grep -q -- '__rcc-daemon '; then continue; fi
    if claim_user "$n" >/dev/null; then
      if recreate_user "$n"; then say "rcc-$n: updated"; else say "rcc-$n: recreating failed; will retry"; failed=1; fi
    fi
  done
  if [ -f egress/update-pending ]; then
    if [ "$(egress_busy)" = 0 ]; then
      if (cd egress && docker compose up -d --pull missing --quiet-pull --force-recreate >&2); then
        rm -f egress/update-pending; say "egress host: updated"
      else
        say "egress host: recreating failed; will retry"; failed=1
      fi
    fi
  fi
  # Done once nothing is pending any more (busy ones and failures are).
  any_pending || exit 0
  if [ "$failed" = 1 ]; then sleep 60; else sleep "$EVERY"; fi
done
