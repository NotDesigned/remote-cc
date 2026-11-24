# shellcheck shell=sh
# One line per account: "acct=NAME|TOKEN|<claude auth status --json, joined>".
: "${CLAUDE:?}" "${ACCTS:?}"
status() { "$CLAUDE" auth status --json 2>/dev/null </dev/null | tr -d '\n'; }
echo "acct=default||$(status)"
for d in "$ACCTS"/*/; do
  [ -d "$d" ] || continue
  dir=${d%/}; tok=""
  # Profile accounts are queried only through their node (--rcb).
  case "${dir##*/}" in rcb-*) echo "acct=$(basename "$dir")||rcb"; continue ;; esac
  if [ -s "$dir/.rcc-oauth-token" ]; then
    tok=yes
    s=$(CLAUDE_CONFIG_DIR="$dir" CLAUDE_CODE_OAUTH_TOKEN=$(cat "$dir/.rcc-oauth-token") status)
  else
    s=$(CLAUDE_CONFIG_DIR="$dir" status)
  fi
  echo "acct=$(basename "$dir")|$tok|$s"
done
