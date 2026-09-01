# Running botler-agent in Docker (NAS deployment)

This doc describes how botler-agent is packaged and published as a Docker image, and how to run it
on a home NAS (e.g. UGREEN 绿联, Synology, QNAP) with a plain `docker run` (or the NAS Docker UI).

## Overview

botler-agent is a long-running Node.js process (`tsx src/index.ts`, no build step). It reads its
real config from `~/.botler-agent/` (`BOTLER_CONFIG_DIR`) and operates on the data root (`DATA_ROOT`).
Both are containerized as two mounted volumes, so everything persists across image upgrades:

| Host concern | In-container path | Mount |
| --- | --- | --- |
| Config dir (`BOTLER_CONFIG_DIR`) | `/config` | `./config:/config` |
| Data root (`DATA_ROOT`) | `/data` | `./data:/data` |

The image runs as **root** (simplest for NAS; no UID/GID mapping needed).

## Code change required (already done)

The WebUI (`src/webui/server.ts`) used to hard-bind `127.0.0.1`, which is unreachable from the NAS
host inside a container. A `WEBUI_HOST` env was added (`src/config.ts`) so the bind address is
configurable:

- Default stays `127.0.0.1` (unchanged behavior, local-only).
- The image defaults `WEBUI_HOST=0.0.0.0` so the host can reach the UI.

No other source change was needed — `BOTLER_CONFIG_DIR`, `DATA_ROOT`, `TZ`, and the channel/model
env vars were already supported.

## Files added

| File | Purpose |
| --- | --- |
| `Dockerfile` | `node:20-alpine` + `git tzdata python3 ca-certificates`, `npm ci --omit=dev`, image defaults |
| `.dockerignore` | Exclude `node_modules`, `.git`, docs, etc. from the build context |
| `.github/workflows/docker-publish.yml` | Multi-arch build → Docker Hub on `main` (dev channel) |
| `.github/workflows/docker-release.yml` | Multi-arch build → Docker Hub on `v*` tag (release channel) |
| `docs/docker.md` | This document |

## Image contents

Base `node:20-alpine` plus the minimal system packages the framework needs:

- **git** — `safety/git.ts` auto-commits each changed `DATA_ROOT` subproject.
- **tzdata** — correct local time for the scheduler and the `__TODAY__` date.
- **python3** — the `run` tool executes in-project `.py` scripts.
- **ca-certificates** — outbound TLS to model providers and channels.

`tsx` is a runtime dependency, so the image runs `node bin/botler.mjs` directly (no build step).
`typescript` is a devDependency and is dropped by `npm ci --omit=dev`.

## Build & publish (Docker Hub)

Publishing is fully automated by two workflows:

- `.github/workflows/docker-publish.yml` — on every push to `main` (or manual): pushes `edge` + short-sha (dev channel).
- `.github/workflows/docker-release.yml` — on a `v*` tag push: pushes `latest` + `v0.2.0` + `0.2.0` + `0.2` (release channel). `latest` is reserved for tagged releases, so it always points at the latest stable version.

- **Platforms**: `linux/amd64` + `linux/arm64` (covers x86 and ARM NAS boxes).
- **Target**: Docker Hub image `crossoverjie/botler-agent` (override via repo variable `DOCKERHUB_IMAGE`).

Required repo secrets: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` (a Docker Hub access token, not the
account password). Set them under **Repo settings → Secrets and variables → Actions**.

Building locally (optional, for testing):

```bash
docker build -t botler-agent:local .
docker buildx build --platform linux/amd64,linux/arm64 -t <user>/botler-agent:test .
```

## Deploying on a NAS

1. Create two folders on the NAS for the volumes, e.g.:

   ```text
   /volume1/docker/botler/
   ├── config/          # mounted as /config
   └── data/            # mounted as /data
   ```

2. **First run — create the config** (either way):
   - Manual: create `config/.env` with the keys below, or
   - Generate a template in-place:
     ```bash
     docker run --rm -v /volume1/docker/botler/config:/config <image> init
     ```
     (copies the `.env.example` / `providers.json` / `system-prompt.md` / `schedules.json`
     templates into `/config`; existing files are not overwritten).

3. Edit `config/.env`: set the model (`PI_PROVIDER` / `PI_MODEL`) and your channel credentials.
   The `DATA_ROOT=` line left over from the template is harmless — it is overridden by the image's
   baked-in `/data` — but you can set or delete it. `BOTLER_CONFIG_DIR` and `DATA_ROOT` are already
   baked into the image, so you normally don't repeat them.

4. Start the container:

   ```bash
   docker run -d \
     --name botler \
     --restart unless-stopped \
     -e TZ=Asia/Shanghai \
     -p 8900:8900 \
     -v /volume1/docker/botler/config:/config \
     -v /volume1/docker/botler/data:/data \
      crossoverjie/botler-agent:latest
   ```

   If you use the Feishu channel, add `-p 3000:3000` to the command above.

5. Open the WebUI at `http://<nas-ip>:8900`.

