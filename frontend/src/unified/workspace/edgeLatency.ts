/**
 * Round-trip measurements that `/api/edges/authorize` uses to rank nodes.
 *
 * Only nodes the server could actually pick are measured, each one gets a
 * warm-up request first so a fresh DNS/TLS handshake does not make a regional
 * node look slower than the already-connected main site, results are reused
 * for a few minutes, and a session never waits longer than its budget for a
 * slow or dead node: unanswered nodes are simply left out, which the server
 * scores as 1000 ms.
 */

export interface LatencyTarget {
  id: string
  region: string
  endpoint: string
}

/** Resolves true when the node answered its probe successfully. */
export type ProbeFn = (target: LatencyTarget, signal: AbortSignal) => Promise<boolean>

export interface LatencyMeterOptions {
  /** How long a successful measurement is reused. */
  cacheMs?: number
  /** How long a failed node is skipped before it is probed again. */
  failureCacheMs?: number
  /** Hard stop for one node's warm-up plus timed request. */
  timeoutMs?: number
  /** Wall clock for cache ages. */
  now?: () => number
  /** Monotonic clock for round-trip durations. */
  clock?: () => number
}

/** The server compares at most this many measured nodes per request. */
export const MAX_MEASURED_NODES = 12

/** Main-site node id in `/api/edges`; its region is also `main`. */
export const MAIN_NODE_ID = 'main'

/**
 * Nodes the server may choose for this request. A pinned region only draws
 * from that region, pinning `main` needs no measurement at all, and a
 * reconnect of a regional session is never moved to the main site.
 */
export function latencyCandidates<T extends LatencyTarget>(
  nodes: readonly T[],
  region: string,
  continuingEdge: boolean,
): T[] {
  if (region === MAIN_NODE_ID) return []
  const automatic = region === '' || region === 'auto'
  return nodes
    .filter((node) => automatic || node.region === region)
    .filter((node) => !(continuingEdge && node.id === MAIN_NODE_ID))
    .slice(0, MAX_MEASURED_NODES)
}

interface CacheEntry {
  ms: number | null
  at: number
}

export class EdgeLatencyMeter {
  private readonly cache = new Map<string, CacheEntry>()
  private readonly inflight = new Map<string, Promise<number | null>>()
  private readonly cacheMs: number
  private readonly failureCacheMs: number
  private readonly timeoutMs: number
  private readonly now: () => number
  private readonly clock: () => number
  private readonly probe: ProbeFn

  constructor(probe: ProbeFn, options: LatencyMeterOptions = {}) {
    this.probe = probe
    this.cacheMs = options.cacheMs ?? 5 * 60_000
    this.failureCacheMs = options.failureCacheMs ?? 30_000
    this.timeoutMs = options.timeoutMs ?? 2_500
    this.now = options.now ?? (() => Date.now())
    this.clock = options.clock ?? (() => performance.now())
  }

  /**
   * Fresh round-trip times for `targets`, keyed by node id. Cached values
   * return at once; the rest are awaited for at most `budgetMs` and keep
   * measuring in the background so the next session can use them.
   */
  async latencies(targets: readonly LatencyTarget[], budgetMs: number): Promise<Record<string, number>> {
    const result: Record<string, number> = {}
    const pending: Array<Promise<void>> = []
    for (const target of targets) {
      const entry = this.fresh(target)
      if (entry) {
        if (entry.ms !== null) result[target.id] = entry.ms
        continue
      }
      pending.push(this.measure(target).then((ms) => {
        if (ms !== null) result[target.id] = ms
      }))
    }
    if (pending.length > 0) {
      let timer: ReturnType<typeof setTimeout> | undefined
      await Promise.race([
        Promise.allSettled(pending),
        new Promise<void>((resolve) => { timer = setTimeout(resolve, budgetMs) }),
      ])
      if (timer !== undefined) clearTimeout(timer)
    }
    return { ...result }
  }

  /** Measures any target without a fresh result, without waiting for it. */
  warm(targets: readonly LatencyTarget[]): void {
    for (const target of targets) {
      if (!this.fresh(target)) void this.measure(target)
    }
  }

  private key(target: LatencyTarget): string {
    return `${target.id}\n${target.endpoint}`
  }

  private fresh(target: LatencyTarget): CacheEntry | undefined {
    const entry = this.cache.get(this.key(target))
    if (!entry) return undefined
    const ttl = entry.ms === null ? this.failureCacheMs : this.cacheMs
    return this.now() - entry.at < ttl ? entry : undefined
  }

  private measure(target: LatencyTarget): Promise<number | null> {
    const key = this.key(target)
    const running = this.inflight.get(key)
    if (running) return running
    const task = this.roundTrip(target)
      .then((ms) => {
        this.cache.set(key, { ms, at: this.now() })
        return ms
      })
      .finally(() => { this.inflight.delete(key) })
    this.inflight.set(key, task)
    return task
  }

  private async roundTrip(target: LatencyTarget): Promise<number | null> {
    const signal = AbortSignal.timeout(this.timeoutMs)
    try {
      // The first request pays for DNS, TCP and TLS; only the second is timed.
      if (!await untilAborted(this.probe(target, signal), signal)) return null
      const started = this.clock()
      if (!await untilAborted(this.probe(target, signal), signal)) return null
      return Math.max(0, this.clock() - started)
    } catch {
      return null
    }
  }
}

/** Settles with `work`, or rejects when `signal` aborts, whichever is first. */
function untilAborted<T>(work: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) return Promise.reject(signal.reason)
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    work.then(resolve, reject).finally(() => signal.removeEventListener('abort', abort))
  })
}
