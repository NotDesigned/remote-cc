# shellcheck shell=sh
# Starts a session: sweeps runs left behind, creates RUN, writes the launch
# script (LAUNCH_B64), starts the session daemon (rcc-shim __rcc-daemon) and
# connects this ssh session's stdio to it (rcc-shim __rcc-dial).
: "${CACHE:?}" "${RUN:?}" "${R:?}" "${SHIM_SHA:?}" "${LAUNCH_B64:?}" "${PROF?}" "${CFG:?}"
set -e
umask 077
# Sweep runs whose daemon (or, from older rcc, sshfs) is gone.
# A daemon pid counts only while that pid is still this run's daemon (its
# command line names the run dir): after a reboot the number may belong to
# anything.
for d in "$CACHE"/run/*/; do
  [ -d "$d" ] || continue
  if p=$(cat "$d/daemon.pid" 2>/dev/null) && [ -n "$p" ]; then
    if kill -0 "$p" 2>/dev/null; then
      [ -r "/proc/$p/cmdline" ] || continue
      tr '\0' '\n' < "/proc/$p/cmdline" | grep -qxF "${d%/}" && continue
    fi
    rm -rf "$d"; continue
  fi
  p=$(cat "$d/pid" 2>/dev/null || true)
  if [ -n "$p" ]; then
    kill -0 "$p" 2>/dev/null && continue        # still running (older rcc)
  else
    # No pid yet: maybe another rcc is starting; only sweep old ones.
    [ -n "$(find "$d" -maxdepth 0 -mmin +2 2>/dev/null)" ] || continue
  fi
  rm -rf "$d"
done
# A server update that replaces this container claims it under this lock
# once no session runs (lib/docker.sh, "Deferred updates"): hold it until
# this session's daemon runs, and stay away while the container is being
# replaced (rcc retries once the new one is up). A mark older than 5
# minutes is left over from an update that failed.
if command -v flock >/dev/null && [ -d /run ] && [ -w /run ]; then
  exec 9>/run/rcc-start.lock
  flock -w 30 9 || true
fi
if [ -f /run/rcc-updating ] && [ -z "$(find /run/rcc-updating -mmin +5 2>/dev/null)" ]; then
  echo "rcc-container-updating" >&2; exit 75
fi
mkdir -p "$RUN"
account_prep

shim="$CACHE/shims/$SHIM_SHA/rcc-shim"
[ -x "$shim" ] || { echo "rcc: rcc-shim is missing on the server" >&2; exit 1; }
# bin/git: Claude Code's own git calls on the project run on the computer
# (see shim.Git); everything else falls through to the server's git.
mkdir -p "$CACHE/shims/$SHIM_SHA/bin"
ln -sf ../rcc-shim "$CACHE/shims/$SHIM_SHA/bin/git"
# rg/rg: Claude Code's searches of the project run there too (see shim.Rg).
mkdir -p "$CACHE/shims/$SHIM_SHA/rg"
ln -sf ../rcc-shim "$CACHE/shims/$SHIM_SHA/rg/rg"
touch "$CACHE/shims/$SHIM_SHA"
find "$CACHE/shims" -mindepth 1 -maxdepth 1 -type d -mtime +30 -exec rm -rf {} + 2>/dev/null || true

# The mount point's missing parents, removed again when the session ends.
first="$R"
while [ ! -e "$(dirname "$first")" ]; do first=$(dirname "$first"); done
[ -e "$R" ] || printf '%s\n' "$first" > "$RUN/created"
printf %s "$LAUNCH_B64" | base64 -d > "$RUN/launch.sh"

R="$R" PROF="$PROF" CFG="$CFG" "$shim" __rcc-daemon "$RUN" 9>&-
exec 9>&-   # the daemon runs (and counts as a session): release the lock
exec "$shim" __rcc-dial "$RUN/session.sock"
