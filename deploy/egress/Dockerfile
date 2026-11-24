# rcc-egress: per-node network namespaces whose only way out is a Mihomo TUN.
# Build from the repository root: docker build -f deploy/egress/Dockerfile .
# See deploy/README.md ("Egress through remote-claude-browser nodes").

FROM golang:1.25-bookworm AS build
WORKDIR /src
ARG GOPROXY
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -ldflags "-s -w" -o /rcc-egress ./cmd/rcc-egress

# Mihomo, pinned and checksummed (same release as remote-claude-browser uses).
FROM debian:bookworm-slim AS mihomo
ARG TARGETARCH
ARG MIHOMO_VERSION=v1.19.34
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
 && case "$TARGETARCH" in \
      amd64) f=mihomo-linux-amd64-v1-v1.19.34.gz; s=0c07340a01408d033dda4088ea2a4c1e4d82aeed7b2f84f7c046e93b6ff3290c ;; \
      arm64) f=mihomo-linux-arm64-v1.19.34.gz; s=daaf78a6240a282cdfd75bcbf98fe321f6bd52765cc29e79f344191198f0b50f ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /m.gz "https://github.com/MetaCubeX/mihomo/releases/download/$MIHOMO_VERSION/$f" \
 && echo "$s  /m.gz" | sha256sum -c - \
 && gunzip -c /m.gz > /mihomo && chmod 755 /mihomo

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends iproute2 iptables ca-certificates \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man
COPY --from=build /rcc-egress /usr/local/bin/rcc-egress
COPY --from=mihomo /mihomo /usr/local/bin/mihomo
COPY deploy/egress/mihomo.LICENSE /usr/share/licenses/mihomo/LICENSE
LABEL org.opencontainers.image.source="https://github.com/NotDesigned/remote-cc" \
      org.opencontainers.image.description="remote-cc egress host: Claude Code sessions exit through remote-claude-browser proxy nodes" \
      org.opencontainers.image.licenses="MIT AND GPL-3.0-only"
ENTRYPOINT ["/usr/local/bin/rcc-egress"]
