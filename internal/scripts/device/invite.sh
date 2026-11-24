# shellcheck shell=sh
# Runs in the user's own container: adds the container half of a self-issued
# invite (the gateway half was registered by rcc-self-invite) and prints the
# container's host key for the invite.
: "${PUB:?}" "${ID:?}" "${EXP:?}"
f=/etc/remote-cc/userkeys/authorized_keys
[ -f "$f" ] || { echo "rcc: this container is not part of a multi-user server" >&2; exit 3; }
case "$ID" in *[!0-9a-f]*) exit 2 ;; esac
case "$EXP" in *[!0-9]*) exit 2 ;; esac
printf 'restrict,command="/usr/local/bin/rcc-enroll %s",expiry-time="%s" %s rcc-invite-%s\n' "$ID" "$EXP" "$PUB" "$ID" >> "$f"
echo "ukey=$(awk '{print $1" "$2}' /etc/ssh/keys/ssh_host_ed25519_key.pub)"
