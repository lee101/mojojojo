#!/bin/bash
# End-to-end: a real signed-in user, a real API key, real credits moving.
#
# Runs against a throwaway mojojojo PostgreSQL database and starts its own web
# tier on a spare port. The runner has to be up; everything else is built here.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${PORT:-4399}
DIR=$(mktemp -d)
DB_NAME="mojojojo_e2e_${PORT}_$$"
createdb "$DB_NAME"
WEB_PID=
cleanup() {
  [ -z "$WEB_PID" ] || kill "$WEB_PID" 2>/dev/null || true
  dropdb --if-exists "$DB_NAME" 2>/dev/null || true
  rm -rf "$DIR"
}
trap cleanup EXIT

go build -o mojojojo .
env PORT="$PORT" MOJOJOJO_DATABASE_URL="postgres:///$DB_NAME?host=/var/run/postgresql&sslmode=disable" \
  MS_PER_CREDIT=10 SITE_BASE_URL="http://127.0.0.1:$PORT" ./mojojojo &
WEB_PID=$!
for _ in $(seq 1 30); do curl -sf "http://127.0.0.1:$PORT/" >/dev/null && break; sleep 0.3; done

fail() { echo "FAIL: $1"; exit 1; }

echo "0a. local signup creates the account and host session"
JAR="$DIR/cookies"
FORM=$(curl -s -c "$JAR" "http://127.0.0.1:$PORT/auth/signup?next=/keys")
CSRF=$(echo "$FORM" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -1)
[ -n "$CSRF" ] || fail "signup form has no CSRF token"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -c "$JAR" \
  -X POST "http://127.0.0.1:$PORT/auth/signup" \
  --data-urlencode "csrf=$CSRF" --data-urlencode "next=/keys" \
  --data-urlencode "email=e2e@example.com" --data-urlencode "password=correct horse battery staple")
[ "$CODE" = "302" ] || fail "local signup got $CODE"
TOKEN=$(awk '$6 == "mojojojo_session" {print $7}' "$JAR" | tail -1)
[ -n "$TOKEN" ] || fail "local signup did not set a session"

echo "0. anonymous server runs are refused, browser runs are not our business"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/run" \
  -H 'Content-Type: application/json' -d '{"code":"print(1)"}')
[ "$CODE" = "401" ] || fail "anonymous run got $CODE, want 401"

echo "1. the session is recognised"
curl -s "http://127.0.0.1:$PORT/v1/me" -H "Cookie: mojojojo_session=$TOKEN" |
  grep -q '"signed_in":true' || fail "session not recognised"

echo "2. a key can be minted with the session"
KEY=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/keys" -H "Cookie: mojojojo_session=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"e2e"}' |
  python3 -c "import json,sys; print(json.load(sys.stdin).get('key',''))")
[[ "$KEY" == sk-mj-* ]] || fail "no key issued"

echo "2b. a signed branded asset upload writes, records, and deletes cleanly"
ASSET_BODY='mojojojo-e2e-asset'
SIGN=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/uploads/sign" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d "{\"filename\":\"e2e.txt\",\"content_type\":\"text/plain\",\"size_bytes\":${#ASSET_BODY}}")
STORAGE_ENABLED=1
if grep -q 'not configured' <<<"$SIGN"; then
  STORAGE_ENABLED=0
  echo "   object storage unavailable, skipped"
