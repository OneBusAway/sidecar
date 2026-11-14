# syntax=docker/dockerfile:1

# Every base image is pinned by digest (multi-arch index), with its tag kept
# for readers: a moved tag cannot change what is built without a commit here.
# Bump a digest deliberately -- e.g. `docker buildx imagetools inspect
# golang:1.26-alpine` for a new Go patch release -- and expect
# deploy/manifests_test.go to fail on any unpinned FROM.

# --- Stage 1: admin SPA ------------------------------------------------------
FROM node:24-alpine@sha256:116b800c3754b440ee0aa3ed2f20f5bf3d99a554bfd9adeea09ea8017f6e85f5 AS web
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts
COPY web/admin/ ./
RUN npm run build

# --- Stage 2: Go binaries ----------------------------------------------------
FROM golang:1.26-alpine@sha256:30b1e546a9c7fa3b747c2bdba3a0c7bdf3a02ebb4c5b8405d2f948d7e8aac4ec AS build
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
# sits unused. Pinned by version and verified against the SHA-256 of each
# release tarball (from the release's checksums.txt, cross-checked against
# the downloaded files); bump all three together. TARGETARCH comes from
# buildx.
FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f AS litestream
ARG TARGETARCH
ARG LITESTREAM_VERSION=0.5.16
ARG LITESTREAM_SHA256_X86_64=9e29112380a942e4a62ee07773684396cb8b308dc4d67e130bef41f75e937f0a
ARG LITESTREAM_SHA256_ARM64=678022e4103145302598e35d37f8718392d42e153feeb1e2d4a64dd0cd3aaf10
RUN apk add --no-cache curl \
 && case "$TARGETARCH" in \
      amd64) arch=x86_64 sum="$LITESTREAM_SHA256_X86_64" ;; \
      arm64) arch=arm64 sum="$LITESTREAM_SHA256_ARM64" ;; \
      *) echo "unsupported arch $TARGETARCH" && exit 1 ;; \
    esac \
 && curl -fsSL --proto '=https' --tlsv1.2 -o /tmp/litestream.tar.gz \
    "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-${LITESTREAM_VERSION}-linux-${arch}.tar.gz" \
 && echo "${sum}  /tmp/litestream.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/litestream.tar.gz -C /usr/local/bin litestream \
 && rm /tmp/litestream.tar.gz

# --- Stage 4: runtime --------------------------------------------------------
FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
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
