#!/bin/bash
# Browser end-to-end for the business subdomain pages. Starts a throwaway
# database, a fake model upstream and a mojojojo on a spare port whose business
# domain is mojojojo.test, then drives embed.mojojojo.test in Chromium.
set -euo pipefail
cd "$(dirname "$0")/.."
PORT=${PORT:-4397}; UP=${UP:-4396}
DB_NAME="mojojojo_bizbrowser_${PORT}_$$"
createdb "$DB_NAME"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; dropdb --if-exists "$DB_NAME" 2>/dev/null || true; }
trap cleanup EXIT

node -e '
require("http").createServer((q,r)=>{let b="";q.on("data",c=>b+=c);q.on("end",()=>{
  r.setHeader("Content-Type","application/json");r.setHeader("X-Mojo-Units","model.embed.tokens=4");
  r.end(JSON.stringify({object:"list",data:[{embedding:[0.1,0.2,0.3]}],usage:{total_tokens:4}}));});}).listen('"$UP"',"127.0.0.1");' &
PIDS+=($!)

go build -o mojojojo .
env PORT="$PORT" MOJOJOJO_DATABASE_URL="postgres:///$DB_NAME?host=/var/run/postgresql&sslmode=disable" \
  SITE_BASE_URL="http://mojojojo.test:$PORT" MOJOJOJO_UPSTREAM_EMBED="http://127.0.0.1:$UP" \
  STRIPE_SECRET_KEY=sk_test_fake STRIPE_PUBLISHABLE_KEY=pk_test_fake STRIPE_CREDITS_PRICE_ID=price_fake \
  ./mojojojo &
PIDS+=($!)
for _ in $(seq 1 40); do curl -sf -H "Host: embed.mojojojo.test:$PORT" "http://127.0.0.1:$PORT/health" >/dev/null && break; sleep 0.3; done
node tests/business_browser.mjs "http://embed.mojojojo.test:$PORT"
