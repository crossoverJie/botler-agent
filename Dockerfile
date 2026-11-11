# Botler Agent (Go) — multi-arch, static binary image.
#
# Compilation runs on the host architecture ($BUILDPLATFORM) and targets $TARGETARCH, so
# multi-arch builds do not pay the QEMU tax for the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags "-s -w -X github.com/crossoverJie/botler-agent/internal/version.Version=$VERSION -X github.com/crossoverJie/botler-agent/internal/version.Commit=$COMMIT" \
    -o /out/botler ./cmd/botler

FROM alpine:3.22
# git + openssh-client: data subprojects are git repos (GIT_PUSH=1).
# python3 / nodejs: the `run` tool only executes existing in-project .py / .js screens.
# tzdata: IANA zones for the scheduler; ca-certificates: HTTPS model/WeChat/holiday APIs.
RUN apk add --no-cache git openssh-client tzdata python3 nodejs ca-certificates
COPY --from=build /out/botler /usr/local/bin/botler
# Same defaults as v1 (ports must not change).
ENV BOTLER_CONFIG_DIR=/config DATA_ROOT=/data WEBUI_HOST=0.0.0.0 \
    WEBUI_ENABLED=1 WEBUI_PORT=8900 \
    SCHEDULER_ENABLED=1 MONITOR_ENABLED=1 MONITOR_PORT=8899
VOLUME ["/config", "/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8899/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["botler"]
