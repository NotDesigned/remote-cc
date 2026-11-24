#!/bin/sh
# Prepare persistent state, then run sshd in the foreground.
set -e

if [ "${RCC_ROLE:-}" = gateway ]; then
  for t in ed25519 ecdsa rsa; do
    k=/etc/ssh/keys/ssh_host_${t}_key
    [ -f "$k" ] || ssh-keygen -q -t "$t" -N '' -f "$k"
  done
  chmod 600 /etc/ssh/keys/*_key
  # Readable by the gateway accounts (rcc-gateway-cmd hostkey); keys/ is 0700.
  install -m 644 /etc/ssh/keys/ssh_host_ed25519_key.pub /etc/ssh/rcc_gateway_hostkey.pub
  mkdir -p /etc/remote-cc/gateway /etc/remote-cc/keys
  rcc-gateway-sync
  exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_gateway_config -o PidFile=/run/sshd-gateway.pid
fi

# Host keys live in a volume so the fingerprint survives image rebuilds.
for t in ed25519 ecdsa rsa; do
  k=/etc/ssh/keys/ssh_host_${t}_key
  [ -f "$k" ] || ssh-keygen -q -t "$t" -N '' -f "$k"
done
chmod 600 /etc/ssh/keys/*_key

# Authorized keys: from $AUTHORIZED_KEYS (added if missing) and/or a file
# already present in the /root volume.
mkdir -p /root/.ssh && chmod 700 /root/.ssh
touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys
if [ -n "${AUTHORIZED_KEYS:-}" ]; then
  printf '%s\n' "$AUTHORIZED_KEYS" | while IFS= read -r key; do
    [ -n "$key" ] || continue
    grep -qxF "$key" /root/.ssh/authorized_keys || printf '%s\n' "$key" >> /root/.ssh/authorized_keys
  done
fi
if [ ! -s /root/.ssh/authorized_keys ]; then
  echo "remote-cc: no authorized keys; set AUTHORIZED_KEYS or put keys in <data>/root/.ssh/authorized_keys" >&2
fi
[ -e /dev/fuse ] || echo "remote-cc: /dev/fuse missing; start the container with the fuse device" >&2

exec /usr/sbin/sshd -D -e
