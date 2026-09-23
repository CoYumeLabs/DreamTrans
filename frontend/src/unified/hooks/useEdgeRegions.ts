import { useEffect, useState } from 'react'
import { authFetch } from '../../pro/api/auth'
import { warmEdgeLatencies, type EdgeNode } from '../workspace/edgeSelection'

function previewEnabled(): boolean {
  try {
    return localStorage.getItem('dreamtrans.edge.preview') === 'true'
  } catch {
    return false
  }
}

/**
 * Regions a signed-in user can pick for new sessions. Mirrors `authorizeEdge`:
 * nodes are offered when Edge routing is on, or when a super administrator has
 * turned on the console's per-browser preview.
 */
export function useEdgeRegions(userId: string | null, role: string | null): string[] {
  const [regions, setRegions] = useState<string[]>([])
  useEffect(() => {
    setRegions([])
    if (!userId) return
    let active = true
    void authFetch<{ edge_enabled?: boolean; edge_control_enabled?: boolean }>('/api/system/access')
      .then(async (access) => {
        const previewing = Boolean(access.edge_control_enabled) && previewEnabled() && role === 'super_admin'
        if (!access.edge_enabled && !previewing) return
        const nodes = await authFetch<EdgeNode[]>('/api/edges')
        if (!active) return
        setRegions([...new Set(nodes.map((node) => node.region))])
        warmEdgeLatencies(nodes)
      })
      .catch(() => {
        // Without the node list the picker stays hidden and routing stays automatic.
      })
    return () => { active = false }
  }, [userId, role])
  return regions
}