else
  UPLOAD_URL=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("upload_url", ""))' <<<"$SIGN")
  ASSET_URL=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("asset_url", ""))' <<<"$SIGN")
  [[ "$UPLOAD_URL" == "http://127.0.0.1:$PORT/v1/uploads/"* ]] || fail "upload URL is not branded: $SIGN"
  [[ "$ASSET_URL" == https://mojojojostatic.mojojojo.com/* ]] || fail "asset URL is not branded: $SIGN"
  UPLOADED=$(printf %s "$ASSET_BODY" | curl -s -X PUT "$UPLOAD_URL" \
    -H 'Content-Type: text/plain' --data-binary @-)
  ASSET_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("id", ""))' <<<"$UPLOADED")
  [ -n "$ASSET_ID" ] || fail "signed upload failed: $UPLOADED"
  curl -s -X DELETE "http://127.0.0.1:$PORT/v1/assets/$ASSET_ID" \
    -H "Authorization: Bearer $KEY" | grep -q '"ok":true' || fail "asset cleanup failed"
fi

echo "2c. a ZIP becomes an immutable project deployment"
if [ "$STORAGE_ENABLED" = 0 ] || ! command -v zip >/dev/null; then
  echo "   object storage or zip unavailable, skipped"
else
  PROJECT=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/projects" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"name":"E2E Fractal","runtime":"static"}')
  PROJECT_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("project", {}).get("id", ""))' <<<"$PROJECT")
  [ -n "$PROJECT_ID" ] || fail "project creation failed: $PROJECT"
  (cd examples/mojojojo-fractal && zip -q -r "$DIR/fractal.zip" mojojojo.json index.html app.js src)
  DEPLOYED=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/deployments" \
    -H "Authorization: Bearer $KEY" -H "X-Project-ID: $PROJECT_ID" \
    -H 'Content-Type: application/zip' --data-binary @"$DIR/fractal.zip")
  DEPLOY_URL=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("deployment", {}).get("url", ""))' <<<"$DEPLOYED")
  [[ "$DEPLOY_URL" == "http://127.0.0.1:$PORT/p/$PROJECT_ID/" ]] || fail "deployment failed: $DEPLOYED"
  curl -sf "$DEPLOY_URL" | grep -qi 'fractal' || fail "stable deployment gateway did not serve the app"
  curl -s -X DELETE "http://127.0.0.1:$PORT/v1/projects/$PROJECT_ID" \
    -H "Authorization: Bearer $KEY" | grep -q '"ok":true' || fail "project cleanup failed"
fi

echo "2d. a vector project embeds, retrieves, meters, and deletes"
VECTOR_PROJECT=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/vector/projects" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"E2E Retrieval","tier":"exact"}')
VECTOR_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("project", {}).get("id", ""))' <<<"$VECTOR_PROJECT")
[ -n "$VECTOR_ID" ] || fail "vector project creation failed: $VECTOR_PROJECT"
VECTOR_UPSERT=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/vector/projects/$VECTOR_ID/upsert" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"documents":[{"id":"mojo","text":"Mojo GPU kernels and systems programming","metadata":{"lang":"mojo"}},{"id":"garden","text":"Growing tomatoes and herbs in a sunny garden","metadata":{"lang":"nature"}}]}')
grep -q '"vector_count":2' <<<"$VECTOR_UPSERT" || fail "vector upsert failed: $VECTOR_UPSERT"
VECTOR_SEARCH=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/vector/projects/$VECTOR_ID/search" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"query":"fast GPU programming","limit":2}')
python3 - "$VECTOR_SEARCH" <<'PY'
import json, sys
r = json.loads(sys.argv[1])
assert r["matches"][0]["id"] == "mojo", r
assert r["backend"] == "mojo-int8-exact", r
assert r["latency_ms"] > 0, r
assert r["ran_on"] == "local", r
print(f"   {r['backend']} returned {r['matches'][0]['id']} in {r['latency_ms']:.3f}ms")
PY
curl -s "http://127.0.0.1:$PORT/v1/vector/projects/$VECTOR_ID/usage" \
  -H "Authorization: Bearer $KEY" | grep -q '"operation":"search"' || fail "vector usage missing"
curl -s -X DELETE "http://127.0.0.1:$PORT/v1/vector/projects/$VECTOR_ID" \
  -H "Authorization: Bearer $KEY" | grep -q '"ok":true' || fail "vector cleanup failed"

