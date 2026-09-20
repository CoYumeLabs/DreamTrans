import { useCallback, useEffect, useState } from 'react'
import { authFetch } from '../api/auth'
import type { EdgeNode } from '../../unified/workspace/edgeSelection'

export function EdgesPage() {
  const [nodes, setNodes] = useState<EdgeNode[]>([])
  const [error, setError] = useState('')
  const [token, setToken] = useState('')
 const [installCommand, setInstallCommand] = useState('')
 const [releaseImage,setReleaseImage] = useState('')
  const [busy, setBusy] = useState(false)
  const [draft, setDraft] = useState({ name: '', region: '', endpoint: '', max_connections: 8, training: false })
  const refresh = useCallback(async () => {
    try { setNodes(await authFetch<EdgeNode[]>('/api/admin/edges')); setError('') }
    catch (reason) { setError(reason instanceof Error ? reason.message : '无法读取节点') }
  }, [])
  useEffect(() => { void refresh(); const timer = setInterval(() => void refresh(), 10000); return () => clearInterval(timer) }, [refresh])
  async function change(node: EdgeNode, mode: string) {
    setBusy(true)
    try {
      await authFetch('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ mode }) })
      await refresh()
    } catch (reason) { setError(reason instanceof Error ? reason.message : '操作失败') }
    finally { setBusy(false) }
  }
  return <section className="pa-stack" aria-label="地区转录节点">
    <h2>地区转录节点</h2>
    <p>主站授权新会话，Edge 直接处理音频。排空后停止分配新会话，已有会话继续运行。</p>
    {error && <p role="alert">{error}</p>}
    <form className="pa-card" onSubmit={event => {
      event.preventDefault(); setBusy(true); setToken('')
      void authFetch<{ registration_token: string }>('/api/admin/edges', { method: 'POST', body: JSON.stringify(draft) })
        .then(result => { setToken(result.registration_token); void authFetch<{ command: string }>('/api/admin/edges/installer').then(info => setInstallCommand(info.command)).catch(() => setInstallCommand('')); return refresh() })
        .catch(reason => setError(reason instanceof Error ? reason.message : '创建失败'))
        .finally(() => setBusy(false))
    }}>
      <h3>创建节点</h3>
      <label>名称 <input required value={draft.name} onChange={e => setDraft({ ...draft, name: e.target.value })} /></label>
      <label>地区 <input required placeholder="ap-northeast-1" value={draft.region} onChange={e => setDraft({ ...draft, region: e.target.value })} /></label>
      <label>独立 HTTPS 地址 <input required type="url" placeholder="https://edge-tokyo-1.yufolo.com" value={draft.endpoint} onChange={e => setDraft({ ...draft, endpoint: e.target.value })} /></label>
      <label>最大并发 <input required type="number" min="1" max="4096" value={draft.max_connections} onChange={e => setDraft({ ...draft, max_connections: Number(e.target.value) })} /></label>
      <label><input type="checkbox" checked={draft.training} onChange={e => setDraft({ ...draft, training: e.target.checked })} />该节点使用允许训练的独立供应商账号</label>
      <button disabled={busy}>创建并生成注册凭证</button>
    </form>
    {token && <div className="pa-card">
      <h3>注册凭证（15 分钟有效）</h3>
      {installCommand && <><p>在新 Edge 主机复制运行以下命令，安装器会提示输入凭证：</p><pre style={{ whiteSpace: 'pre-wrap' }}>{installCommand}</pre><button onClick={() => void navigator.clipboard.writeText(installCommand)}>复制安装命令</button></>}
      <p>在 Edge 安装器的隐藏输入提示中粘贴，完成注册后失效。不要放进命令行参数或普通日志。</p>
      <input aria-label="注册凭证" type="password" readOnly value={token} />
      <button onClick={() => void navigator.clipboard.writeText(token)}>复制凭证</button>
      <button onClick={() => setToken('')}>清除显示</button>
    </div>}
    <label>地区发布镜像（repository@sha256:digest）<input value={releaseImage} onChange={e => setReleaseImage(e.target.value)} /></label>
    <div className="pa-card" style={{ overflowX: 'auto' }}><table>
      <thead><tr><th>节点 / 地区</th><th>状态 / 心跳</th><th>版本 / 协议</th><th>并发</th><th>供应商链路</th><th>回传积压</th><th>操作</th></tr></thead>
      <tbody>{nodes.map(node => <tr key={node.id}>
        <td>{node.name}<br />{node.region}<br /><small>{node.endpoint}</small></td>
        <td>{node.mode}<br />{node.heartbeat_at ? new Date(node.heartbeat_at).toLocaleString() : '尚未注册或心跳'}</td>
        <td>{node.version || '—'}<br />{node.protocol_min}–{node.protocol_max}</td>
        <td>{node.active} / {node.max_connections}</td>
        <td>{node.metrics.provider_latency_ms ?? '—'} ms</td>
        <td>{node.metrics.queue_bytes ?? 0} B<br />最老 {Math.round(node.metrics.oldest_event_seconds ?? 0)} 秒</td>
        <td><button disabled={busy || node.mode === 'revoked'} onClick={() => {
          setBusy(true); void authFetch('/api/admin/edges/' + node.id + '/tunnel', { method: 'POST', body: JSON.stringify({ automatic: true }) }).then(refresh).catch(reason => setError(String(reason))).finally(() => setBusy(false))
        }}>配置独立 Tunnel</button><button disabled={busy || !releaseImage || node.mode === 'revoked'} onClick={() => {
          setBusy(true); void authFetch('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ image: releaseImage }) }).then(refresh).catch(reason => setError(String(reason))).finally(() => setBusy(false))
        }}>请求此节点升级</button><button disabled={busy} onClick={() => {
          setBusy(true); void authFetch<{ registration_token: string }>('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ rotate: true }) }).then(value => { setToken(value.registration_token); return refresh() }).catch(reason => setError(String(reason))).finally(() => setBusy(false))
        }}>轮换身份</button>{['enabled' , 'disabled', 'draining', 'revoked'].map((mode, i) => <button key={mode} disabled={busy || node.mode === mode || node.mode === 'revoked'} onClick={() => void change(node, mode)}>{['启用调度', '禁用调度', '排空', '吊销身份'][i]}</button>)}</td>
      </tr>)}</tbody>
    </table></div>
  </section>
}
