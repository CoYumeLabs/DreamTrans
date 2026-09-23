import { authFetch } from '../../pro/api/auth'
import type { EdgeAuthorization } from '../../core/transcription/RegionalEdge'

export interface EdgeNode {
  id: string; name: string; region: string; endpoint: string; mode: string
  active: number; max_connections: number; version: string; heartbeat_at: string | null
  protocol_min: number; protocol_max: number
  metrics: { provider_latency_ms?: number; load?: number; queue_bytes?: number; oldest_event_seconds?: number }
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
  const latencies: Record<string, number> = {}
  await Promise.allSettled(nodes.slice(0, 12).map(async node => {
    const start = performance.now()
    const options = { signal: AbortSignal.timeout(2500), cache: 'no-store' as const }
    const response = node.id === 'main'
      ? await authFetch<{ ready: boolean }>('/api/edges/probe', options).then(value => ({ ok: value.ready }))
      : await fetch(node.endpoint + '/probe', { ...options, credentials: 'omit' })
    if (response.ok) latencies[node.id] = performance.now() - start
  }))
  const region = requestedRegion ?? (localStorage.getItem('dreamtrans.edge.region') || 'auto')
  const result = await authFetch<EdgeAuthorization | { transport: 'main' }>('/api/edges/authorize', {
    method: 'POST',
    body: JSON.stringify({ preview, session_id: sessionId, protocol: 2, sample_rate: sampleRate,
      ...(!continuingEdge && nodes.some(node => node.id === 'main') ? { allow_main: true } : {}),
      region, latencies }),
  })
  if ('transport' in result && result.transport === 'main') return null
  return { ...result as EdgeAuthorization, requestedRegion: region }
}