echo "2e. managed JSON data CRUD works without another database process"
DATA_DB=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/data/namespaces" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"E2E customers"}')
DATA_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("database", {}).get("id", ""))' <<<"$DATA_DB")
[ -n "$DATA_ID" ] || fail "managed data database creation failed: $DATA_DB"
curl -s -X PUT "http://127.0.0.1:$PORT/v1/data/namespaces/$DATA_ID/records" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"key":"customer:123","value":{"name":"Ada","plan":"starter"}}' \
  | grep -q '"status":"not_linked"' || fail "managed data upsert failed"
curl -s "http://127.0.0.1:$PORT/v1/data/namespaces/$DATA_ID/records" \
  -H "Authorization: Bearer $KEY" | grep -q '"customer:123"' || fail "managed data list failed"
curl -s -X DELETE "http://127.0.0.1:$PORT/v1/data/namespaces/$DATA_ID/records" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"key":"customer:123"}' | grep -q '"ok":true' || fail "managed data delete failed"

echo "2f. Studio publishes the default game and two browsers connect peer-to-peer"
if [ "$STORAGE_ENABLED" = 0 ]; then
  echo "   object storage unavailable, skipped"
else
  node tests/multiplayer.mjs "http://127.0.0.1:$PORT" "mojojojo_session=$TOKEN"
fi

echo "3. the key runs code, and the account has its one welcome dollar"
# Earlier API probes may themselves leave a fractional compute accrual. Capture
# it here so the sub-credit assertion below checks the actual carry arithmetic
# instead of assuming this run is the first metered operation on the account.
OWED_BEFORE_OUT=$(psql -d "$DB_NAME" -Atqc "SELECT COALESCE((SELECT owed_ms FROM accrual WHERE user_id=(SELECT id FROM users WHERE email='e2e@example.com')), 0)")
OUT=$(curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"code":"print(sum(range(100)))","timeout_ms":10000}')
echo "$OUT" | grep -q '"stdout":"4950' || fail "wrong output: $OUT"
GRANTED=$(psql -d "$DB_NAME" -Atqc "SELECT COUNT(*) FROM credit_ledger WHERE reason='mojojojo:welcome'")
[ "$GRANTED" = "1" ] || fail "welcome grant not in the ledger"

echo "3b. the welcome grant happens exactly once"
curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"code":"print(1)"}' >/dev/null
AGAIN=$(psql -d "$DB_NAME" -Atqc "SELECT COUNT(*) FROM credit_ledger WHERE reason='mojojojo:welcome'")
[ "$AGAIN" = "1" ] || fail "granted the welcome credits twice"

echo "3c. imports are installed without being asked for"
AUTO=$(curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"code":"import numpy as np\nprint(int(np.arange(4).sum()))","timeout_ms":30000}')
echo "$AUTO" | grep -q '"stdout":"6' || fail "auto-install run failed: $AUTO"

echo "4. milliseconds became credits in the mojojojo ledger"
# A program that does enough work to cross a credit. `print(sum(range(100)))`
# used to, which was never a fact about the program: it was our own cold start
# leaking into the billed window. A run that costs a millisecond now bills a
# millisecond, so a run that should be charged has to actually spend the time.
WORK=$(curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"code":"import time\nt=time.perf_counter()\nwhile time.perf_counter()-t < 0.05: pass\nprint(\"worked\")","timeout_ms":10000}')
python3 - "$WORK" "$OUT" "$OWED_BEFORE_OUT" <<'PY'
import json, sys
r = json.loads(sys.argv[1])
c = r["charge"]
assert c["billed_ms"] >= 50, r
assert c["credits"] >= 5, f"a {c['billed_ms']:.0f}ms run at 10ms/credit should charge: {c}"
print(f"   billed {c['billed_ms']:.0f}ms -> {c['credits']} credit(s), balance {c['balance']}")

