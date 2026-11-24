# shellcheck shell=sh
# Claude accounts on the server: each named account is its own config dir
# (ACCT_DIR, used as CLAUDE_CONFIG_DIR); empty ACCT_DIR = the default config.

# account_prep: create ACCT_DIR and link the user's shared preferences from
# the default config dir (DEFCFG). Existing entries are never replaced, so
# turning a link into a file un-shares that item.
account_prep() {
  [ -n "$ACCT_DIR" ] || return 0
  ( umask 077; mkdir -p "$ACCT_DIR" )
  for f in settings.json CLAUDE.md keybindings.json skills agents commands plugins output-styles; do
    if [ ! -e "$ACCT_DIR/$f" ] && [ ! -L "$ACCT_DIR/$f" ] && [ -e "$DEFCFG/$f" ]; then
      ln -s "$DEFCFG/$f" "$ACCT_DIR/$f"
    fi
  done
}

# rcb_guard: a remote-claude-browser profile's account (rcb-*) is used only
# inside its node's namespace (RCB_NODE), never with this container's network.
rcb_guard() {
  case "${ACCT_DIR##*/}" in
    rcb-*) [ -n "${RCB_NODE:-}" ] || { echo "rcc: account ${ACCT_DIR##*/} belongs to a remote-claude-browser profile; use --rcb" >&2; exit 2; } ;;
  esac
}

# rcb_env: parameters for rcc-shim's netexec stages (internal/netexec).
rcb_env() {
  export RCC_EGRESS_NODE="$RCB_NODE" RCC_EGRESS_RESOLV="$1" RCC_EGRESS_EXPECT_IP="${RCB_EXPECT_IP:-}" \
    RCC_EGRESS_TZ="${RCB_TZ:-}" RCC_EGRESS_EMAIL="${RCB_EMAIL:-}" RCC_EGRESS_LABEL="${RCB_LABEL:-}"
}

# account_env: select the account for claude processes started afterwards.
account_env() {
  [ -n "$ACCT_DIR" ] || return 0
  export CLAUDE_CONFIG_DIR="$ACCT_DIR"
  if [ -s "$ACCT_DIR/.rcc-oauth-token" ]; then
    CLAUDE_CODE_OAUTH_TOKEN=$(cat "$ACCT_DIR/.rcc-oauth-token"); export CLAUDE_CODE_OAUTH_TOKEN
  fi
}
