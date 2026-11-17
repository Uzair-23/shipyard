// k6: dashboard API latency through Caddy (PRD target: p95 < 200 ms).
// Env: API (http://shipyard.localhost: the Host and Origin the dashboard uses), BASE_URL (where Caddy
// listens, default http://127.0.0.1: k6 cannot resolve *.localhost), BENCH_EMAIL, BENCH_PASSWORD
import http from 'k6/http'
import { check } from 'k6'

const API = __ENV.API || 'http://shipyard.localhost'
const BASE = __ENV.BASE_URL || 'http://127.0.0.1'
const HOST = API.replace(/^https?:\/\//, '')

export const options = {
  scenarios: { api: { executor: 'constant-arrival-rate', rate: 50, timeUnit: '1s', duration: '15s', preAllocatedVUs: 10, maxVUs: 50 } },
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
}

export function setup() {
  const res = http.post(
    `${BASE}/api/v1/auth/login`,
    JSON.stringify({ email: __ENV.BENCH_EMAIL, password: __ENV.BENCH_PASSWORD }),
    { headers: { 'Content-Type': 'application/json', Origin: API, Host: HOST } },
  )
  if (res.status !== 200) throw new Error(`login failed: ${res.status}`)
  return { cookie: Object.values(res.cookies).map((c) => `${c[0].name}=${c[0].value}`).join('; ') }
}

export default function (d) {
  const headers = { Cookie: d.cookie, Origin: API, Host: HOST }
  const r = http.batch([
    ['GET', `${BASE}/api/v1/me`, null, { headers, tags: { name: 'me' } }],
    ['GET', `${BASE}/api/v1/apps`, null, { headers, tags: { name: 'apps' } }],
  ])
  check(r[0], { 'me 200': (x) => x.status === 200 })
  check(r[1], { 'apps 200': (x) => x.status === 200 })
}

export function handleSummary(data) {
  const v = data.metrics.http_req_duration.values
  const f = (x) => x.toFixed(2)
  const out = [
    '### Dashboard API (GET /me + GET /apps via Caddy, 50 iterations/s, ms)',
    `p50 ${f(v.med)} · p95 **${f(v['p(95)'])}** · p99 ${f(v['p(99)'])} · max ${f(v.max)}`,
    `failed requests: ${(data.metrics.http_req_failed.values.rate * 100).toFixed(2)} %`,
    '',
  ].join('\n')
  return { stdout: out }
}
