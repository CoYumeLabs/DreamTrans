import { useEffect, useState } from 'react'
import { authFetch, getAccessToken } from '../../pro/api/auth'
import type { EdgeNode } from '../workspace/edgeSelection'

export function EdgeRegionSelector() {
  const [regions, setRegions] = useState<string[]>([])
  const [selected, setSelected] = useState(() => localStorage.getItem('dreamtrans.edge.region') || 'auto')
  useEffect(() => {
    if (!getAccessToken()) return
    let active = true
    void authFetch<{ edge_enabled?: boolean }>('/api/system/access').then(async access => {
      if (!access.edge_enabled) return
      const nodes = await authFetch<EdgeNode[]>('/api/edges')
      if (active) setRegions([...new Set(nodes.map(node => node.region))])
    }).catch(() => {})
    return () => { active = false }
  }, [])
  if (!regions.length) return null
  return <label>新转录会话接入地区 <select value={selected} onChange={event => {
    setSelected(event.target.value); localStorage.setItem('dreamtrans.edge.region', event.target.value)
  }}>
    <option value="auto">自动（测量延迟与可用容量）</option>
    {regions.map(region => <option key={region} value={region}>{region}</option>)}
  </select></label>
}
