# shellcheck shell=sh
# Installs rcc-shim from stdin (gzip-compressed if GZ=1) as
# shims/<sha256>/rcc-shim: sessions of different rcc versions never replace
# each other's shim; a private temp name keeps simultaneous uploads apart.
: "${CACHE:?}" "${SHIM_SHA:?}" "${GZ:?}"
set -e
umask 077
d="$CACHE/shims/$SHIM_SHA"
mkdir -p "$d"
tmp="$d/.rcc-shim.$$"
if [ "$GZ" = 1 ]; then gzip -dc > "$tmp"; else cat > "$tmp"; fi
if [ "$(sha256sum "$tmp" | cut -d' ' -f1)" != "$SHIM_SHA" ]; then
  rm -f "$tmp"
  echo "rcc: the uploaded rcc-shim is damaged; try again" >&2
  exit 1
fi
chmod 755 "$tmp"
mv -f "$tmp" "$d/rcc-shim"
