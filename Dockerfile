# syntax=docker/dockerfile:1

# --- Stage 1: admin SPA ------------------------------------------------------
FROM node:24-alpine AS web
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts
COPY web/admin/ ./
RUN npm run build

# --- Stage 2: Go binaries ----------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# The SPA is embedded via //go:embed all:dist, so it must sit in the tree
# before go build. Copy from the web stage rather than trusting whatever the
# developer's local dist/ holds.
RUN rm -rf internal/httpapi/adminui/dist && mkdir -p internal/httpapi/adminui/dist
COPY --from=web /src/web/admin/build/ internal/httpapi/adminui/dist/
# One invocation builds both binaries so shared packages compile once; the
# cache mounts keep incremental builds across `make image` runs. VERSION is
# the git sha the CI workflow passes in (.dockerignore drops .git, so the
# binary cannot read it itself); it tags Sentry events.
ARG VERSION=
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ ./cmd/sidecar ./cmd/sidecar-admin

# --- Stage 3: litestream -----------------------------------------------------
# Streams the SQLite file to an S3-compatible bucket (README, Backups). Only
# active at runtime when SIDECAR_BACKUP_BUCKET is set; otherwise the binary
# sits unused. Pinned by version; TARGETARCH comes from buildx.
FROM alpine:3.22 AS litestream
ARG TARGETARCH
ARG LITESTREAM_VERSION=0.5.16
RUN apk add --no-cache curl \
 && case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=arm64 ;; *) echo "unsupported arch $TARGETARCH" && exit 1 ;; esac \
 && curl -fsSL "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-${LITESTREAM_VERSION}-linux-${arch}.tar.gz" \
    | tar -xz -C /usr/local/bin litestream

# --- Stage 4: runtime --------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S sidecar && adduser -S -G sidecar sidecar \
 && mkdir -p /data && chown sidecar:sidecar /data
COPY --from=build /out/sidecar /out/sidecar-admin /usr/local/bin/
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY deploy/litestream.yml /etc/litestream.yml
USER sidecar
WORKDIR /data
ENV SIDECAR_DB=/data/sidecar.db
EXPOSE 8080
ENTRYPOINT ["entrypoint.sh"]
