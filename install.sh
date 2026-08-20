#!/usr/bin/env bash
# botler-agent one-liner installer.
# Usage: curl -fsSL https://raw.githubusercontent.com/crossoverJie/botler-agent/main/install.sh | bash
# Idempotent: re-running updates an existing clone (git pull) instead of re-cloning.
set -euo pipefail

REPO="https://github.com/crossoverJie/botler-agent.git"
DIR="${BOTLER_INSTALL_DIR:-$HOME/.local/share/botler-agent}"

if [ -d "$DIR/.git" ]; then
  echo "==> Updating existing install at $DIR"
  git -C "$DIR" pull
else
  echo "==> Cloning botler-agent into $DIR"
  git clone "$REPO" "$DIR"
fi

cd "$DIR"
echo "==> Installing dependencies"
npm install
echo "==> Initializing user config (~/.botler-agent/)"
npm run init

cat <<EOF

Done. Next steps:
  1. Edit ~/.botler-agent/.env — fill in DATA_ROOT, model selection (PI_PROVIDER / PI_MODEL), channel credentials.
  2. Edit ~/.botler-agent/providers.json with your model providers (baseUrl / apiKey / models).
  3. Run from this directory:  cd $DIR && npm start
     (or install the global CLI:  npm i -g . && botler)
EOF
