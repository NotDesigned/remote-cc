# shellcheck shell=sh
# Prints the user's authorized keys and a checksum for keys-put.sh.
f=/etc/remote-cc/userkeys/authorized_keys
[ -f "$f" ] || { echo "rcc: this container is not part of a multi-user server" >&2; exit 3; }
echo "@@sha=$(sha256sum < "$f" | cut -c1-64)"
cat "$f"
