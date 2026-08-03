#!/bin/bash
# Run the edge tier locally, under the same workerd Cloudflare runs.
#
#   bash wasmworker/dev.sh            # http://127.0.0.1:8787
#   EDGE_TOKEN=... bash wasmworker/dev.sh
#
# Deploying is deliberately not here: `npm run deploy` in this directory, once
# somebody has decided to.
set -euo pipefail
cd "$(dirname "$0")"
[ -d node_modules ] || npm install --no-fund --no-audit
if [ -n "${EDGE_TOKEN:-}" ]; then
    printf 'EDGE_TOKEN = "%s"\n' "$EDGE_TOKEN" > .dev.vars
fi
exec ./node_modules/.bin/wrangler dev --port "${PORT:-8787}" --local
