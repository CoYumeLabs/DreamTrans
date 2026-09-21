import { useCallback, useEffect, useState } from 'react'
import { authFetch } from '../api/auth'
import type { EdgeNode } from '../../unified/workspace/edgeSelection'
import { ErrorBanner, Modal } from './ui'

type Setup = { control_ready: boolean; routing_enabled: boolean; installer_ready: boolean; missing: string[]; release_image: string; tunnel_install_available: boolean }
const regions = [['ap-southeast-1', '新加坡'], ['ap-northeast-1', '东京'], ['eu-west-2', '伦敦'], ['us-west-2', '美国西部'], ['custom', '其他地区']]
const modes: Record<string, string> = { enabled: '参与调度', disabled: '暂停调度', draining: '正在排空', revoked: '身份已吊销' }
const configureCommand = 'sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans configure-edge'
const routingCommand = configureCommand + ' --routing on'

export function EdgesPage() {
  const [setup, setSetup] = useState<Setup | null>(null)
  const [nodes, setNodes] = useState<EdgeNode[]>([])
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [token, setToken] = useState('')
  const [installCommand, setInstallCommand] = useState('')
  const [installNode, setInstallNode] = useState('')
  const [tunnelMode, setTunnelMode] = useState('existing')
  const [releaseImage, setReleaseImage] = useState('')
  const [busy, setBusy] = useState(false)
  const [creating, setCreating] = useState(false)
  const [danger, setDanger] = useState<EdgeNode | null>(null)
  const [preview, setPreview] = useState(() => localStorage.getItem('dreamtrans.edge.preview') === 'true')
  const [regionChoice, setRegionChoice] = useState('ap-southeast-1')
  const [draft, setDraft] = useState({ name: '', region: 'ap-southeast-1', endpoint: '', max_connections: 2, training: false })
  const refresh = useCallback(async () => {
    const next = await authFetch<Setup>('/api/admin/edges/setup')
    setSetup(next)
    setNodes(next.control_ready ? await authFetch<EdgeNode[]>('/api/admin/edges') : [])
  }, [])
  useEffect(() => {
    const update = () => { void refresh().catch(reason => setError(String(reason))) }
    update(); const timer = setInterval(update, 10000)
    return () => clearInterval(timer)
  }, [refresh])
  async function action(work: () => Promise<void>) {
    setBusy(true); setError(''); setNotice('')
    try { await work() } catch (reason) { setError(reason instanceof Error ? reason.message : '操作失败，请重试') }
    finally { setBusy(false) }
  }
  async function installer(node: string, mode: string) {
    setInstallCommand('')
    const info = await authFetch<{ command: string }>('/api/admin/edges/installer?node=' + encodeURIComponent(node) + '&tunnel=' + mode)
    setInstallCommand(info.command)
  }
  async function copy(value: string) {
    await navigator.clipboard.writeText(value); setNotice('已复制')
  }
  async function change(node: EdgeNode, mode: string) {
    await authFetch('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ mode }) })
    await refresh()
  }
  const online = nodes.filter(node => node.heartbeat_at && Date.now() - Date.parse(node.heartbeat_at) < 30000).length
  return <section className="pa-stack pa-edges" aria-label="地区转录节点">
    <div className="pa-section-heading"><div><h2>地区转录节点</h2><p>先安装并验证节点，再开启正式转录调度。无需 Cloudflare API Key。</p></div>
      <button className="pa-button pa-button--primary" disabled={busy || !setup?.installer_ready} onClick={() => setCreating(!creating)}>添加节点</button>
    </div>
    <ErrorBanner message={error} onClose={() => setError('')} />
    {notice && <p role="status" className="pa-banner">{notice}</p>}
    {!setup && !error && <p role="status">正在检查主站配置…</p>}
    {setup && !setup.installer_ready && <div className="pa-card pa-edge-panel">
      <h3>先初始化节点管理</h3><p>尚未完成：{setup.missing.join('、')}。现有转录继续由主站处理。</p>
      <p>在主站运行以下命令。它会保留现有数据与配置、生成或复用签名密钥，并自动固定安装版本。</p>
      <pre>{configureCommand}</pre>
      <button className="pa-button" onClick={() => void action(() => copy(configureCommand))}>复制初始化命令</button>
      <button className="pa-button" disabled={busy} onClick={() => void action(refresh)}>重新检查</button>
    </div>}
    {setup?.control_ready && <>
      <div className="pa-summary-grid"><div><small>节点在线</small><strong>{online} / {nodes.length}</strong></div><div><small>活跃连接</small><strong>{nodes.reduce((sum, node) => sum + node.active, 0)}</strong></div><div><small>新会话入口</small><strong>{setup.routing_enabled ? '地区 Edge' : '主站'}</strong></div></div>
      <div className="pa-card pa-edge-panel">
        <h3>{setup.routing_enabled ? '正式调度已开启' : '准备阶段 · 普通用户仍使用主站'}</h3>
        <p>管理员可在本浏览器试用已启用的节点。测试账号仍按正常规则授权和计费。</p>
        <label className="pa-switch"><input type="checkbox" checked={preview} onChange={event => {
          const enabled = event.target.checked; localStorage.setItem('dreamtrans.edge.preview', String(enabled)); setPreview(enabled)
        }} /><span>本浏览器管理员试用 Edge</span></label>
        {!setup.routing_enabled && <details><summary>测试通过后开启正式调度</summary><p>先确认真实转录、历史保存和计费正常，再在主站执行。命令会检查在线节点容量。</p><pre>{routingCommand}</pre><button className="pa-button" onClick={() => void action(() => copy(routingCommand))}>复制启用命令</button></details>}
      </div>
    </>}
    {creating && setup?.installer_ready && <form className="pa-card pa-edge-panel" onSubmit={event => {
      event.preventDefault()
      void action(async () => {
        setToken(''); setInstallCommand(''); setInstallNode('')
        const result = await authFetch<{ id: string; registration_token: string }>('/api/admin/edges', { method: 'POST', body: JSON.stringify(draft) })
        setToken(result.registration_token); setInstallNode(result.id); setCreating(false)
        await refresh(); await installer(result.id, tunnelMode)
      })
    }}>
      <h3>1 · 创建节点</h3>
      <div className="pa-form-grid">
        <label><span>名称</span><input required placeholder="伦敦 01" value={draft.name} onChange={e => setDraft({ ...draft, name: e.target.value })} /></label>
        <label><span>地区</span><select aria-label="地区" value={regionChoice} onChange={e => { setRegionChoice(e.target.value); setDraft({ ...draft, region: e.target.value === 'custom' ? '' : e.target.value }) }}>{regions.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
        {regionChoice === 'custom' && <label><span>地区编码</span><input required value={draft.region} onChange={e => setDraft({ ...draft, region: e.target.value })} /></label>}
        <label><span>最大并发</span><input required type="number" min="1" max="4096" value={draft.max_connections} onChange={e => setDraft({ ...draft, max_connections: Number(e.target.value) })} /></label>
        <label className="pa-edge-wide"><span>独立 HTTPS 地址</span><input required type="url" placeholder="https://edge-london-01.yufolo.com" value={draft.endpoint} onChange={e => setDraft({ ...draft, endpoint: e.target.value })} /></label>
      </div>
      <label className="pa-switch"><input type="checkbox" checked={draft.training} onChange={e => setDraft({ ...draft, training: e.target.checked })} /><span>使用允许训练的独立供应商账号</span></label>
      <div className="pa-button-row"><button className="pa-button pa-button--primary" disabled={busy}>创建并生成注册凭证</button><button type="button" className="pa-button" onClick={() => setCreating(false)}>取消</button></div>
    </form>}
    {token && <div className="pa-card pa-edge-panel">
      <h3>2 · 在新机器安装</h3>
      <p>注册凭证 15 分钟有效。安装时分别隐藏输入注册凭证和节点独立 Speechmatics Key，无需主站数据库密码或 Cloudflare API Key。</p>
      <label className="pa-field"><span>Tunnel 接入方式</span><select aria-label="Tunnel 接入方式" value={tunnelMode} disabled={busy} onChange={e => {
        setTunnelMode(e.target.value); void action(() => installer(installNode, e.target.value))
      }}><option value="existing">复用机器上的 Tunnel</option>{setup?.tunnel_install_available && <option value="install">安装 Tunnel 客户端，输入节点 Token</option>}</select></label>
      <p>{tunnelMode === 'existing' ? '先自行配置独立域名与 Tunnel。宿主机上的 Tunnel 转发到 http://127.0.0.1:16003；安装命令不会索要 Cloudflare 凭证。' : '自行创建该节点的独立 Tunnel，服务地址设置为 http://dreamtrans:8080；安装器会提示输入该 Tunnel 的专用 Token。'}</p>
      <label className="pa-field"><span>注册凭证</span><input type="password" readOnly value={token} /></label>
      <div className="pa-button-row"><button className="pa-button" onClick={() => void action(() => copy(token))}>复制凭证</button><button className="pa-button" onClick={() => { setToken(''); setInstallCommand(''); setInstallNode('') }}>清除显示</button></div>
      {installCommand ? <><pre>{installCommand}</pre><button className="pa-button pa-button--primary" onClick={() => void action(() => copy(installCommand))}>复制安装命令</button></> : <button className="pa-button" disabled={busy} onClick={() => void action(() => installer(installNode, tunnelMode))}>重新获取安装命令</button>}
      <p>安装后回到下方查看心跳，检测公网入口，再启用该节点。凭证过期可在节点操作中重新生成。</p>
    </div>}
    {setup?.control_ready && <div className="pa-card pa-edge-panel">
      <h3>3 · 验证与管理节点</h3>
      {!nodes.length ? <p className="pa-empty">还没有节点。添加节点后，这里会显示安装状态、心跳和连接数。</p> : <div className="pa-table-wrap"><table>
        <thead><tr><th>节点 / 地区</th><th>状态</th><th>连接 / 积压</th><th>操作</th></tr></thead>
        <tbody>{nodes.map(node => <tr key={node.id}>
          <td><strong>{node.name}</strong><br />{node.region}<br /><small>{node.endpoint}</small><details><summary>版本与协议</summary><small>{node.version || '尚未安装'} · 协议 {node.protocol_min}–{node.protocol_max}</small></details></td>
          <td>{modes[node.mode] || node.mode}<br /><small>{node.heartbeat_at ? (Date.now() - Date.parse(node.heartbeat_at) < 30000 ? '心跳正常' : '心跳已过期') : '等待安装注册'}</small></td>
          <td>{node.active} / {node.max_connections}<br /><small>{node.metrics.queue_bytes ?? 0} B 待回传 · 最老 {Math.round(node.metrics.oldest_event_seconds ?? 0)} 秒</small></td>
          <td><div className="pa-actions">
            <button className="pa-button" disabled={busy || node.mode === 'revoked'} onClick={() => void action(async () => {
              const result = await fetch(node.endpoint + '/probe', { signal: AbortSignal.timeout(5000), credentials: 'omit', cache: 'no-store' })
              if (!result.ok) throw new Error('节点入口检测失败：HTTP ' + result.status)
              setNotice(node.name + ' 入口可达；仍需完成真实转录与保存测试')
            })}>检测入口</button>
            <button className="pa-button" disabled={busy || node.mode === 'revoked' || node.mode === 'enabled' || !node.heartbeat_at} onClick={() => void action(() => change(node, 'enabled'))}>启用调度</button>
            <button className="pa-button" disabled={busy || node.mode === 'revoked' || node.mode === 'draining'} onClick={() => void action(() => change(node, 'draining'))}>排空</button>
            <details><summary>更多操作</summary><div className="pa-button-row">
              <button className="pa-button" disabled={busy || node.mode === 'revoked' || node.mode === 'disabled'} onClick={() => void action(() => change(node, 'disabled'))}>禁用调度</button>
              <button className="pa-button" disabled={busy || node.mode === 'revoked' || !(releaseImage || setup.release_image)} onClick={() => void action(async () => {
                await authFetch('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ image: releaseImage || setup.release_image }) }); setNotice('已请求 ' + node.name + ' 升级；节点会在后台领取固定版本'); await refresh()
              })}>请求此节点升级</button>
              <button className="pa-button" disabled={busy || node.mode === 'revoked'} onClick={() => void action(async () => {
                setToken(''); setInstallCommand(''); setInstallNode('')
                const value = await authFetch<{ registration_token: string }>('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ rotate: true }) })
                setToken(value.registration_token); setInstallNode(node.id); await refresh(); await installer(node.id, tunnelMode)
              })}>重新生成注册凭证</button>
              <button className="pa-button pa-button--danger-quiet" disabled={busy || node.mode === 'revoked'} onClick={() => setDanger(node)}>吊销身份</button>
            </div></details>
          </div></td>
        </tr>)}</tbody>
      </table></div>}
      <details><summary>高级发布设置</summary><p>默认使用主站配置的固定 Edge 发行版本。这里仅覆盖本次节点升级，不改变新节点安装版本。</p><label className="pa-field"><span>地区发布镜像（repository@sha256:digest）</span><input value={releaseImage} placeholder={setup.release_image} onChange={e => setReleaseImage(e.target.value)} /></label></details>
    </div>}
    {danger && <Modal title={'吊销 ' + danger.name + ' 的节点身份'} danger onClose={() => setDanger(null)} footer={<><button className="pa-button" onClick={() => setDanger(null)}>取消</button><button className="pa-button pa-button--danger" disabled={busy} onClick={() => void action(async () => { await change(danger, 'revoked'); setDanger(null) })}>确认吊销</button></>}><p>该节点将无法继续认证和上报。先排空连接并确认待回传数据已处理；重新启用需要创建新的节点身份。</p></Modal>}
  </section>
}
