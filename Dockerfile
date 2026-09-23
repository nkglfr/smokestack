# smokestack container image — meant for tests and for people who already
# run everything in containers. For published measurements, prefer the
# native install (install.sh): see the disclaimer in DEPLOY.md § 12.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.BuildDate=$(date -u +%Y-%m-%d)" \
      -o /smokestack .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata libcap && \
    adduser -S -D -H -u 10001 smokestack && \
    mkdir -p /var/lib/smokestack && chown smokestack /var/lib/smokestack
COPY --from=build /smokestack /usr/local/bin/smokestack
# ICMP needs raw sockets. The capability is carried by the binary, so the
# container can run as a normal user; start it with --cap-add=NET_RAW.
RUN setcap cap_net_raw+ep /usr/local/bin/smokestack
USER smokestack
VOLUME /var/lib/smokestack
ENV SMOKESTACK_LISTEN_IP=0.0.0.0 SMOKESTACK_LISTEN_PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- "http://127.0.0.1:${SMOKESTACK_LISTEN_PORT}/healthz" >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/smokestack"]
CMD ["-config", "/var/lib/smokestack/config.json"]
