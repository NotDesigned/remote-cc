# shellcheck shell=sh
# Ends a session's server side if it is still there (the daemon normally
# removes it itself); safe to run more than once.
: "${R:?}" "${RUN:?}"
if [ -f "$RUN/daemon.pid" ]; then
  kill "$(cat "$RUN/daemon.pid")" 2>/dev/null || true   # it ends Claude Code and sshfs
  i=0
  while [ -d "$RUN" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
fi
[ -d "$RUN" ] || exit 0
if [ -f "$RUN/pid" ]; then
  kill "$(cat "$RUN/pid")" 2>/dev/null || true      # sshfs; its namespace goes with it
  sleep 0.2
fi
if [ -f "$RUN/created" ]; then
  first=$(cat "$RUN/created"); d="$R"
  while rmdir "$d" 2>/dev/null; do
    [ "$d" = "$first" ] && break
    d=$(dirname "$d")
  done
fi
rm -rf "$RUN"
