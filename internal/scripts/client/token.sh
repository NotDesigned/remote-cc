# shellcheck shell=sh
# Stores (first stdin line) or clears (ACTION=clear) the account's setup-token.
: "${ACCT_DIR:?}" "${ACTION:?}"
account_prep
umask 077
f="$ACCT_DIR/.rcc-oauth-token"
if [ "$ACTION" = clear ]; then rm -f "$f"; echo "token removed"; exit 0; fi
IFS= read -r tok
[ -n "$tok" ] || { echo "rcc: empty token" >&2; exit 2; }
printf '%s\n' "$tok" > "$f.tmp" && mv -f "$f.tmp" "$f"
echo "token saved"
