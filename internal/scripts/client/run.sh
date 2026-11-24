# shellcheck shell=sh
# Waits for the mount and the command socket, then starts Claude Code in the
# project dir inside the session's mount namespace. Arguments: claude's.
: "${R:?}" "${RUN:?}" "${SOCK:?}" "${CACHE:?}" "${SHIM_SHA:?}" "${CLAUDE:?}"
i=0
while :; do
  if [ -S "$SOCK" ] && [ -f "$RUN/pid" ]; then
    pid=$(cat "$RUN/pid")
    nsenter -t "$pid" -m -- mountpoint -q "$R" 2>/dev/null && break
  fi
  i=$((i+1))
  if [ $i -gt 200 ]; then
    echo "rcc: timed out waiting for the file mount / command socket on the remote machine" >&2
    exit 1
  fi
  sleep 0.1
done
RCC_TOKEN=$(cat "$RUN/token")
export RCC_SOCK="unix:$SOCK" RCC_TOKEN CLAUDE_CODE_SHELL_PREFIX="$CACHE/shims/$SHIM_SHA/rcc-shim"
if [ "${LOCAL_GIT:-1}" = 1 ]; then
  RCC_REAL_GIT=$(command -v git || true)
  export RCC_ROOT="$R" RCC_REAL_GIT PATH="$CACHE/shims/$SHIM_SHA/bin:$PATH"
fi
account_env
# As root, Claude Code refuses --dangerously-skip-permissions unless IS_SANDBOX=1.
[ "${SANDBOX:-0}" = 1 ] && export IS_SANDBOX=1
# shellcheck disable=SC2016 # $1 expands in the inner shell
exec nsenter -t "$pid" -m -- sh -c 'cd "$1" && shift && exec "$@"' sh "$R" "$CLAUDE" "$@"