# And the trivial run before it: below one credit, combined with precisely the
# carry that existed immediately before it. Charging a whole credit merely
# because a request ended is the thing per-millisecond billing exists not to do.
first = json.loads(sys.argv[2])["charge"]
owed_before = float(sys.argv[3])
assert 0 < first["billed_ms"] < 10, first
total = owed_before + first["billed_ms"]
expected_credits = int(total // 10)
expected_owed = total - expected_credits * 10
assert first["credits"] == expected_credits, (owed_before, first)
assert abs(first["owed_ms"] - expected_owed) < 0.01, (owed_before, first)
print(f"   and {first['billed_ms']:.1f}ms preserved the fractional carry exactly")
PY
psql -d "$DB_NAME" -Atqc "SELECT delta, reason, app_id FROM credit_ledger" | grep -q "exec:cpu" ||
  fail "no traceable ledger line"

echo "4b. a Mojo program compiles on the server and runs with no interpreter"
MOJO=$(curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"language":"mojo","code":"def main():\n    var t: Int = 0\n    for i in range(1000):\n        t += i\n    print(t)\n","timeout_ms":120000}')
echo "$MOJO" | grep -q '"stdout":"499500' || fail "mojo run failed: $MOJO"
python3 - "$MOJO" <<'PY'
import json, sys
r = json.loads(sys.argv[1])
assert r["language"] == "mojo", r
# The build is ours. Whether it happened just now or was already cached, the
# caller is charged for the running and nothing else.
assert r["charge"]["billed_ms"] <= r["wall_ms"], r
assert r["charge"]["billed_ms"] < 5000, r
print(f"   mojo ran in {r['charge']['billed_ms']:.0f}ms, build {r.get('build_ms', 0):.0f}ms (free)")
PY
BAD=$(curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"language":"mojo","code":"def main():\n    var x: Int = \"nope\"\n","timeout_ms":120000}')
echo "$BAD" | grep -q '"ok":false' || fail "a broken Mojo program was not reported: $BAD"
echo "$BAD" | grep -q '"billed_ms":0' || fail "a program that did not compile was billed: $BAD"
curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/run" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"language":"ruby","code":"puts 1"}' | grep -q 400 ||
  fail "an unknown language was not refused"

echo "4c. the plan endpoint quotes a program before it runs"
PLAN=$(curl -s "http://127.0.0.1:$PORT/v1/plan" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"code":"import time\nt=time.perf_counter()\nwhile time.perf_counter()-t < 0.05: pass\nprint(\"worked\")"}')
python3 - "$PLAN" <<'PY'
import json, sys
p = json.loads(sys.argv[1])
# That program has run once already, so the estimate is a measurement.
assert p["estimate"], p
assert p["estimate"]["ms"] >= 50, p
assert p["suggestion"]["where"] in ("home", "edge"), p
assert p["rates"]["ms_per_credit"] > 0, p
print(f"   estimated {p['estimate']['ms']:.0f}ms from {p['estimate']['seen']} run(s)"
      f" -> {p['suggestion']['where']}, {p['suggestion']['credits']} credit(s)")
PY
curl -s "http://127.0.0.1:$PORT/v1/fleet" | grep -q '"home"' || fail "no fleet view"

echo "5. an unknown key is refused, not quietly downgraded to the free tier"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/run" \
  -H "Authorization: Bearer mj_live_not_a_real_key" -H 'Content-Type: application/json' \
  -d '{"code":"print(1)"}')
[ "$CODE" = "401" ] || fail "an invalid key got $CODE, want 401"

echo "6. a revoked key stops working"
ID=$(psql -d "$DB_NAME" -Atqc "SELECT id FROM api_keys LIMIT 1")
curl -s -X POST "http://127.0.0.1:$PORT/v1/keys/revoke" -H "Cookie: mojojojo_session=$TOKEN" \
  -H 'Content-Type: application/json' -d "{\"id\":\"$ID\"}" >/dev/null
BAL_BEFORE=$(psql -d "$DB_NAME" -Atqc "SELECT free_credits FROM users WHERE email='e2e@example.com'")
REVOKED=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v1/run" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"code":"print(1)"}')
[ "$REVOKED" = "401" ] || fail "a revoked key got $REVOKED, want 401"
BAL_AFTER=$(psql -d "$DB_NAME" -Atqc "SELECT free_credits FROM users WHERE email='e2e@example.com'")
[ "$BAL_BEFORE" = "$BAL_AFTER" ] || fail "a revoked key still spent credits"

echo "7. every page renders — signed out, and signed in"
PAGES="/ /editor /studio /apps /tools /pricing /docs /keys /usage /credits /billing /cloud /data /vector /examples /agent /research"
for p in $PAGES; do
  BODY=$(curl -s "http://127.0.0.1:$PORT$p")
  CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$p")
  [ "$CODE" = "200" ] || fail "$p returned $CODE signed out"
  case "$BODY" in
    *"template error"*) fail "$p failed to render signed out" ;;
    *"<no value>"*) fail "$p rendered a <no value> signed out" ;;
  esac
  # A stranger is always one click from an account, and from paying.
  grep -q '/auth/signup' <<<"$BODY" || fail "$p has no way to sign up"
  grep -q 'href="/credits"' <<<"$BODY" || fail "$p has no link to credits"

  IN=$(curl -s "http://127.0.0.1:$PORT$p" -H "Cookie: mojojojo_session=$TOKEN")
  case "$IN" in
    *"template error"*) fail "$p failed to render signed in" ;;
    *"<no value>"*) fail "$p rendered a <no value> signed in" ;;
  esac
  grep -q 'href="/credits"' <<<"$IN" || fail "$p has no way to manage credits when signed in"
  grep -q 'class="pill" href="/credits"' <<<"$IN" || fail "$p does not show the balance"
