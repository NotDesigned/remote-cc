# shellcheck shell=sh
# Replaces the user's authorized keys with stdin, only if the file still has
# checksum OLD (so concurrent changes are never overwritten).
: "${OLD:?}"
f=/etc/remote-cc/userkeys/authorized_keys
[ "$(sha256sum < "$f" | cut -c1-64)" = "$OLD" ] || { echo "rcc: the key list changed meanwhile; try again" >&2; exit 4; }
tmp=$(mktemp "$f.XXXXXX")
cat > "$tmp"
chmod 644 "$tmp"
mv -f "$tmp" "$f"
echo "saved"
