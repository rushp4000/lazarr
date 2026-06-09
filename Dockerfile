# Lazarr — multi-stage build. Pure-Go (CGO off): modernc.org/sqlite + hanwen/go-fuse.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lazarr ./cmd/lazarr

FROM alpine:3.20
RUN apk add --no-cache fuse3 ca-certificates
COPY --from=build /lazarr /usr/local/bin/lazarr
# FUSE requires: --cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor:unconfined
ENTRYPOINT ["/usr/local/bin/lazarr", "--config", "/config/config.yaml"]