done

echo "8. the credits page is a statement of account, not a marketing page"
CRED=$(curl -s "http://127.0.0.1:$PORT/credits" -H "Cookie: mojojojo_session=$TOKEN")
grep -q 'Ledger' <<<"$CRED" || fail "no ledger on the credits page"
grep -q 'exec:cpu' <<<"$CRED" || fail "the credits page does not show what was charged"
grep -q 'mojojojo:welcome' <<<"$CRED" || fail "the welcome grant is not visible in the ledger"
grep -q 'class="meter"' <<<"$CRED" || fail "no accrual meter"
grep -q 'Manage mojojojo credits' <<<"$CRED" || fail "no credit management for a signed-in user"

echo "8b. /v1/credits says the same thing to a machine"
JSON=$(curl -s "http://127.0.0.1:$PORT/v1/credits" -H "Cookie: mojojojo_session=$TOKEN")
python3 - "$JSON" <<'PY'
import json, sys
c = json.loads(sys.argv[1])
assert c["signed_in"] is True, c
assert c["welcome_granted"] is True, c
assert c["granted_credits"] == 1000, c
assert c["spent_credits"] >= 1, c
assert c["credits"] == c["balance"]["free"] + c["balance"]["plan"] + c["balance"]["paid"], c
assert c["buys"]["cpu_ms"] == c["credits"] * c["ms_per_credit"], c
assert any(e["app"] == "mojojojo" and e["delta"] < 0 for e in c["ledger"]), c["ledger"]
assert c["top_up"].startswith("http"), c
print(f"   {c['credits']} credits, {len(c['ledger'])} ledger lines, "
      f"{c['spent_credits']} spent here")
PY
# Signed out it is a sign-up page, and it never leaks a balance.
curl -s "http://127.0.0.1:$PORT/v1/credits" | grep -q '"signed_in":false' ||
  fail "/v1/credits answered an anonymous caller with an account"

echo "9. the highlighter is served and the pages ask for it"
curl -sf "http://127.0.0.1:$PORT/hl.js" | grep -q 'export function highlight' ||
  fail "/hl.js is not being served"
