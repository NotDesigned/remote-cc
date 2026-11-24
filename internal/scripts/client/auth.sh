# shellcheck shell=sh
# Runs one claude command (login, logout, status, setup-token) for the account.
: "${CLAUDE:?}"
account_prep
account_env
exec "$CLAUDE" "$@"
