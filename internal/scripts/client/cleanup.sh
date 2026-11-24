# shellcheck shell=sh
# Ends a session's server side; safe to run more than once.
: "${R:?}" "${RUN:?}" "${SOCK:?}"
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
rm -rf "$RUN" "$SOCK"
