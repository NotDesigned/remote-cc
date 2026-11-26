# shellcheck shell=sh
# Host scripts start with this: re-run as root through sudo when needed.
# SELF_B64 holds the whole script (parameters + body); HAVE_PASS=1 means the
# sudo password arrives as the first stdin line. As root this is a no-op.
set -e
if [ "$(id -u)" != 0 ]; then
  command -v sudo >/dev/null || { echo "rcc: not logged in as root and sudo is not installed" >&2; exit 3; }
  if sudo -n true 2>/dev/null; then
    exec sudo -n sh -c "$(printf %s "$SELF_B64" | base64 -d)"
  fi
  [ "${HAVE_PASS:-0}" = 1 ] || { echo "rcc-needs-sudo-password"; exit 10; }
  # The password goes to sudo through SUDO_ASKPASS, never through stdin:
  # sudo -S asks again after a wrong password and would read the next stdin
  # line (a token meant for the script) as a password. The helper holds no
  # secret; it prints the variable it inherits from here.
  IFS= read -r RCC_SUDO_PW || true
  rcc_ap=$(mktemp)
  # shellcheck disable=SC2016 # expanded when sudo runs the helper
  printf '#!/bin/sh\nprintf "%%s\\n" "$RCC_SUDO_PW"\n' > "$rcc_ap"
  chmod 700 "$rcc_ap"
  export RCC_SUDO_PW SUDO_ASKPASS="$rcc_ap"
  if ! sudo -A -p "" true 2>/dev/null; then
    rm -f "$rcc_ap"; echo "rcc-wrong-sudo-password"; exit 11
  fi
  rcc_st=0
  sudo -A -p "" sh -c "$(printf %s "$SELF_B64" | base64 -d)" || rcc_st=$?
  rm -f "$rcc_ap"
  exit "$rcc_st"
fi
