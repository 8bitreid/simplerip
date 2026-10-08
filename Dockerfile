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
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
        -o /simplerip \
        ./cmd/simplerip


# ── Stage 2: final runtime image ─────────────────────────────────────────────
FROM ubuntu:24.04 AS final

ENV DEBIAN_FRONTEND=noninteractive

# Runtime dependencies only — no build tools.
# ffmpeg:  used by simplerip for MKV metadata inspection (ffprobe)
# rsync:   used by simplerip for NAS delivery
# makemkv-bin / makemkv-oss: PPA-provided runtime for makemkvcon and its libs
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        software-properties-common \
        gnupg \
        ca-certificates \
        libssl3 \
        libexpat1 \
        ffmpeg \
        rsync \
        eject \
    && add-apt-repository -y ppa:heyarje/makemkv-beta \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        makemkv-bin \
        makemkv-oss \
    && rm -rf /var/lib/apt/lists/*

# simplerip binary is statically linked — no extra runtime deps.
COPY --from=gobuilder /simplerip /usr/local/bin/simplerip

RUN ldconfig

RUN mkdir -p /root/.MakeMKV
RUN mkdir -p /staging && chmod 755 /staging

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
