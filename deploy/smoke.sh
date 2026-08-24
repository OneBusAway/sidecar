#!/bin/sh
# Smoke-test a running sidecar. Usage: deploy/smoke.sh [base-url]
# Exit 0 when /healthz and /admin both answer 200. The alerts feed is
# checked only if a region exists yet (it 404s on a fresh database).
set -eu
base="${1:-http://localhost:8080}"

# status prints the HTTP status for a path, or 000 when the connection
# fails. curl already writes '000' via -w before returning non-zero, so the
# fallback replaces the captured value rather than appending to it.
status() {
  code="$(curl -s -o /dev/null -w '%{http_code}' "$base$1")" || code=000
  echo "$code"
}

check() {
  path="$1"; want="$2"
  got="$(status "$path")"
  if [ "$got" != "$want" ]; then
    echo "FAIL $path -> $got (want $want)" >&2
    exit 1
  fi
  echo "ok   $path -> $got"
}

check /healthz 200
check /admin 200            # proves the SPA embedded (503 means it did not)
alerts="$(status /api/v1/regions/1/alerts.pbtext)"
case "$alerts" in
  200) echo "ok   /api/v1/regions/1/alerts.pbtext -> 200" ;;
  404) echo "skip /api/v1/regions/1/alerts.pbtext -> 404 (no regions synced yet)" ;;
  *)   echo "FAIL /api/v1/regions/1/alerts.pbtext -> $alerts" >&2; exit 1 ;;
esac
