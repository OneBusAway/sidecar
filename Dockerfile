# syntax=docker/dockerfile:1

# --- Stage 1: admin SPA ------------------------------------------------------
FROM node:24-alpine AS web
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci
COPY web/admin/ ./
RUN npm run build

# --- Stage 2: Go binaries ----------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The SPA is embedded via //go:embed all:dist, so it must sit in the tree
# before go build. Copy from the web stage rather than trusting whatever the
# developer's local dist/ holds.
RUN rm -rf internal/httpapi/adminui/dist && mkdir -p internal/httpapi/adminui/dist
COPY --from=web /src/web/admin/build/ internal/httpapi/adminui/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sidecar ./cmd/sidecar \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sidecar-admin ./cmd/sidecar-admin

# --- Stage 3: runtime --------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S sidecar && adduser -S -G sidecar sidecar \
 && mkdir -p /data && chown sidecar:sidecar /data
COPY --from=build /out/sidecar /out/sidecar-admin /usr/local/bin/
USER sidecar
WORKDIR /data
ENV SIDECAR_DB=/data/sidecar.db
EXPOSE 8080
ENTRYPOINT ["sidecar"]
