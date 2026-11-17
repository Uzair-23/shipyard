// k6: proxy overhead and throughput of the router against one warm app.
//
//   direct_*  : straight to the app's loopback port
//   router_*  : through the router (Host header picks the app)
//   *_rate    : a fixed, modest request rate, so latency is not queueing delay
//   *_max     : as many requests as 50 virtual users can make (throughput)
//
// Env: ROUTER_URL (http://127.0.0.1:8081), HOST (<slug>.apps.localhost), DIRECT_URL (http://127.0.0.1:<port>)
import http from 'k6/http'
import { check } from 'k6'
import { Trend, Rate } from 'k6/metrics'

const ROUTER_URL = __ENV.ROUTER_URL
const DIRECT_URL = __ENV.DIRECT_URL
const HOST = __ENV.HOST
const RATE = Number(__ENV.RATE || 200)

const names = ['direct_rate', 'router_rate', 'direct_max', 'router_max']
const lat = Object.fromEntries(names.map((n) => [n, new Trend('lat_' + n, true)]))
const failed = new Rate('failed_requests')

const rateScenario = (exec, startTime) => ({
  executor: 'constant-arrival-rate', exec, startTime, rate: RATE, timeUnit: '1s',
  duration: '15s', preAllocatedVUs: 20, maxVUs: 100,
})
const maxScenario = (exec, startTime) => ({
  executor: 'constant-vus', exec, startTime, vus: 50, duration: '15s',
})

export const options = {
  scenarios: {
    direct_rate: rateScenario('directRate', '0s'),
    router_rate: rateScenario('routerRate', '17s'),
    direct_max: maxScenario('directMax', '34s'),
    router_max: maxScenario('routerMax', '51s'),
  },
  summaryTrendStats: ['min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
}

function hit(name, url, headers) {
  const res = http.get(url, { headers })
  const ok = check(res, { 'status 200': (r) => r.status === 200 })
  failed.add(!ok)
  lat[name].add(res.timings.duration)
}
export function directRate() { hit('direct_rate', DIRECT_URL + '/', {}) }
export function routerRate() { hit('router_rate', ROUTER_URL + '/', { Host: HOST }) }
export function directMax() { hit('direct_max', DIRECT_URL + '/', {}) }
export function routerMax() { hit('router_max', ROUTER_URL + '/', { Host: HOST }) }

const f = (v) => (v === undefined ? 'n/a' : v.toFixed(2))

export function handleSummary(data) {
  const m = (n) => data.metrics['lat_' + n].values
  const row = (label, n) => `| ${label} @ ${RATE} req/s | ${f(m(n).med)} | ${f(m(n)['p(95)'])} | ${f(m(n)['p(99)'])} |`
  const overheadP50 = m('router_rate').med - m('direct_rate').med
  const overheadP95 = m('router_rate')['p(95)'] - m('direct_rate')['p(95)']
  const rps = (n) => (m(n).count / 15).toFixed(0)
  const out = [
    '### Proxy overhead (warm app, ms)',
    '| path | p50 | p95 | p99 |',
    '|---|---|---|---|',
    row('direct to app', 'direct_rate'),
    row('through router', 'router_rate'),
    '',
    `Router overhead: **p50 ${f(overheadP50)} ms, p95 ${f(overheadP95)} ms**`,
    '',
    '### Throughput (50 virtual users, 15 s, req/s)',
    `direct to app: **${rps('direct_max')}** · through router: **${rps('router_max')}**`,
    `failed requests: ${(data.metrics.failed_requests.values.rate * 100).toFixed(2)} %`,
    '',
  ].join('\n')
  return { stdout: out }
}
