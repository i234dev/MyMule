# MyMule container image: the mymule daemon on Alpine, run as a normal
# user (PUID/PGID), with its data in /config and downloads in /downloads.
#
#   docker build -t mymule .
#
# The binary is cross-compiled on the build machine (no emulation), so a
# multi-platform build (linux/amd64, linux/arm64, linux/arm/v7) is quick.

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mymule ./cmd/mymule

FROM alpine:3.22
# su-exec: drop from root to PUID/PGID; tzdata: TZ for log times;
# ca-certificates: HTTPS (TMDB, Wikidata, server and Kad node lists).
RUN apk add --no-cache su-exec tzdata ca-certificates
# mymule is a wrapper: as root (docker exec), it runs the program as
# PUID:PGID, so /config never gets files owned by root.
COPY --from=build /out/mymule /usr/local/bin/mymule.bin
COPY --chmod=755 docker/mymule.sh /usr/local/bin/mymule
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh

# Data (mymule.conf with the api_key, identity, server list, log) lives
# in /config; finished files in /downloads, partial ones in
# /downloads/incomplete, so finishing a file is a rename, not a copy.
# The web interface is on, and the API listens on every interface of the
# container: publish port 4711 on 127.0.0.1 or your LAN address only (as
# docker-compose.yml does), never on every address of a computer that is
# on the Internet: Docker's published ports bypass ufw and firewalld. Any setting can be changed with MYMULE_<SETTING> variables.
ENV MYMULE_HOME=/config \
    MYMULE_DOWNLOAD_DIR=/downloads \
    MYMULE_TEMP_DIR=/downloads/incomplete \
    MYMULE_API_LISTEN=0.0.0.0:4711 \
    MYMULE_WEB_UI=true \
    PUID=1000 \
    PGID=1000

VOLUME ["/config", "/downloads"]
# 4662/tcp: peers (forward it in your router for HighID)
# 4672/udp: Kad (forward it too)
# 4711/tcp: web interface and API (local network only)
EXPOSE 4662/tcp 4672/udp 4711/tcp

HEALTHCHECK --interval=1m --timeout=10s --start-period=1m \
    CMD mymule status >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["mymule", "daemon"]

LABEL org.opencontainers.image.title="MyMule" \
      org.opencontainers.image.description="eMule (eD2K and Kad) client with a web interface and Radarr/Sonarr support" \
      org.opencontainers.image.licenses="GPL-3.0-or-later"
