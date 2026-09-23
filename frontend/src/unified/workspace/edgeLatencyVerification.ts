import { EdgeLatencyMeter, latencyCandidates, MAX_MEASURED_NODES, type LatencyTarget } from './edgeLatency'

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(`Edge latency verification failed: ${message}`)
}

const main: LatencyTarget = { id: 'main', region: 'main', endpoint: '' }
const sydney: LatencyTarget = { id: 'syd', region: 'ap-southeast-2', endpoint: 'https://syd.example.test' }
const sydneyTwo: LatencyTarget = { id: 'syd-2', region: 'ap-southeast-2', endpoint: 'https://syd2.example.test' }
const tokyo: LatencyTarget = { id: 'tyo', region: 'ap-northeast-1', endpoint: 'https://tyo.example.test' }
const nodes = [main, sydney, sydneyTwo, tokyo]
const ids = (targets: readonly LatencyTarget[]) => targets.map((target) => target.id).join(',')

// Only nodes the server could pick are measured.
assert(ids(latencyCandidates(nodes, 'auto', false)) === 'main,syd,syd-2,tyo', 'automatic sessions measure every node')
assert(ids(latencyCandidates(nodes, '', false)) === 'main,syd,syd-2,tyo', 'an unset region is automatic')
assert(ids(latencyCandidates(nodes, 'ap-southeast-2', false)) === 'syd,syd-2', 'a pinned region measures only its own nodes')
assert(latencyCandidates(nodes, 'main', false).length === 0, 'pinning the main site needs no measurement')
assert(ids(latencyCandidates(nodes, 'auto', true)) === 'syd,syd-2,tyo', 'a regional reconnect never measures the main site')
const many = Array.from({ length: 20 }, (_, index) => ({ id: `n${index}`, region: 'r', endpoint: `https://n${index}.example.test` }))
assert(latencyCandidates(many, 'auto', false).length === MAX_MEASURED_NODES, 'measurements stay capped')

// The warm-up request is not timed, so a cold handshake does not count.
let wall = 0
let clock = 0
const calls: string[] = []
const callCount = () => calls.length
const coldCost: Record<string, number> = { main: 5, syd: 400, 'syd-2': 400, tyo: 400 }
const warmCost: Record<string, number> = { main: 90, syd: 40, 'syd-2': 60, tyo: 150 }
const seen = new Set<string>()
const meter = new EdgeLatencyMeter(async (target) => {
  calls.push(target.id)
  clock += seen.has(target.id) ? warmCost[target.id] : coldCost[target.id]
  seen.add(target.id)
  return true
}, { now: () => wall, clock: () => clock })
// Measured one at a time so the shared fake clock only advances for one node.
const first = { ...await meter.latencies([main], 1_000), ...await meter.latencies([sydney], 1_000) }
assert(first.main === 90 && first.syd === 40, `only the second request is timed, got ${JSON.stringify(first)}`)
assert(first.syd < first.main, 'a nearer regional node is no longer penalised for its cold handshake')
assert(callCount() === 4, 'each node is probed twice: warm-up and timed')

// Cached results are reused without new requests until they expire.
wall += 60_000
const cached = await meter.latencies([main, sydney], 1_000)
assert(JSON.stringify(cached) === JSON.stringify(first) && callCount() === 4, 'fresh results are reused')
wall += 5 * 60_000
await meter.latencies([sydney], 1_000)
assert(callCount() === 6, 'expired results are measured again')

// A dead node neither blocks the session nor gets probed on every start.
let deadCalls = 0
const dead: LatencyTarget = { id: 'dead', region: 'x', endpoint: 'https://dead.example.test' }
const hanging = new EdgeLatencyMeter(async (target) => {
  if (target.id === 'dead') {
    deadCalls++
    return new Promise<boolean>(() => { /* never answers */ })
  }
  return true
}, { timeoutMs: 80, failureCacheMs: 30_000 })
const started = Date.now()
const partial = await hanging.latencies([sydney, dead], 40)
const waited = Date.now() - started
assert('syd' in partial && !('dead' in partial), 'answered nodes are reported and the dead one is left out')
assert(waited < 200, `the session waits only for its budget, waited ${waited} ms`)
await new Promise((resolve) => setTimeout(resolve, 120))
await hanging.latencies([dead], 40)
assert(deadCalls === 1, 'a node that timed out is skipped for a while instead of re-probed on every start')

// A failed probe is not reported as a latency.
const refused = new EdgeLatencyMeter(async () => false)
const none = await refused.latencies([tokyo], 100)
assert(Object.keys(none).length === 0, 'refused probes are left out so the server scores them as unmeasured')

// Warming shares its in-flight measurement with the session that follows.
let warmCalls = 0
const warming = new EdgeLatencyMeter(async () => {
  warmCalls++
  await new Promise((resolve) => setTimeout(resolve, 20))
  return true
})
warming.warm([sydney])
const afterWarm = await warming.latencies([sydney], 1_000)
assert('syd' in afterWarm && warmCalls === 2, 'a session reuses the warm-up already running instead of probing again')

console.log(JSON.stringify({
  edgeLatencyCandidates: true,
  warmUpExcludedFromTiming: true,
  cacheReuse: true,
  deadNodeBounded: waited,
  status: 'ok',
}))