curl -s "http://127.0.0.1:$PORT/" | grep -q 'data-lang="bash"' ||
  fail "the home page code blocks are not tagged for highlighting"
curl -s "http://127.0.0.1:$PORT/" | grep -q 'id="mojocode"' ||
  fail "the home page lost its Mojo example"
curl -s "http://127.0.0.1:$PORT/docs" | grep 'data-lang="json"' >/dev/null ||
  fail "the docs code blocks are not tagged for highlighting"
curl -s "http://127.0.0.1:$PORT/" | grep -Eq 'import \{ highlightAll \} from "/hl.js\?v=[^"]+"' ||
  fail "no page ever loads the highlighter"

echo "9b. every research post the index links to actually renders"
LINKED=$(curl -s "http://127.0.0.1:$PORT/research" |
  grep -o 'href="/research/[a-z0-9-]*"' | sed 's/.*research\///;s/"//' | sort -u)
[ -n "$LINKED" ] || fail "the research index links to no posts"
for slug in $LINKED; do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/research/$slug")
  [ "$CODE" = "200" ] || fail "/research/$slug returned $CODE"
  BODY=$(curl -s "http://127.0.0.1:$PORT/research/$slug")
  case "$BODY" in *"template error"*) fail "/research/$slug failed to render" ;; esac
  grep -q 'data-lang=' <<<"$BODY" || fail "/research/$slug has no highlighted code"
done
echo "   $(echo "$LINKED" | wc -w) posts"
# An unknown slug is a 404 page, not a 500 and not somebody else's template.
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/research/nope")
[ "$CODE" = "404" ] || fail "an unknown post slug returned $CODE, want 404"
curl -s "http://127.0.0.1:$PORT/research/../docs" | grep -q 'template error' &&
  fail "a traversing slug reached a template"
curl -s "http://127.0.0.1:$PORT/sitemap.xml" | grep -q '/research/' ||
  fail "research posts are missing from the sitemap"
SITEMAP=$(curl -s "http://127.0.0.1:$PORT/sitemap.xml")
grep -q '/cloud</loc>' <<<"$SITEMAP" || fail "cloud is missing from the sitemap"
grep -q '/data</loc>' <<<"$SITEMAP" || fail "managed data is missing from the sitemap"
grep -q '/vector</loc>' <<<"$SITEMAP" || fail "vector search is missing from the sitemap"
grep -q '/examples</loc>' <<<"$SITEMAP" || fail "examples are missing from the sitemap"
CODE=$(curl -s -o /dev/null -w '%{http_code}' \
  "http://127.0.0.1:$PORT/examples/source/mojojojo-fractal/README.md")
[ "$CODE" = "200" ] || fail "example source returned $CODE"

echo "10. the same, in a real browser (skipped when playwright is absent)"
if command -v node >/dev/null; then
  node tests/browser.mjs "http://127.0.0.1:$PORT" --cookie "mojojojo_session=$TOKEN" ||
    fail "browser e2e failed for a signed-in user"
  node tests/browser.mjs "http://127.0.0.1:$PORT" || fail "browser e2e failed for a visitor"
else
  echo "   node unavailable, skipped"
fi

echo "11. a user with no credits is refused before running anything"
psql -d "$DB_NAME" -qc "UPDATE users SET free_credits=0, paid_credits=0, plan_credits=0 WHERE email='e2e@example.com'"
KEY2=$(curl -s -X POST "http://127.0.0.1:$PORT/v1/keys" -H "Cookie: mojojojo_session=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"e2e2"}' |
  python3 -c "import json,sys; print(json.load(sys.stdin).get('key',''))")
curl -s "http://127.0.0.1:$PORT/v1/run" -H "Authorization: Bearer $KEY2" \
  -H 'Content-Type: application/json' -d '{"code":"print(1)"}' |
  grep -q '402\|out of credits' || fail "a broke account was allowed to run"

echo
echo "e2e OK"
