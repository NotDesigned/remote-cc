# shellcheck shell=sh
# Run by the session daemon in Claude Code's pseudo-terminal once the project
# directory is mounted: sets up the environment, then starts Claude Code in
# the project directory inside the session's mount namespace. Arguments:
# claude's.
: "${R:?}" "${RUN:?}" "${CACHE:?}" "${SHIM_SHA:?}" "${CLAUDE:?}"
pid=$(cat "$RUN/pid")
RCC_TOKEN=$(cat "$RUN/token")
export RCC_SOCK="unix:$RUN/s" RCC_TOKEN CLAUDE_CODE_SHELL_PREFIX="$CACHE/shims/$SHIM_SHA/rcc-shim"
# Grep and Glob take rg from PATH: rg/rg searches the project on the computer.
RCC_REAL_RG=$(command -v rg || true)
export RCC_ROOT="$R" RCC_REAL_RG USE_BUILTIN_RIPGREP=0 PATH="$CACHE/shims/$SHIM_SHA/rg:$PATH"
if [ "${LOCAL_GIT:-1}" = 1 ]; then
  RCC_REAL_GIT=$(command -v git || true)
  export RCC_ROOT="$R" RCC_REAL_GIT PATH="$CACHE/shims/$SHIM_SHA/bin:$PATH"
fi
rcb_guard
account_env
# As root, Claude Code refuses --dangerously-skip-permissions unless IS_SANDBOX=1.
[ "${SANDBOX:-0}" = 1 ] && export IS_SANDBOX=1
if [ -n "${RCB_NODE:-}" ]; then
  # Claude runs inside the profile's node namespace: its only way out is the node.
  rcb_env "$RUN/resolv.conf"
  # shellcheck disable=SC2016 # $1 expands in the inner shell
  exec nsenter -t "$pid" -m -- sh -c 'cd "$1" && shift && exec "$@"' sh "$R" \
    "$CACHE/shims/$SHIM_SHA/rcc-shim" __rcc-netexec -- "$CLAUDE" "$@"
fi
# shellcheck disable=SC2016 # $1 expands in the inner shell
exec nsenter -t "$pid" -m -- sh -c 'cd "$1" && shift && exec "$@"' sh "$R" "$CLAUDE" "$@"
