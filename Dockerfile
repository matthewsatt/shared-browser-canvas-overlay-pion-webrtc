FROM golang:1.24-bookworm AS build
WORKDIR /src
# Dependencies first (cached), pinned by go.sum; the build may not change them.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/collab-browser ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium ffmpeg ca-certificates tini \
      fonts-liberation fonts-noto-core fonts-noto-color-emoji fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 app
COPY --from=build /out/collab-browser /usr/local/bin/collab-browser
USER app
# Chromium's sandbox is on. It needs to create user namespaces, which
# Docker's default seccomp profile only allows with CAP_SYS_ADMIN; run with
# deploy/chromium-seccomp.json (docker-compose.yml does), which adds exactly
# those calls to Docker's default profile. HOME is /tmp so the root
# filesystem can be read-only.
ENV CHROME_PATH=/usr/bin/chromium \
    CHROME_NO_SANDBOX=false \
    HOME=/tmp \
    LISTEN_ADDR=:8080 \
    WEBRTC_UDP_PORT_MIN=50000 \
    WEBRTC_UDP_PORT_MAX=50100 \
    WEBRTC_PUBLIC_IPS=127.0.0.1
EXPOSE 8080 50000-50100/udp
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/collab-browser", "healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/collab-browser"]
