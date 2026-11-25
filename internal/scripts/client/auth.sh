# shellcheck shell=sh
# Runs one claude command (login, logout, status, setup-token) for the account.
: "${CLAUDE:?}"
rcb_guard
account_prep
account_env
# rcbLogin drives the terminal through a pipe; give the pty its real size.
if [ "${TTY_COLS:-0}" -gt 0 ] 2>/dev/null; then stty cols "$TTY_COLS" rows "$TTY_ROWS" 2>/dev/null || true; fi
if [ -n "${RCB_NODE:-}" ]; then
  : "${SHIM:?}"
  d=$(mktemp -d "${TMPDIR:-/tmp}/rcc-auth.XXXXXX")
  # No exec: this shell stays to remove $d. It outlives claude on the
  # terminal's ^C / hangup with no-op traps, which (unlike ignoring them)
  # reset to the default in claude, so claude still gets them.
  trap 'rm -rf "$d"' EXIT
  trap : INT QUIT HUP TERM
  rcb_env "$d/resolv.conf"
  # A private mount namespace for the session's resolver; login, token
  # refresh and status all go through the profile's node.
  rc=0
  unshare -m --propagation private "$SHIM" __rcc-netexec -- "$CLAUDE" "$@" || rc=$?
  exit "$rc"
fi
exec "$CLAUDE" "$@"
