import { authFetch } from '../../pro/api/auth'
import type { EdgeAuthorization } from '../../core/transcription/RegionalEdge'

export interface EdgeNode {
  id: string; name: string; region: string; endpoint: string; mode: string
  active: number; max_connections: number; version: string; heartbeat_at: string | null
  protocol_min: number; protocol_max: number
  metrics: { provider_latency_ms?: number; load?: number; queue_bytes?: number; oldest_event_seconds?: number }
}

export async function authorizeEdge(sessionId: string, sampleRate: number): Promise<EdgeAuthorization | null> {
  const access = await authFetch<{ edge_enabled?: boolean }>('/api/system/access')
  if (!access.edge_enabled) return null
  const nodes = await authFetch<EdgeNode[]>('/api/edges')
  const latencies: Record<string, number> = {}
  await Promise.allSettled(nodes.slice(0, 12).map(async node => {
    const start = performance.now()
    const response = await fetch(node.endpoint + '/probe', {
      signal: AbortSignal.timeout(2500), cache: 'no-store', credentials: 'omit',
    })
    if (response.ok) latencies[node.id] = performance.now() - start
  }))
  return authFetch<EdgeAuthorization>('/api/edges/authorize', {
    method: 'POST',
    body: JSON.stringify({ session_id: sessionId, sample_rate: sampleRate,
      region: localStorage.getItem('dreamtrans.edge.region') || 'auto', latencies }),
  })
}
