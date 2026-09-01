# botler-agent container.
# Runs the persistent channel via tsx (no build step); tsx is a runtime dependency.
#
# Extra system packages (kept minimal):
#   git        auto-commit of each DATA_ROOT subproject (safety/git.ts)
#   tzdata     TZ-aware scheduler + __TODAY__ date
#   python3    the `run` tool executes in-project python3 scripts (.py)
#   ca-certificates  outbound TLS to model providers / channels

FROM node:20-alpine

RUN apk add --no-cache git tzdata python3 ca-certificates

WORKDIR /app

COPY package.json package-lock.json ./
# --ignore-scripts matches the repo CI (ci.yml) and skips native postinstall
# binaries (e.g. esbuild) that crash under multi-arch QEMU emulation.
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY bin ./bin
COPY src ./src
COPY tsconfig.json .env.example ./

# Image-level defaults so a bare `docker run` (or the NAS Docker UI) works out of the
# box. BOTLER_CONFIG_DIR / DATA_ROOT are container-location concerns; WEBUI_HOST is
# 0.0.0.0 so the UI is reachable from the host; the three switches are ON by default.
# These ENV values take precedence over the mounted /config/.env — override them at
# container creation (e.g. `-e WEBUI_ENABLED=0`), not inside /config/.env.
ENV BOTLER_CONFIG_DIR=/config \
    DATA_ROOT=/data \
    WEBUI_HOST=0.0.0.0 \
    WEBUI_ENABLED=1 \
    SCHEDULER_ENABLED=1 \
    MONITOR_ENABLED=1

VOLUME ["/config", "/data"]

# Liveness: the monitor's /healthz (bound 127.0.0.1 inside the container). Requires
# MONITOR_ENABLED to stay on (default). 
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8899/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["node", "bin/botler.mjs"]
