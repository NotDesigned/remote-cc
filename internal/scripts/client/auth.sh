# shellcheck shell=sh
# Runs one claude command (login, logout, status, setup-token) for the account.
: "${CLAUDE:?}"
rcb_guard
account_prep
account_env
if [ -n "${RCB_NODE:-}" ]; then
  : "${SHIM:?}"
  d=$(mktemp -d "${TMPDIR:-/tmp}/rcc-auth.XXXXXX")
  rcb_env "$d/resolv.conf"
  # A private mount namespace for the session's resolver; login, token
  # refresh and status all go through the profile's node.
  exec unshare -m --propagation private "$SHIM" __rcc-netexec -- "$CLAUDE" "$@"
fi
exec "$CLAUDE" "$@"
