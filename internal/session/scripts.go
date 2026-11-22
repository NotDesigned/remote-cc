package session

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// The scripts below run on B under /bin/sh. They are sent base64-encoded
// inside `sh -c 'eval "$(...)"'` so that they survive whatever login shell
// (bash, zsh, fish, …) the remote user has.

// remoteCommand wraps a POSIX sh script so any login shell can run it.
func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func remoteCommand(script string) string {
	b64 := base64Std(script)
	return `sh -c 'eval "$(printf %s ` + b64 + ` | base64 -d)"'`
}

// shQuote quotes s for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func vars(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "%s=%s\n", kv[i], shQuote(kv[i+1]))
	}
	return b.String()
}

// probeScript prints key=value facts about B.
const probeScript = `
echo "os=$(uname -s)"
echo "arch=$(uname -m)"
echo "uid=$(id -u)"
echo "home=$HOME"
echo "cfgdir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
for b in sshfs unshare nsenter mountpoint fusermount3 fusermount base64 sha256sum; do
  p=$(command -v $b 2>/dev/null) && echo "bin_$b=$p"
done
[ -e /dev/fuse ] && echo "fuse=1"
c=$(command -v claude 2>/dev/null)
[ -n "$c" ] || c=$("${SHELL:-sh}" -lc 'command -v claude' </dev/null 2>/dev/null | tail -n1)
[ -n "$c" ] || c=$("${SHELL:-sh}" -ic 'command -v claude' </dev/null 2>/dev/null | tail -n1)
for f in "$HOME/.local/bin/claude" "$HOME/.claude/local/claude" /usr/local/bin/claude /usr/bin/claude; do
  [ -n "$c" ] || { [ -x "$f" ] && c=$f; }
done
echo "claude=$c"
f="$HOME/.cache/rcc/rcc-shim"
[ -x "$f" ] && command -v sha256sum >/dev/null && echo "shim_sha=$(sha256sum < "$f" | cut -c1-64)"
true
`

// prepareScript creates the run dir, stores the token (first stdin line)
// and, if NEED_SHIM=1, installs the shim from the rest of stdin.
const prepareScript = `
set -e
umask 077
# Sweep runs left behind by an rcc that died without cleaning up.
for d in "$CACHE"/run/*/; do
  [ -d "$d" ] || continue
  p=$(cat "$d/pid" 2>/dev/null || true)
  if [ -n "$p" ]; then
    # Has a pid: stale exactly when that process is gone.
    kill -0 "$p" 2>/dev/null && continue
  else
    # No pid yet: possibly another rcc still starting; only sweep old ones.
    [ -n "$(find "$d" -maxdepth 0 -mmin +2 2>/dev/null)" ] || continue
  fi
  if [ -f "$d/mnt" ]; then fusermount3 -uz "$(cat "$d/mnt")" 2>/dev/null || fusermount -uz "$(cat "$d/mnt")" 2>/dev/null || true; fi
  rm -rf "$d"
done
mkdir -p "$RUN"
` + accountPrep + `
IFS= read -r tok
printf '%s\n' "$tok" > "$RUN/token"
if [ "$NEED_SHIM" = 1 ]; then
  cat > "$CACHE/rcc-shim.tmp"
  chmod 755 "$CACHE/rcc-shim.tmp"
  mv -f "$CACHE/rcc-shim.tmp" "$CACHE/rcc-shim"
fi
`

// mountScript runs sshfs in slave mode on this session's stdio. In ns mode
// the mount lives in a private mount namespace (B's global view is
// untouched, and A==B on one host works); in global mode it is a normal mount.
const mountScript = `
set -e
opts="slave,uid=$(id -u),gid=$(id -g),attr_timeout=1,entry_timeout=1,negative_timeout=1"
first="$R"
while [ ! -e "$(dirname "$first")" ]; do first=$(dirname "$first"); done
[ -e "$R" ] || printf '%s\n' "$first" > "$RUN/created"
if [ "$MODE" = ns ]; then
  echo $$ > "$RUN/pid"
  # Inside the namespace, also swap Claude Code's conversation store
  # (projects/) and prompt history for this A machine's profile, so that
  # different A machines with the same project path don't share history.
  exec unshare -m --propagation private sh -c '
    set -e
    mkdir -p "$1"
    if [ -n "$3" ]; then
      umask 077
      mkdir -p "$3/projects" "$4/projects"
      [ -e "$3/history.jsonl" ] || : > "$3/history.jsonl"
      [ -e "$4/history.jsonl" ] || : > "$4/history.jsonl"
      mount --bind "$3/projects" "$4/projects"
      mount --bind "$3/history.jsonl" "$4/history.jsonl"
    fi
    exec sshfs -f -o "$2" ":$1" "$1"' sh "$R" "$opts" "$PROF" "$CFG"
fi
mkdir -p "$R"
if [ -n "$(ls -A "$R" 2>/dev/null)" ]; then
  echo "rcc: $R already exists on the remote machine and is not empty; refusing to mount over it" >&2
  exit 3
fi
echo $$ > "$RUN/pid"
printf '%s\n' "$R" > "$RUN/mnt"
exec sshfs -f -o "$opts" ":$R" "$R"
`

