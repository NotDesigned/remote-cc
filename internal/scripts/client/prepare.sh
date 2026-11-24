# shellcheck shell=sh
# Prepares a session: sweeps stale runs, creates RUN, stores the token (first
# stdin line), installs the shim (rest of stdin) if NEED_SHIM=1.
: "${CACHE:?}" "${RUN:?}" "${SHIM_SHA:?}" "${NEED_SHIM:?}"
set -e
umask 077
# Sweep runs left behind by an rcc that died without cleaning up.
for d in "$CACHE"/run/*/; do
  [ -d "$d" ] || continue
  p=$(cat "$d/pid" 2>/dev/null || true)
  if [ -n "$p" ]; then
    kill -0 "$p" 2>/dev/null && continue        # still running
  else
    # No pid yet: maybe another rcc is starting; only sweep old ones.
    [ -n "$(find "$d" -maxdepth 0 -mmin +2 2>/dev/null)" ] || continue
  fi
  rm -rf "$d"
done
mkdir -p "$RUN"
account_prep

IFS= read -r tok
printf '%s\n' "$tok" > "$RUN/token"
# shims/<sha256>/rcc-shim: sessions of different rcc versions never replace
# each other's shim; a private temp name keeps simultaneous uploads apart.
mkdir -p "$CACHE/shims/$SHIM_SHA"
if [ "$NEED_SHIM" = 1 ]; then
  tmp="$CACHE/shims/$SHIM_SHA/.rcc-shim.$$"
  cat > "$tmp"
  chmod 755 "$tmp"
  mv -f "$tmp" "$CACHE/shims/$SHIM_SHA/rcc-shim"
fi
touch "$CACHE/shims/$SHIM_SHA"
find "$CACHE/shims" -mindepth 1 -maxdepth 1 -type d -mtime +30 -exec rm -rf {} + 2>/dev/null || true
