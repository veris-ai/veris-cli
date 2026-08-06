# Build stage: a static binary, so the runtime stage can be any base image the
# customer already uses, including Alpine and distroless.
FROM golang:1.24-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/veris-proxy ./cmd/veris-proxy

# Distributable stage: just the binary, for COPY --from into any image.
FROM scratch AS binary
COPY --from=build /out/veris-proxy /veris-proxy

# Reference runner. Extend it with your own toolchain, or copy the binary and
# entrypoint into an image you already have.
FROM alpine:3.22 AS runner
RUN apk add --no-cache ca-certificates iptables
COPY --from=build /out/veris-proxy /usr/local/bin/veris-proxy
COPY container/entrypoint.sh /usr/local/bin/veris-entrypoint
RUN chmod +x /usr/local/bin/veris-entrypoint && mkdir -p /veris/ca
ENV VERIS_CONFIG=/veris/config.json VERIS_CA_DIR=/veris/ca
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/veris-entrypoint"]
