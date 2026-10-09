# syntax=docker/dockerfile:1

# ── Stage 1: build the simplerip Go binary ──────────────────────────────────
FROM golang:1.25-alpine AS gobuilder

WORKDIR /src

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# A plain `docker compose build` passes no BUILD_DATE; stamp the build time.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "${BUILD_DATE}" = unknown ]; then BUILD_DATE=$(date -u +%FT%TZ); fi \
    && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
        -o /simplerip \
        ./cmd/simplerip


# ── Stage 2: final runtime image ─────────────────────────────────────────────
FROM ubuntu:24.04 AS final

ENV DEBIAN_FRONTEND=noninteractive

# Pinned so a PPA update cannot silently change the robot-mode output the scan
# parser depends on. The PPA only keeps the newest build, so when it moves on
# this install fails; bump the version after checking the parser fixtures.
ARG MAKEMKV_PPA_VERSION=2.0.0-1~noble

# Runtime dependencies only — no build tools.
# ffmpeg:  used by simplerip for MKV metadata inspection (ffprobe)
# rsync:   used by simplerip for NAS delivery
# makemkv-bin / makemkv-oss: PPA-provided runtime for makemkvcon and its libs
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        eject \
        ffmpeg \
        gnupg \
        libexpat1 \
        libssl3 \
        rsync \
        software-properties-common \
    && add-apt-repository -y ppa:heyarje/makemkv-beta \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        makemkv-bin=${MAKEMKV_PPA_VERSION} \
        makemkv-oss=${MAKEMKV_PPA_VERSION} \
    && rm -rf /var/lib/apt/lists/*

# simplerip binary is statically linked — no extra runtime deps.
COPY --from=gobuilder /simplerip /usr/local/bin/simplerip

RUN ldconfig \
    && mkdir -p /root/.MakeMKV \
    && mkdir -p /staging && chmod 755 /staging

COPY --chmod=755 entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
