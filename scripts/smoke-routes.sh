#!/bin/sh
# Public-origin route gate. Every documented page/installer must resolve, and
# protected APIs must fail as authentication errors rather than disappearing.
set -eu

base=${1:-https://mojojojo.cc}
base=${base%/}
temporary=$(mktemp -d "${TMPDIR:-/tmp}/mojojojo-routes.XXXXXX")
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

fail() { echo "route smoke: $*" >&2; exit 1; }

for path in \
  / /editor /playground /studio /ide /apps /tools /pricing /docs /keys \
  /usage /credits /billing /cloud /data /vector /examples /agent /research \
  /auth/login /auth/signup /visualbench/ /robots.txt /sitemap.xml \
  /mojojojo.js /mojojojo.min.js /hl.js /route.js; do
  code=$(curl -sS -L -o "$temporary/body" -w '%{http_code}' "$base$path")
  [ "$code" = 200 ] || fail "$path returned $code"
  [ -s "$temporary/body" ] || fail "$path returned an empty body"
done

curl -fsS "$base/install.sh" -o "$temporary/install.sh"
sh -n "$temporary/install.sh"
grep -q 'SHA256SUMS' "$temporary/install.sh" || fail "/install.sh is not the release installer"

curl -fsS "$base/install.ps1" -o "$temporary/install.ps1"
grep -q 'Get-FileHash' "$temporary/install.ps1" || fail "/install.ps1 is not the release installer"

for path in /health /v1/pricing /v1/me /v1/credits /v1/showcase; do
  code=$(curl -sS -o "$temporary/api" -w '%{http_code}' "$base$path")
  [ "$code" = 200 ] || fail "$path returned $code"
done

for spec in \
  'GET /v1/usage' 'GET /v1/keys' 'GET /v1/storage' \
  'GET /v1/projects' 'GET /v1/data/namespaces' 'GET /v1/vector/projects' \
  'GET /v1/vector/reservations' 'GET /v1/agent/sessions'; do
  method=${spec%% *}
  path=${spec#* }
  code=$(curl -sS -X "$method" -o "$temporary/protected" -w '%{http_code}' "$base$path")
  [ "$code" = 401 ] || fail "$method $path returned $code, want 401"
done

code=$(curl -sS -X POST -H 'Content-Type: application/json' -d '{}' \
  -o "$temporary/internal" -w '%{http_code}' "$base/internal/agent/settle")
[ "$code" = 401 ] || fail "internal settlement returned $code, want 401"

grep -q '<loc>.*\/agent</loc>' "$temporary/body" 2>/dev/null || \
  curl -fsS "$base/sitemap.xml" | grep -q '<loc>.*\/agent</loc>' || \
  fail "sitemap is missing /agent"

echo "route smoke: public pages, installers, APIs, and auth boundaries passed"
