import { authFetch } from '../../pro/api/auth'
import type { EdgeAuthorization } from '../../core/transcription/RegionalEdge'
import { readEdgeRegion } from './edgeRegions'
import { EdgeLatencyMeter, latencyCandidates, MAIN_NODE_ID, type ProbeFn } from './edgeLatency'

export interface EdgeNode {
  id: string; name: string; region: string; endpoint: string; mode: string
  active: number; max_connections: number; version: string; heartbeat_at: string | null
  protocol_min: number; protocol_max: number
  metrics: { provider_latency_ms?: number; load?: number; queue_bytes?: number; oldest_event_seconds?: number }
}

/** Longest a new session waits for node measurements before authorizing. */
const START_BUDGET_MS = 1_200
/** A reconnect keeps its region and should resume quickly. */
const RECONNECT_BUDGET_MS = 600

const probeNode: ProbeFn = async (node, signal) => {
  const options = { signal, cache: 'no-store' as const }
  if (node.id === MAIN_NODE_ID) {
    return (await authFetch<{ ready: boolean }>('/api/edges/probe', options)).ready
  }
  return (await fetch(node.endpoint + '/probe', { ...options, credentials: 'omit' })).ok
}

const latencyMeter = new EdgeLatencyMeter(probeNode)

/**
 * Measures the nodes an automatic session could use while the workspace is
 * idle, so pressing record does not wait for the round trips.
 */
export function warmEdgeLatencies(nodes: readonly EdgeNode[]): void {
  latencyMeter.warm(latencyCandidates(nodes, readEdgeRegion(), false))
}

export async function authorizeEdge(sessionId: string, sampleRate: number, continuingEdge = false, requestedRegion?: string): Promise<EdgeAuthorization | null> {
  const access = await authFetch<{ edge_enabled?: boolean; edge_control_enabled?: boolean }>('/api/system/access')
  let preview = localStorage.getItem('dreamtrans.edge.preview') === 'true' && !!access.edge_control_enabled
  if (!access.edge_enabled && preview) {
    const profile = await authFetch<{ user: { role: string } }>('/api/user/profile')
    preview = profile.user.role === 'super_admin'
  }
  if (!access.edge_enabled && !preview && !continuingEdge) return null
  const nodes = await authFetch<EdgeNode[]>('/api/edges')
  const region = requestedRegion ?? readEdgeRegion()
  const latencies = await latencyMeter.latencies(
    latencyCandidates(nodes, region, continuingEdge),
    continuingEdge ? RECONNECT_BUDGET_MS : START_BUDGET_MS,
  )
  const result = await authFetch<EdgeAuthorization | { transport: 'main' }>('/api/edges/authorize', {
    method: 'POST',
    body: JSON.stringify({ preview, session_id: sessionId, protocol: 2, sample_rate: sampleRate,
      ...(!continuingEdge && nodes.some(node => node.id === MAIN_NODE_ID) ? { allow_main: true } : {}),
      region, latencies }),
  })
  if ('transport' in result && result.transport === 'main') return null
  return { ...result as EdgeAuthorization, requestedRegion: region }
}
