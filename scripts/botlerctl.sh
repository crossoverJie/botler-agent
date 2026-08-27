#!/usr/bin/env bash
#
# botlerctl.sh — local control script for the botler-agent process.
#
# Usage:
#   botlerctl.sh restart [main|stable]   update code then (re)start in background
#   botlerctl.sh start                   start in background (no code update)
#   botlerctl.sh stop                    stop the running process
#   botlerctl.sh status                  show whether it is running
#   botlerctl.sh logs                    tail the log file
#
# Notes:
#   - Only touches the repo directory. Never touches DATA_ROOT or ~/.botler-agent/.
#   - `restart` discards local working-tree changes (git checkout -f).
#   - stable mode resolves the latest GitHub release tag; if none exists yet,
#     it falls back to main with a warning.
#
# Configurable env vars:
#   BOTLER_GITHUB_REPO   owner/name   (default: crossoverJie/botler-agent)
#   BOTLER_GITHUB_API    base url     (default: https://api.github.com)
#   HTTPS_PROXY / https_proxy         used automatically by curl / gh

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIDFILE="$REPO_DIR/.botler.pid"
LOGFILE="$REPO_DIR/botler.log"
REMOTE="origin"
MAIN_BRANCH="main"

GITHUB_REPO="${BOTLER_GITHUB_REPO:-crossoverJie/botler-agent}"
GITHUB_API="${BOTLER_GITHUB_API:-https://api.github.com}"

is_running() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null
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
  # Safety net: reap any lingering tsx index process.
  pkill -f "tsx src/index.ts" 2>/dev/null || true
  echo "stopped"
}

start() {
  if is_running; then
    echo "already running (pid $(cat "$PIDFILE"))" >&2
    return 0
  fi
  cd "$REPO_DIR"
  # shellcheck disable=SC2093
  nohup npm start >"$LOGFILE" 2>&1 &
  local pid=$!
  echo "$pid" >"$PIDFILE"
  echo "started (pid $pid), log: $LOGFILE"
}

update_main() {
  cd "$REPO_DIR"
  echo "updating main..."
  git fetch "$REMOTE" "$MAIN_BRANCH"
  git checkout -f "$REMOTE/$MAIN_BRANCH"
  npm install
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
    echo "warn: no stable release found for $GITHUB_REPO, falling back to main" >&2
    update_main
    return 0
  fi
  cd "$REPO_DIR"
  echo "stable: checking out $tag"
  git fetch --tags "$REMOTE"
  git checkout -f "$tag"
  npm install
}

restart() {
  local mode="${1:-main}"
  stop
  if [[ "$mode" == "stable" ]]; then
    update_stable
  else
    update_main
  fi
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
  restart) restart "${2:-main}" ;;
  start)   start ;;
  stop)    stop ;;
  status)  status ;;
  logs)    logs ;;
  *) echo "usage: $0 {restart|start|stop|status|logs} [main|stable]" >&2; exit 1 ;;
esac
