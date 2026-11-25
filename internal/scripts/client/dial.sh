# shellcheck shell=sh
# Reconnects to a running session: connects this ssh session's stdio to the
# session daemon. The message below is what rcc recognises as "ended".
: "${RUN:?}" "${CACHE:?}" "${SHIM_SHA:?}"
s="$CACHE/shims/$SHIM_SHA/rcc-shim"
if [ ! -x "$s" ] || [ ! -S "$RUN/session.sock" ]; then
  echo "error the session on the server has ended"
  exit 3
fi
exec "$s" __rcc-dial "$RUN/session.sock"
