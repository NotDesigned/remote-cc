# shellcheck shell=sh
# Host scripts start with this: re-run as root through sudo when needed.
# SELF_B64 holds the whole script (parameters + body); HAVE_PASS=1 means the
# sudo password arrives on stdin. As root this is a no-op.
set -e
if [ "$(id -u)" != 0 ]; then
  command -v sudo >/dev/null || { echo "rcc: not logged in as root and sudo is not installed" >&2; exit 3; }
  if sudo -n true 2>/dev/null; then set -- sudo -n
  elif [ "${HAVE_PASS:-0}" = 1 ]; then set -- sudo -S -p ""
  else echo "rcc-needs-sudo-password"; exit 10; fi
  exec "$@" sh -c "$(printf %s "$SELF_B64" | base64 -d)"
fi
