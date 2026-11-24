# shellcheck shell=sh
# Installs rcc-shim (stdin) under shims/<sha256>/ if NEED_SHIM=1, as prepare
# does for sessions; account commands with --rcb need it too.
: "${CACHE:?}" "${SHIM_SHA:?}" "${NEED_SHIM:?}"
set -e
umask 077
mkdir -p "$CACHE/shims/$SHIM_SHA"
if [ "$NEED_SHIM" = 1 ]; then
  tmp="$CACHE/shims/$SHIM_SHA/.rcc-shim.$$"
  cat > "$tmp"
  chmod 755 "$tmp"
  mv -f "$tmp" "$CACHE/shims/$SHIM_SHA/rcc-shim"
fi
touch "$CACHE/shims/$SHIM_SHA"
