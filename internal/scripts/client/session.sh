# shellcheck shell=sh
# Starts a session: sweeps runs left behind, creates RUN, writes the launch
# script (LAUNCH_B64), starts the session daemon (rcc-shim __rcc-daemon) and
# connects this ssh session's stdio to it (rcc-shim __rcc-dial).
: "${CACHE:?}" "${RUN:?}" "${R:?}" "${SHIM_SHA:?}" "${LAUNCH_B64:?}" "${PROF?}" "${CFG:?}"
set -e
umask 077
# Sweep runs whose daemon (or, from older rcc, sshfs) is gone.
for d in "$CACHE"/run/*/; do
  [ -d "$d" ] || continue
  p=$(cat "$d/daemon.pid" 2>/dev/null || cat "$d/pid" 2>/dev/null || true)
  if [ -n "$p" ]; then
    kill -0 "$p" 2>/dev/null && continue        # still running
  else
    # No pid yet: maybe another rcc is starting; only sweep old ones.
    [ -n "$(find "$d" -maxdepth 0 -mmin +2 2>/dev/null)" ] || continue
  fi
  rm -rf "$d"
done
mkdir -p "$RUN"
account_prep

shim="$CACHE/shims/$SHIM_SHA/rcc-shim"
[ -x "$shim" ] || { echo "rcc: rcc-shim is missing on the server" >&2; exit 1; }
# bin/git: Claude Code's own git calls on the project run on the computer
# (see shim.Git); everything else falls through to the server's git.
mkdir -p "$CACHE/shims/$SHIM_SHA/bin"
ln -sf ../rcc-shim "$CACHE/shims/$SHIM_SHA/bin/git"
touch "$CACHE/shims/$SHIM_SHA"
find "$CACHE/shims" -mindepth 1 -maxdepth 1 -type d -mtime +30 -exec rm -rf {} + 2>/dev/null || true

# The mount point's missing parents, removed again when the session ends.
first="$R"
while [ ! -e "$(dirname "$first")" ]; do first=$(dirname "$first"); done
[ -e "$R" ] || printf '%s\n' "$first" > "$RUN/created"
printf %s "$LAUNCH_B64" | base64 -d > "$RUN/launch.sh"

R="$R" PROF="$PROF" CFG="$CFG" "$shim" __rcc-daemon "$RUN"
exec "$shim" __rcc-dial "$RUN/session.sock"
