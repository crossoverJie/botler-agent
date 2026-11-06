#!/usr/bin/env bash
#
# botlerctl.sh — local control script for the botler-agent v2 binary.
#
# Usage:
#   botlerctl.sh restart [branch|stable] update code, build, then (re)start
#                                        "branch" is the default and follows
#                                        BOTLER_BRANCH (default: v2). Do not
#                                        update "main": it is the v1 TypeScript
#                                        tree and has no Rust workspace.
#   botlerctl.sh start                   start in background (no rebuild)
#   botlerctl.sh build                   cargo build --release
#   botlerctl.sh stop                    stop the running process
#   botlerctl.sh status                  show whether it is running
#   botlerctl.sh logs                    tail the log file
#
# Notes:
#   - Only touches the repo directory. Never touches DATA_ROOT or ~/.botler-agent/.
#   - `restart` discards local working-tree changes (git checkout -f).
#   - stable mode resolves the latest GitHub release tag; if none exists yet,
#     it falls back to main with a warning.
#   - The binary loads its own config from ~/.botler-agent/.env (and the source
#     .env fallback), so nothing is exported by this script.
#
# Configurable env vars:
#   BOTLER_GITHUB_REPO   owner/name   (default: crossoverJie/botler-agent)
#   BOTLER_GITHUB_API    base url     (default: https://api.github.com)
#   BOTLER_BIN           binary path  (default: target/release/botler)
#   BOTLER_CARGO         cargo binary (default: cargo)
#   BOTLER_CARGO_TOOLCHAIN  rustup toolchain override (default: +stable);
#                        set to "" to honor the repo's rust-toolchain.toml
#   BOTLER_FEATURES      cargo features (default: channels)
#   HTTPS_PROXY / https_proxy         used automatically by curl / gh

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIDFILE="$REPO_DIR/.botler.pid"
LOGFILE="$REPO_DIR/botler.log"
REMOTE="origin"
# The Rust rewrite lives on this branch; `main` is the v1 TypeScript tree.
BRANCH="${BOTLER_BRANCH:-v2}"

GITHUB_REPO="${BOTLER_GITHUB_REPO:-crossoverJie/botler-agent}"
GITHUB_API="${BOTLER_GITHUB_API:-https://api.github.com}"
BIN="${BOTLER_BIN:-$REPO_DIR/target/release/botler}"
CARGO="${BOTLER_CARGO:-cargo}"
FEATURES="${BOTLER_FEATURES:-channels}"

# rustup installs cargo into ~/.cargo/bin, which non-interactive shells
# (launchd, cron, this script) often lack on PATH. Source its env only when
# cargo is not already resolvable, so a caller-provided BOTLER_CARGO wins.
if ! command -v "$CARGO" >/dev/null 2>&1 && [[ -f "$HOME/.cargo/env" ]]; then
  # shellcheck disable=SC1091
  . "$HOME/.cargo/env"
fi

# rust-toolchain.toml pins a nightly for the unresolved pi-sdk feature. The
# deterministic core / channels build is stable-only (see AGENTS.md), so
# override that pin by default; an empty value honors the pinned toolchain.
CARGO_TOOLCHAIN="${BOTLER_CARGO_TOOLCHAIN-+stable}"

is_running() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null
}

build() {
  cd "$REPO_DIR"
  echo "building ($FEATURES)..."
  if [[ -n "$CARGO_TOOLCHAIN" ]]; then
    "$CARGO" "$CARGO_TOOLCHAIN" build --release --locked --features "$FEATURES" -p botler
  else
    "$CARGO" build --release --locked --features "$FEATURES" -p botler
  fi
}

stop() {
  if is_running; then
    local pid; pid="$(cat "$PIDFILE")"
    echo "stopping (pid $pid)..."
    kill "$pid" 2>/dev/null || true
    local i
    for i in $(seq 1 30); do
      is_running || break
      sleep 1
    done
    if is_running; then
      echo "did not exit gracefully, sending SIGKILL"
      kill -9 "$(cat "$PIDFILE")" 2>/dev/null || true
    fi
    rm -f "$PIDFILE"
  fi
  echo "stopped"
}

start() {
  if is_running; then
    echo "already running (pid $(cat "$PIDFILE"))" >&2
    return 0
  fi
  if [[ ! -x "$BIN" ]]; then
    echo "binary not found at $BIN; run '$0 build' first" >&2
    return 1
  fi
  cd "$REPO_DIR"
  nohup "$BIN" >"$LOGFILE" 2>&1 &
  local pid=$!
  echo "$pid" >"$PIDFILE"
  echo "started (pid $pid), log: $LOGFILE"
}

update_branch() {
  cd "$REPO_DIR"
  echo "updating $BRANCH..."
  git fetch "$REMOTE" "$BRANCH"
  git checkout -f "$REMOTE/$BRANCH"
}

resolve_stable_tag() {
  local tag=""
  if command -v gh >/dev/null 2>&1; then
    tag="$(gh release list --repo "$GITHUB_REPO" --limit 1 --json tagName -q '.[0].tagName' 2>/dev/null || true)"
  fi
  if [[ -z "$tag" ]]; then
    tag="$(curl -fsSL "$GITHUB_API/repos/$GITHUB_REPO/releases/latest" 2>/dev/null \
      | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' \
      | head -1 | sed 's/.*:[[:space:]]*//; s/"//g' )" || true
  fi
  printf '%s' "$tag"
}

update_stable() {
  local tag
  tag="$(resolve_stable_tag)"
  if [[ -z "$tag" ]]; then
    echo "warn: no stable release found for $GITHUB_REPO, falling back to $BRANCH" >&2
    update_branch
    return 0
  fi
  cd "$REPO_DIR"
  echo "stable: checking out $tag"
  git fetch --tags "$REMOTE"
  git checkout -f "$tag"
}

restart() {
  local mode="${1:-branch}"
  case "$mode" in
    branch) update_branch ;;
    stable) update_stable ;;
    *)
      echo "unknown restart target \"$mode\"" >&2
      echo "usage: $0 {restart|start|build|stop|status|logs} [branch|stable]" >&2
      exit 1
      ;;
  esac
  stop
  build
  start
}

status() {
  if is_running; then
    echo "running (pid $(cat "$PIDFILE"))"
  else
    echo "not running"
  fi
}

logs() {
  tail -f "$LOGFILE"
}

case "${1:-restart}" in
  restart) restart "${2:-branch}" ;;
  start)   start ;;
  build)   build ;;
  stop)    stop ;;
  status)  status ;;
  logs)    logs ;;
  *) echo "usage: $0 {restart|start|build|stop|status|logs} [branch|stable]" >&2; exit 1 ;;
esac
