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
func remoteCommand(script string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
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
  if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then continue; fi
  [ -n "$(find "$d" -maxdepth 0 -mmin +1 2>/dev/null)" ] || continue
  if [ -f "$d/mnt" ]; then fusermount3 -uz "$(cat "$d/mnt")" 2>/dev/null || fusermount -uz "$(cat "$d/mnt")" 2>/dev/null || true; fi
  rm -rf "$d"
done
mkdir -p "$RUN"
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