// runScript waits for the mount and the forwarded socket, then starts
// Claude Code inside the project directory (and the namespace, in ns mode).
const runScript = `
i=0
while :; do
  ok=1
  [ -S "$SOCK" ] || ok=0
  if [ $ok = 1 ] && [ -f "$RUN/pid" ]; then
    pid=$(cat "$RUN/pid")
    if [ "$MODE" = ns ]; then nsenter -t "$pid" -m -- mountpoint -q "$R" 2>/dev/null || ok=0
    else mountpoint -q "$R" 2>/dev/null || ok=0; fi
  else ok=0; fi
  [ $ok = 1 ] && break
  i=$((i+1))
  if [ $i -gt 200 ]; then
    echo "rcc: timed out waiting for the file mount / command socket on the remote machine" >&2
    exit 1
  fi
  sleep 0.1
done
export RCC_SOCK="unix:$SOCK" RCC_TOKEN="$(cat "$RUN/token")" CLAUDE_CODE_SHELL_PREFIX="$CACHE/rcc-shim"
` + accountEnv + `
# As root, Claude Code refuses --dangerously-skip-permissions unless IS_SANDBOX=1.
[ "$SANDBOX" = 1 ] && export IS_SANDBOX=1
if [ "$MODE" = ns ]; then
  exec nsenter -t "$pid" -m -- sh -c 'cd "$1" && shift && exec "$@"' sh "$R" "$CLAUDE" "$@"
fi
cd "$R" && exec "$CLAUDE" "$@"
`

// cleanupScript tears everything down; safe to run more than once.
const cleanupScript = `
if [ -f "$RUN/pid" ]; then
  if [ "$MODE" = global ]; then fusermount3 -u "$R" 2>/dev/null || fusermount -u "$R" 2>/dev/null || true; fi
  kill "$(cat "$RUN/pid")" 2>/dev/null || true
  sleep 0.2
fi
if [ -f "$RUN/created" ]; then
  first=$(cat "$RUN/created"); d="$R"
  while rmdir "$d" 2>/dev/null; do
    [ "$d" = "$first" ] && break
    d=$(dirname "$d")
  done
fi
rm -rf "$RUN" "$SOCK"
`

// accountPrep creates an account's config dir and links the user's shared
// preferences from the default config dir into it. Existing entries are
// never touched, so replacing a link with a real file un-shares that item.
const accountPrep = `
if [ -n "$ACCT_DIR" ]; then
  ( umask 077; mkdir -p "$ACCT_DIR" )
  for f in settings.json CLAUDE.md keybindings.json skills agents commands plugins output-styles; do
    if [ ! -e "$ACCT_DIR/$f" ] && [ ! -L "$ACCT_DIR/$f" ] && [ -e "$DEFCFG/$f" ]; then
      ln -s "$DEFCFG/$f" "$ACCT_DIR/$f"
    fi
  done
fi
`

// accountEnv selects the account for a claude process started by the script.
const accountEnv = `
if [ -n "$ACCT_DIR" ]; then
  export CLAUDE_CONFIG_DIR="$ACCT_DIR"
  if [ -s "$ACCT_DIR/.rcc-oauth-token" ]; then
    CLAUDE_CODE_OAUTH_TOKEN=$(cat "$ACCT_DIR/.rcc-oauth-token"); export CLAUDE_CODE_OAUTH_TOKEN
  fi
fi
`

// authScript runs one claude command for the account, with no mount or agent.
const authScript = accountPrep + accountEnv + `
exec "$CLAUDE" "$@"
`

// tokenScript stores (first stdin line) or clears the account's setup-token.
const tokenScript = accountPrep + `
umask 077
f="$ACCT_DIR/.rcc-oauth-token"
if [ "$ACTION" = clear ]; then rm -f "$f"; echo "token removed"; exit 0; fi
IFS= read -r tok
[ -n "$tok" ] || { echo "rcc: empty token" >&2; exit 2; }
printf '%s\n' "$tok" > "$f.tmp" && mv -f "$f.tmp" "$f"
echo "token saved"
`

// accountsScript prints, per account, a marker line and its auth status JSON.
const accountsScript = `
show() {
  echo "@@ $1 $2"
  if [ -n "$2" ]; then
    tok=""; [ -s "$2/.rcc-oauth-token" ] && tok=1
    CLAUDE_CONFIG_DIR="$2" CLAUDE_CODE_OAUTH_TOKEN=$( [ -n "$tok" ] && cat "$2/.rcc-oauth-token" ) "$CLAUDE" auth status --json 2>/dev/null </dev/null
    echo; echo "@@token $tok"
  else
    "$CLAUDE" auth status --json 2>/dev/null </dev/null; echo
  fi
}
show default ""
for d in "$ACCTS"/*/; do
  [ -d "$d" ] || continue
  n=$(basename "$d")
  show "$n" "${d%/}"
done
true
`
