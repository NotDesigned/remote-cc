# shellcheck shell=sh
# Prints key=value facts about the server (the user's container).
echo "os=$(uname -s)"
echo "arch=$(uname -m)"
echo "uid=$(id -u)"
echo "home=$HOME"
echo "hostname=$(hostname)"
[ -f /etc/ssh/sshd_config.d/remote-cc.conf ] && echo "rccserver=1"
echo "cfgdir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
for b in sshfs unshare nsenter mountpoint fusermount3 fusermount base64 sha256sum gzip; do
  p=$(command -v $b 2>/dev/null) && echo "bin_$b=$p"
done
[ -e /dev/fuse ] && echo "fuse=1"
# A server update waits for this container's sessions to end (see
# lib/docker.sh, "Deferred updates").
[ -f /run/rcc-update-pending ] && echo "update_pending=1"
c=$(command -v claude 2>/dev/null)
[ -n "$c" ] || c=$("${SHELL:-sh}" -lc 'command -v claude' </dev/null 2>/dev/null | tail -n1)
[ -n "$c" ] || c=$("${SHELL:-sh}" -ic 'command -v claude' </dev/null 2>/dev/null | tail -n1)
for f in "$HOME/.local/bin/claude" "$HOME/.claude/local/claude" /usr/local/bin/claude /usr/bin/claude; do
  [ -n "$c" ] || { [ -x "$f" ] && c=$f; }
done
echo "claude=$c"
# Installed shims, one directory per content hash (versions coexist).
echo "shims=$(find "$HOME/.cache/rcc/shims" -mindepth 1 -maxdepth 1 -type d -printf '%f,' 2>/dev/null)"
true
