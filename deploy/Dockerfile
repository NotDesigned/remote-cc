# remote-cc server: Claude Code + sshd + sshfs in one container.
# rcc on the local machine connects here over ssh; see deploy/README.md.

# --- stage 1: fetch the Claude Code native build
FROM debian:bookworm-slim AS claude
ARG CLAUDE_VERSION=stable
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
 && HOME=/tmp/c bash -c 'curl -fsSL https://claude.ai/install.sh | bash -s -- "$CLAUDE_VERSION"' \
 && cp "$(readlink -f /tmp/c/.local/bin/claude)" /claude \
 && /claude --version

# --- stage 2: runtime
FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server sshfs fuse3 procps ca-certificates sudo \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man \
 && mkdir -p /run/sshd /etc/ssh/keys \
 && rm -f /etc/ssh/ssh_host_*

# Pinned: updates come from a new image, not from auto-update.
COPY --from=claude /claude /usr/local/bin/claude
RUN mkdir -p /etc/claude-code \
 && printf '{\n  "env": { "DISABLE_AUTOUPDATER": "1" }\n}\n' > /etc/claude-code/managed-settings.json \
 && claude --version

COPY sshd_config /etc/ssh/sshd_config.d/remote-cc.conf
COPY --chmod=755 entrypoint.sh /usr/local/bin/entrypoint.sh
COPY sshd_gateway_config /etc/ssh/sshd_gateway_config
COPY --chmod=755 rcc-gateway-sync /usr/local/bin/rcc-gateway-sync
COPY --chmod=755 rcc-enroll /usr/local/bin/rcc-enroll
COPY --chmod=755 rcc-gateway-cmd /usr/local/bin/rcc-gateway-cmd
COPY --chmod=755 rcc-self-invite /usr/local/bin/rcc-self-invite
# Gateway accounts may run exactly this one program as root (self-service invites).
RUN printf 'ALL ALL=(root) NOPASSWD: /usr/local/bin/rcc-self-invite\n' > /etc/sudoers.d/rcc-self-invite \
 && chmod 440 /etc/sudoers.d/rcc-self-invite && visudo -cf /etc/sudoers.d/rcc-self-invite
# Accounts the gateway creates for users start here (rcc-gateway-sync removes only these).
RUN sed -i 's/^#\?UID_MIN.*/UID_MIN 2000/' /etc/login.defs

LABEL org.opencontainers.image.source="https://github.com/NotDesigned/remote-cc" \
      org.opencontainers.image.description="remote-cc server: Claude Code + sshd + sshfs" \
      org.opencontainers.image.licenses="MIT"
EXPOSE 22
VOLUME ["/root", "/etc/ssh/keys"]
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
