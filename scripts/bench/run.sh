#!/usr/bin/env bash
# Benchmarks for Shipyard (PRD 7.1). Run `make bench`.
#
# Needs the stack running: docker compose up -d, make worker, make router
# (or any router address via ROUTER_ADDR). It creates a throwaway user and a
# public static app, measures, and deletes the app again.
#
#   cold start        n=20 sleeping -> first byte         (scripts/bench/coldstart.sh)
#   proxy overhead    direct vs router at a fixed rate    (scripts/bench/proxy.js, k6)
#   throughput        50 virtual users, 15 s              (scripts/bench/proxy.js, k6)
#   dashboard API     GET /me + /apps through Caddy       (scripts/bench/api.js, k6)
#
# For the throughput number comparable to "1 vCPU", start the router with
# GOMAXPROCS=1. k6 runs on the same machine, so the numbers are conservative.
set -euo pipefail
cd "$(dirname "$0")/../.."

API=${API:-http://shipyard.localhost}
export ROUTER_ADDR=${ROUTER_ADDR:-127.0.0.1:8081}
PG_CONTAINER=${PG_CONTAINER:-shipyard-postgres-1}
export PG_CONTAINER
EMAIL=${BENCH_EMAIL:-bench@example.com}
PASSWORD=${BENCH_PASSWORD:-bench-password-123456}
slug="bench-$RANDOM"
tmp=$(mktemp -d)
jar="$tmp/jar"

for tool in curl k6 docker tar; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done
pg() { docker exec "$PG_CONTAINER" psql -U shipyard -d shipyard -tAq -c "$1"; }
api() { curl -s -m 60 -H "Origin: $API" -b "$jar" -c "$jar" "$@"; }
json=(-H 'Content-Type: application/json')

cleanup() {
  api -X DELETE "$API/api/v1/apps/$slug" -o /dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

echo "== setup: user $EMAIL, app $slug" >&2
creds=$(printf '{"email":"%s","password":"%s"}' "$EMAIL" "$PASSWORD")
api "${json[@]}" -d "$creds" "$API/api/v1/auth/signup" -o /dev/null || true # exists already: fine
code=$(api "${json[@]}" -d "$creds" "$API/api/v1/auth/login" -o /dev/null -w '%{http_code}')
[ "$code" = 200 ] || { echo "login failed (HTTP $code); is the stack up?" >&2; exit 1; }
api "${json[@]}" -d "{\"slug\":\"$slug\"}" "$API/api/v1/apps" -o /dev/null
api -X PATCH "${json[@]}" -d '{"visibility":"public"}' "$API/api/v1/apps/$slug" -o /dev/null
tar -czf "$tmp/app.tar.gz" -C testdata/apps/static-site .
api -F "source=@$tmp/app.tar.gz" "$API/api/v1/apps/$slug/deploys" -o /dev/null

for _ in $(seq 1 90); do
  state=$(api "$API/api/v1/apps/$slug" | grep -o '"status":"[a-z]*"' | head -n1 || true)
  [ "$state" = '"status":"running"' ] && break
  sleep 2
done
[ "$state" = '"status":"running"' ] || { echo "app did not become live in time" >&2; exit 1; }
port=$(pg "SELECT i.host_port FROM instances i JOIN apps a ON a.current_deploy_id = i.deploy_id WHERE a.slug = '$slug' AND i.state = 'running'")
echo "== app is live on 127.0.0.1:$port, router at $ROUTER_ADDR" >&2

echo "== proxy overhead + throughput (about 70 s)" >&2
ROUTER_URL="http://$ROUTER_ADDR" DIRECT_URL="http://127.0.0.1:$port" HOST="$slug.apps.localhost" \
  k6 run --quiet scripts/bench/proxy.js 2>"$tmp/k6.err" || { cat "$tmp/k6.err" >&2; exit 1; }

echo "== dashboard API (about 15 s)" >&2
API="$API" BENCH_EMAIL="$EMAIL" BENCH_PASSWORD="$PASSWORD" k6 run --quiet scripts/bench/api.js 2>"$tmp/k6.err" || { cat "$tmp/k6.err" >&2; exit 1; }

echo "== cold start (${ROUNDS:-20} rounds, about 20 s each)" >&2
scripts/bench/coldstart.sh "$slug"
