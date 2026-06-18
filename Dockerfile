# ── Stage 1: build the simplerip Go binary ──────────────────────────────────
FROM golang:1.25-alpine AS gobuilder

WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /simplerip \
        ./cmd/simplerip


# ── Stage 2: build makemkvcon from source ────────────────────────────────────
# This stage is intentionally heavy (build tools, codec headers).
# Only the compiled binary is carried forward to the final image.
FROM ubuntu:24.04 AS makemkv

ARG MAKEMKV_VERSION=1.18.4
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        libc6-dev \
        libssl-dev \
        libexpat1-dev \
        libavcodec-dev \
        libavutil-dev \
        zlib1g-dev \
        wget \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Download both tarballs in a single layer to keep the image clean.
RUN wget -q \
        "https://www.makemkv.com/download/makemkv-oss-${MAKEMKV_VERSION}.tar.gz" \
        "https://www.makemkv.com/download/makemkv-bin-${MAKEMKV_VERSION}.tar.gz" \
    && tar -xzf "makemkv-oss-${MAKEMKV_VERSION}.tar.gz" \
    && tar -xzf "makemkv-bin-${MAKEMKV_VERSION}.tar.gz"

# Build the open-source library.
# --disable-gui skips the Qt/X11 dependency; we only need makemkvcon (CLI).
RUN cd "makemkv-oss-${MAKEMKV_VERSION}" \
    && ./configure --disable-gui \
    && make -j"$(nproc)" \
    && make install

# Accept the MakeMKV End-User Licence Agreement and build the CLI binary.
# Creating the sentinel file tells the Makefile that the EULA has been read
# and accepted — equivalent to pressing Enter at the interactive prompt.
# By building this image you agree to the MakeMKV EULA (see makemkv.com).
RUN cd "makemkv-bin-${MAKEMKV_VERSION}" \
    && mkdir -p tmp \
    && touch tmp/eula_accepted \
    && make -j"$(nproc)" \
    && make install


# ── Stage 3: final runtime image ─────────────────────────────────────────────
FROM ubuntu:24.04 AS final

ENV DEBIAN_FRONTEND=noninteractive

# Runtime dependencies only — no build tools.
# ffmpeg:  used by simplerip for MKV metadata inspection (ffprobe)
# rsync:   used by simplerip for NAS delivery
# libssl3, libexpat1, libavcodec*, libavutil*: makemkvcon runtime libs
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libssl3 \
        libexpat1 \
        ffmpeg \
        rsync \
        eject \
        udev \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# simplerip binary is statically linked — no extra runtime deps.
COPY --from=gobuilder /simplerip /usr/local/bin/simplerip

# makemkvcon binary and the makemkv shared library it links against.
COPY --from=makemkv /usr/bin/makemkvcon        /usr/local/bin/makemkvcon
COPY --from=makemkv /usr/bin/mmgplsrv          /usr/local/bin/mmgplsrv
COPY --from=makemkv /usr/bin/mmccextr          /usr/local/bin/mmccextr
COPY --from=makemkv /usr/lib/libdriveio.so.0   /usr/lib/libdriveio.so.0
COPY --from=makemkv /usr/lib/libmakemkv.so.1   /usr/lib/libmakemkv.so.1
COPY --from=makemkv /usr/lib/libmmbd.so.0      /usr/lib/libmmbd.so.0
COPY --from=makemkv /usr/share/MakeMKV         /usr/share/MakeMKV
RUN ldconfig

RUN mkdir -p /root/.MakeMKV
RUN mkdir -p /staging && chmod 755 /staging

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