### Environment / ports reference

| Env / port | Purpose | Default in container |
| --- | --- | --- |
| `WEBUI_HOST` | WebUI bind host | `0.0.0.0` (image) |
| `WEBUI_ENABLED` | Task-log UI | `1` (image) |
| `SCHEDULER_ENABLED` | In-process scheduler | `1` (image) |
| `MONITOR_ENABLED` | Health/metrics server | `1` (image) |
| `8900` | WebUI | mapped to host |
| `3000` | Feishu webhook | map only if using Feishu |
| `8899` | Monitor `/metrics` | map only for external scraping |

> **Precedence note**: the `WEBUI_HOST` / `WEBUI_ENABLED` / `SCHEDULER_ENABLED` / `MONITOR_ENABLED`
> defaults are baked into the image and take precedence over the mounted `/config/.env` (the loader
> only fills keys that are not already set in the process environment). To change a default-on
> switch, override it at container creation (e.g. `-e WEBUI_ENABLED=0`, or the NAS UI's env field).

## Upgrading

To upgrade to the latest tagged release (or a specific version), pull the new image and recreate the
container — the `/config` and `/data` volumes persist, so no config is lost:

```bash
docker pull crossoverjie/botler-agent:latest
docker stop botler
docker rm botler
# re-run the same `docker run` command from the "Deploying" section
```

For a specific version, replace `:latest` with e.g. `:0.2.0`. The `edge` tag tracks `main` (unstable).

## WeChat login inside Docker

The WeChat channel needs an interactive QR login. Run it once with a TTY, then the credentials
(`/config/wechat/account.json`) persist and the normal channel picks them up:

```bash
docker run -it --rm -v /volume1/docker/botler/config:/config <image> wechat-login
```

The ASCII QR code is printed in the terminal; scan it with WeChat. After success, **set
`WECHAT_ENABLED=1` in `config/.env`** (the template ships with it `0`), then restart the container
(`docker restart botler`). Only then will the WeChat monitor start with the saved token —
`src/index.ts` skips the channel entirely while `WECHAT_ENABLED` is off, and it fails silently.

## Git commit identity (the most common NAS gotcha)

The framework auto-commits each changed `DATA_ROOT` subproject. In the container, `git commit` needs
a `user.name`/`user.email`; the host's global `~/.gitconfig` is **not** present. Pick one:

- **Recommended** — set it once inside each data repo (persists with `/data`):

  ```bash
  cd data/<project>
  git config user.name  "Your Name"
  git config user.email "you@example.com"
  ```

- Or mount the host config read-only at container creation:

  ```bash
  -v ~/.gitconfig:/root/.gitconfig:ro
  ```

A failed commit is only a warning (not a blocker), but tasks won't be committed without this.

## Healthcheck

The container healthcheck probes `http://127.0.0.1:8899/healthz` (the monitor server, bound to
loopback inside the container). It relies on `MONITOR_ENABLED=1` (the default). If you disable the
monitor, the healthcheck will report unhealthy — that's expected and harmless.

## Security notes

- The container runs as **root** for zero-friction NAS setup. It only touches the two mounted
  volumes; keep those on the NAS and don't mount anything else.
- `WEBUI_HOST=0.0.0.0` exposes the WebUI to the LAN. It is read-mostly (the only write action is
  deleting task logs / editing config), but do not expose port 8900 to the public internet without
  a reverse proxy + auth.
- The `safePath` allowlist and the "no arbitrary shell" rules are unchanged — the container does
  not relax any security boundary, it only relocates the process.
