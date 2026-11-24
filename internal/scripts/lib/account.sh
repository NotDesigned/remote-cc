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

# account_env: select the account for claude processes started afterwards.
account_env() {
  [ -n "$ACCT_DIR" ] || return 0
  export CLAUDE_CONFIG_DIR="$ACCT_DIR"
  if [ -s "$ACCT_DIR/.rcc-oauth-token" ]; then
    CLAUDE_CODE_OAUTH_TOKEN=$(cat "$ACCT_DIR/.rcc-oauth-token"); export CLAUDE_CODE_OAUTH_TOKEN
  fi
}
