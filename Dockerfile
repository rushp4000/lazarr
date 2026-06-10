# Lazarr — multi-stage build. Pure-Go (CGO off): modernc.org/sqlite + hanwen/go-fuse.
FROM golang:1.26-alpine AS build
# VERSION stamps internal/version.Version so the GHCR image's /health reports the real
# release (S7). Defaults to "dev" for local builds; the release workflow passes the tag.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/rushp4000/lazarr/internal/version.Version=${VERSION}" \
    -o /lazarr ./cmd/lazarr

FROM alpine:3.20
RUN apk add --no-cache fuse3 ca-certificates
# Allow non-mounting uids (Plex, the *arr suite) to read the FUSE tree. Needed
# because go-fuse's AllowOther goes through fusermount3, which refuses
# allow_other unless user_allow_other is enabled here (rclone/decypharr do the
# same). Harmless on a single-tenant trusted-LAN media host.
RUN echo "user_allow_other" >> /etc/fuse.conf
COPY --from=build /lazarr /usr/local/bin/lazarr
# FUSE requires: --cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined
ENTRYPOINT ["/usr/local/bin/lazarr", "--config", "/config/config.yaml"]
