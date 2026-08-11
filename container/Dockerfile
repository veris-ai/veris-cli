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
# No privilege dropper: `veris-proxy serve --transparent` drops itself once the
# rules are in, so an image adopting this needs a rule writer and nothing else.
# Both writers ship because the choice belongs to the HOST kernel, not this
# image: iptables' owner match needs xt_owner, which minimal kernels omit, and
# serve falls back to nftables there.
RUN apk add --no-cache ca-certificates iptables nftables

# cloudflared opens the callback path. Baked in rather than fetched at run time:
# putting a network download on the startup path of the thing whose job is to be
# reliable before the command starts is the wrong trade. Pinned rather than
# `latest`, so an image built today and one built next month are the same image.
ARG TARGETARCH
ARG CLOUDFLARED_VERSION=2026.7.3
ADD https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-${TARGETARCH} /usr/local/bin/cloudflared
RUN chmod +x /usr/local/bin/cloudflared && cloudflared --version
COPY --from=build /out/veris-proxy /usr/local/bin/veris-proxy
COPY container/entrypoint.sh /usr/local/bin/veris-entrypoint
RUN chmod +x /usr/local/bin/veris-entrypoint && mkdir -p /veris/ca
ENV VERIS_CONFIG=/veris/config.json VERIS_CA_DIR=/veris/ca
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/veris-entrypoint"]
