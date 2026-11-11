#!/usr/bin/env bash
# Parity harness: drive v1 (TypeScript + pi-ai) and v3 (Go) with the same scripted model output
# and compare everything user-visible (task logs, conversation history, data files, commit
# messages).
#
# How the model is made deterministic: v1 hard-binds pi-ai's models.streamSimple at module load,
# so instead of patching v1 we point providers.json at a local OpenAI-compatible stub
# (scripts/parity/stub), which replays a fixed assistant/tool-call sequence. The stub supports
# both SSE (pi-ai always streams) and plain JSON (the Go client), so both sides see the same
# bytes of model output.
#
# Usage:
#   scripts/parity.sh                 # run the default scenario
#   scripts/parity.sh scenarios/x.json
#
# Requirements: go, python3, node+npx (for the v1 side; skipped with SKIP_V1=1).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCENARIO="${1:-$REPO_ROOT/scripts/parity/scenarios/basic.json}"
PORT="${STUB_PORT:-18099}"
SKIP_V1="${SKIP_V1:-0}"

WORK="$(mktemp -d)"
trap 'kill "${STUB_PID:-0}" 2>/dev/null || true; rm -rf "$WORK"' EXIT

echo "==> work dir: $WORK"
mkdir -p "$WORK/data/cook" "$WORK/cfg-v1" "$WORK/cfg-go" "$WORK/bin"

cat > "$WORK/data/cook/AGENTS.md" <<'EOF'
# cook

记账数据在 cook/data.json。

路由提示：「记账」
EOF
echo '{"items":[]}' > "$WORK/data/cook/data.json"

# One shared providers.json template, pointed at the stub.
providers='{
  "providers": {
    "stub": {
      "api": "openai-completions",
      "baseUrl": "http://127.0.0.1:'"$PORT"'/v1",
      "apiKey": "stub-key",
      "models": [{"id": "stub-model", "name": "Stub", "contextWindow": 100000, "maxTokens": 4096}]
    }
  }
}'
printf '%s\n' "$providers" > "$WORK/cfg-v1/providers.json"
printf '%s\n' "$providers" > "$WORK/cfg-go/providers.json"

env_file="DATA_ROOT=$WORK/data
PI_PROVIDER=stub
PI_MODEL=stub-model
WEBUI_ENABLED=0
SCHEDULER_ENABLED=0
MONITOR_ENABLED=0
GIT_PUSH=0
CONVERSATION_CONTEXT_ENABLED=1"
printf '%s\n' "$env_file" > "$WORK/cfg-v1/.env"
printf '%s\n' "$env_file" > "$WORK/cfg-go/.env"

echo "==> starting model stub"
go build -o "$WORK/bin/stub" ./scripts/parity/stub
"$WORK/bin/stub" -addr "127.0.0.1:$PORT" -script "$SCENARIO" &
STUB_PID=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.2
done

MESSAGE="${PARITY_MESSAGE:-记一下：今天午饭 30 元}"
status=0

if [[ "$SKIP_V1" != "1" ]]; then
  echo "==> running v1 (TypeScript)"
  ( cd "$REPO_ROOT" && BOTLER_CONFIG_DIR="$WORK/cfg-v1" PI_PROVIDER=stub PI_MODEL=stub-model \
      npx tsx src/index.ts "$MESSAGE" ) \
    | tee "$WORK/v1.out" || { echo "v1 run failed"; status=1; }
  cp -R "$WORK/data" "$WORK/data-v1"
  git -C "$WORK/data" checkout -- . 2>/dev/null || true
fi

echo "==> running v3 (Go)"
( cd "$REPO_ROOT" && go build -o "$WORK/bin/botler" ./cmd/botler )
# Process env beats .env by design (config priority: env > user .env > built-in defaults), so the
# harness pins the model selection explicitly instead of inheriting the developer's shell.
( cd "$REPO_ROOT" && BOTLER_CONFIG_DIR="$WORK/cfg-go" PI_PROVIDER=stub PI_MODEL=stub-model \
    "$WORK/bin/botler" "$MESSAGE" ) \
  | tee "$WORK/go.out" || { echo "go run failed"; status=1; }

if [[ "$SKIP_V1" == "1" ]]; then
  echo "SKIP_V1=1: Go run completed; artifacts in $WORK"
  echo "--- reply ---"; cat "$WORK/go.out"
  exit "$status"
fi

echo "==> comparing"
python3 "$REPO_ROOT/scripts/parity/normalize.py" "$WORK" || status=1
echo "--- diff of data trees (v1 vs v3) ---"
diff -r "$WORK/data-v1" "$WORK/data" || status=1
exit "$status"
