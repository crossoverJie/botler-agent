# botler-agent v2 (Rust) container.
# Multi-stage build: the release binary is a single native artifact, so the
# runtime image carries no toolchain, no Node, and no source tree.
#
# System packages (kept minimal):
#   git              auto-commit of each DATA_ROOT subproject (safety/git.rs)
#   tzdata           TZ-aware scheduler + __TODAY__ date
#   python3          the `run` tool executes in-project python3 scripts
#   nodejs           the `run` tool also supports .js / .mjs scripts
#   ca-certificates  outbound TLS to model gateways / channels
#   wget             HEALTHCHECK against the loopback monitor port

FROM rust:1.95-bookworm AS builder
WORKDIR /build
COPY Cargo.toml Cargo.lock rust-toolchain.toml ./
COPY crates ./crates
# Keep the deterministic core on stable; pi-sdk is appended via BOTLER_FEATURES
# only after the license/compliance gate is cleared (and then needs nightly).
ARG BOTLER_FEATURES=webui,channels
RUN cargo +stable build --release --locked --features "$BOTLER_FEATURES" -p botler

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git tzdata python3 nodejs ca-certificates wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /build/target/release/botler /usr/local/bin/botler

WORKDIR /app
# Image-level defaults so a bare `docker run` (or a NAS Docker UI) works out of
# the box. Override at container creation (-e WEBUI_ENABLED=0), not in /config/.env.
ENV BOTLER_CONFIG_DIR=/config \
    DATA_ROOT=/data \
    WEBUI_HOST=0.0.0.0 \
    WEBUI_ENABLED=1 \
    SCHEDULER_ENABLED=1 \
    MONITOR_ENABLED=1

VOLUME ["/config", "/data"]

# Liveness: the standalone monitor's /healthz (loopback inside the container).
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8899/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/botler"]
