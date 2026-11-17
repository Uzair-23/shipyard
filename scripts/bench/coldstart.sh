#!/usr/bin/env bash
# Cold start: how long a request to a sleeping app takes until the first byte.
#
# For every round: make the app look idle, put it to sleep with a sleep job (the
# same job the idle reaper enqueues, without waiting for its 30 s tick), then
# time one request through the router. Also reads the router's own measurement
# (apps.last_cold_start_ms: job enqueued -> route running).
#
# Usage: coldstart.sh <slug>      Env: ROUTER_ADDR, ROUNDS (20), PG_CONTAINER
set -euo pipefail

slug=${1:?usage: coldstart.sh <slug>}
router=${ROUTER_ADDR:-127.0.0.1:8081}
rounds=${ROUNDS:-20}
pg() { docker exec "${PG_CONTAINER:-shipyard-postgres-1}" psql -U shipyard -d shipyard -tAq -c "$1"; }

# percentile <q> <numbers...>  (nearest rank)
percentile() {
  local q=$1; shift
  printf '%s\n' "$@" | sort -n | awk -v q="$q" '{a[NR]=$1} END {i=int(q*NR/100); if (i<q*NR/100) i++; if (i<1) i=1; print a[i]}'
}

client=(); router_side=()
for i in $(seq 1 "$rounds"); do
  ok=0
  for attempt in 1 2; do
    # The router flushes last_request_at every 10 s; wait it out so it cannot undo the backdating.
    sleep 11
    pg "UPDATE apps SET last_request_at = now() - interval '2 hours' WHERE slug = '$slug';
        UPDATE instances SET created_at = now() - interval '2 hours'
          WHERE state = 'running' AND deploy_id = (SELECT current_deploy_id FROM apps WHERE slug = '$slug');
        INSERT INTO jobs (kind, app_id, payload, max_attempts)
          SELECT 'sleep', id, jsonb_build_object('app_id', id::text), 3 FROM apps WHERE slug = '$slug';
        SELECT pg_notify('jobs_new', '');" >/dev/null
    status=""
    for _ in $(seq 1 60); do
      status=$(pg "SELECT status FROM apps WHERE slug = '$slug'")
      [ "$status" = sleeping ] && break
      sleep 0.5
    done
    [ "$status" = sleeping ] || { echo "round $i: app did not fall asleep (status=$status), retrying" >&2; continue; }

    out=$(curl -s -m 40 -o /dev/null -w '%{http_code} %{time_starttransfer}' -H "Host: $slug.apps.localhost" "http://$router/") || true
    code=${out%% *}; ttfb=${out##* }
    if [ "$code" != 200 ]; then echo "round $i: got HTTP $code, retrying" >&2; continue; fi
    ms=$(awk -v t="$ttfb" 'BEGIN {printf "%d", t*1000 + 0.5}')
    rs=$(pg "SELECT coalesce(last_cold_start_ms, 0) FROM apps WHERE slug = '$slug'")
    client+=("$ms"); router_side+=("$rs")
    echo "round $i: first byte after $ms ms (router measured $rs ms)" >&2
    ok=1; break
  done
  [ "$ok" = 1 ] || echo "round $i: FAILED" >&2
done

n=${#client[@]}
[ "$n" -gt 0 ] || { echo "no successful rounds" >&2; exit 1; }
sorted() { printf '%s\n' "$@" | sort -n; }
echo "### Cold start (sleeping app -> first byte, ms, n=$n of $rounds)"
echo "| measured by | p50 | p95 | min | max |"
echo "|---|---|---|---|---|"
echo "| client (curl time to first byte) | $(percentile 50 "${client[@]}") | $(percentile 95 "${client[@]}") | $(sorted "${client[@]}" | head -n1) | $(sorted "${client[@]}" | tail -n1) |"
echo "| router (enqueue -> route running) | $(percentile 50 "${router_side[@]}") | $(percentile 95 "${router_side[@]}") | $(sorted "${router_side[@]}" | head -n1) | $(sorted "${router_side[@]}" | tail -n1) |"
echo
