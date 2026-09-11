# syntax=docker/dockerfile:1
# image-service: native-CGO build (CON-281). Links libvips (glibc) discovered at
# build time via pkg-config and loaded at runtime from the system lib dir. The
# runtime is debian-slim, NOT scratch — libvips is a shared library with a large
# transitive graph (libglib, libjpeg, libpng, libwebp, libheif, libtiff, ...),
# so a static/scratch image is impractical here. linux/amd64 only.

# ─── build ───────────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS build
WORKDIR /app

# libvips headers + dev libs so CGO can compile+link govips. bookworm's libvips
# is built with HEIC/AVIF/WebP/TIFF support (via libheif/libwebp/libtiff), which
# the format allow-list needs.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      libvips-dev pkg-config; \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /image-service ./cmd/image-service

# ─── runtime ─────────────────────────────────────────────────────────────────
# Debian slim + the libvips RUNTIME package (not -dev) and its codec deps. Also
# ships grpc_health_probe since there is no HTTP endpoint to health-check.
FROM debian:bookworm-slim
ARG GRPC_HEALTH_PROBE_VERSION=v0.4.34
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates libvips wget; \
    wget -qO /usr/local/bin/grpc_health_probe \
      "https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-amd64"; \
    chmod +x /usr/local/bin/grpc_health_probe; \
    apt-get purge -y wget; apt-get autoremove -y; rm -rf /var/lib/apt/lists/*; \
    useradd -r -u 10001 app

COPY --from=build /image-service /usr/local/bin/image-service
USER app

ENV IMAGE_SERVICE_LISTEN=":50051"
EXPOSE 50051

# Private-network only — orchestrators probe gRPC health via grpc_health_probe.
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/grpc_health_probe", "-addr=:50051"]

ENTRYPOINT ["/usr/local/bin/image-service"]
