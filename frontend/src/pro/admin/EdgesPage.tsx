import { useCallback, useEffect, useState } from 'react'
import { authFetch } from '../api/auth'
import type { EdgeNode } from '../../unified/workspace/edgeSelection'
import { EDGE_REGION_CODES, edgeRegionLabel } from '../../unified/workspace/edgeRegions'
import { ErrorBanner, Modal } from './ui'

type Setup = {
  provider_ready?: boolean
  training_provider_ready?: boolean
  control_ready: boolean
  routing_enabled: boolean
  installer_ready: boolean
  missing: string[]
  release_image: string
  tunnel_install_available: boolean
}

type Draft = { name: string; region: string; endpoint: string; max_connections: number; training: boolean }

/** A node counts as online while its last heartbeat is this recent. */
const HEARTBEAT_FRESH_MS = 30_000
const PREVIEW_KEY = 'dreamtrans.edge.preview'
const configureCommand = 'sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans configure-edge'
const routingCommand = configureCommand + ' --routing on'

const modeLabels: Record<string, string> = {
  enabled: '参与调度',
  disabled: '暂停调度',
  draining: '正在排空',
  revoked: '身份已吊销',
}

const modeTones: Record<string, string> = {
  enabled: 'is-good',
  disabled: 'is-muted',
  draining: 'is-warn',
  revoked: 'is-bad',
}

function heartbeatAge(node: EdgeNode, now: number): number | null {
  return node.heartbeat_at ? now - Date.parse(node.heartbeat_at) : null
}

function isOnline(node: EdgeNode, now: number): boolean {
  const age = heartbeatAge(node, now)
  return age !== null && age < HEARTBEAT_FRESH_MS
}

function formatAge(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1000))
  if (seconds < 60) return `${seconds} 秒前`
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.round(minutes / 60)
  if (hours < 48) return `${hours} 小时前`
  return `${Math.round(hours / 24)} 天前`
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

function readPreview(): boolean {
  try {
    return localStorage.getItem(PREVIEW_KEY) === 'true'
  } catch {
    return false
  }
}

function reasonText(reason: unknown): string {
  return reason instanceof Error ? reason.message : '操作失败，请重试'
}

export function EdgesPage() {
  const [setup, setSetup] = useState<Setup | null>(null)
  const [nodes, setNodes] = useState<EdgeNode[]>([])
  const [now, setNow] = useState(() => Date.now())
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
  const [preview, setPreview] = useState(readPreview)
  const [regionChoice, setRegionChoice] = useState<string>(EDGE_REGION_CODES[0])
  const [draft, setDraft] = useState<Draft>({ name: '', region: EDGE_REGION_CODES[0], endpoint: '', max_connections: 2, training: false })

  const refresh = useCallback(async () => {
    const next = await authFetch<Setup>('/api/admin/edges/setup')
    setSetup(next)
    setNodes(next.control_ready ? await authFetch<EdgeNode[]>('/api/admin/edges') : [])
    setNow(Date.now())
  }, [])

  useEffect(() => {
    const update = () => { void refresh().catch((reason) => setError(reasonText(reason))) }
    update()
    const timer = setInterval(update, 10_000)
    return () => clearInterval(timer)
  }, [refresh])

  async function action(work: () => Promise<void>) {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await work()
    } catch (reason) {
      setError(reasonText(reason))
    } finally {
      setBusy(false)
    }
  }

  async function installer(node: string, mode: string) {
    setInstallCommand('')
    const info = await authFetch<{ command: string }>('/api/admin/edges/installer?node=' + encodeURIComponent(node) + '&tunnel=' + mode)
    setInstallCommand(info.command)
  }

  async function copy(value: string, what: string) {
    await navigator.clipboard.writeText(value)
    setNotice(`已复制${what}`)
  }

  async function change(node: EdgeNode, mode: string) {
    await authFetch('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ mode }) })
    await refresh()
  }

  function clearInstall() {
    setToken('')
    setInstallCommand('')
    setInstallNode('')
  }

  const online = nodes.filter((node) => isOnline(node, now)).length
  const activeConnections = nodes.reduce((sum, node) => sum + node.active, 0)
  const capacity = nodes.filter((node) => node.mode === 'enabled').reduce((sum, node) => sum + node.max_connections, 0)
  const installName = nodes.find((node) => node.id === installNode)?.name ?? draft.name
  const providerReady = draft.training ? setup?.training_provider_ready : setup?.provider_ready

  return (
    <section className="pa-stack pa-edges" aria-label="地区转录节点">
      <div className="pa-section-heading">
        <p>让海外用户就近接入转录。主站始终可用；节点先安装、验证，再参与调度。</p>
        <button
          className="pa-button pa-button--primary"
          disabled={busy || !setup?.installer_ready}
          onClick={() => setCreating(!creating)}
          type="button"
        >
          添加节点
        </button>
      </div>

      <ErrorBanner message={error} onClose={() => setError('')} />
      {notice && <p role="status" className="pa-banner">{notice}</p>}
      {!setup && !error && <p role="status">正在检查主站配置…</p>}

      {setup && !setup.installer_ready && (
        <div className="pa-card pa-edge-panel">
          <h3>先初始化节点管理</h3>
          <p>尚未完成：{setup.missing.join('、')}。现有转录继续由主站处理。</p>
          <p>在主站运行以下命令。它会保留现有数据与配置、生成或复用签名密钥，并自动固定安装版本。</p>
          <pre>{configureCommand}</pre>
          <div className="pa-button-row">
            <button className="pa-button" onClick={() => void action(() => copy(configureCommand, '初始化命令'))} type="button">复制初始化命令</button>
            <button className="pa-button" disabled={busy} onClick={() => void action(refresh)} type="button">重新检查</button>
          </div>
        </div>
      )}

      {setup?.control_ready && (
        <>
          <div className="pa-summary-grid">
            <div>
              <small>在线节点</small>
              <strong>{online} / {nodes.length}</strong>
              <em>{HEARTBEAT_FRESH_MS / 1000} 秒内有心跳</em>
            </div>
            <div>
              <small>活跃连接</small>
              <strong>{activeConnections}</strong>
              <em>参与调度的节点共 {capacity} 路并发</em>
            </div>
            <div>
              <small>新会话入口</small>
              <strong>{setup.routing_enabled ? '地区节点 + 主站' : '仅主站'}</strong>
              <em>{setup.routing_enabled ? '用户可在「会话配置」里选择接入节点' : '准备阶段，普通用户不会看到节点'}</em>
            </div>
          </div>

          <div className="pa-card pa-edge-panel">
            <div className="pa-edge-panel__head">
              <h3>调度</h3>
              <span className={`pa-status ${setup.routing_enabled ? 'is-good' : 'is-warn'}`}>
                {setup.routing_enabled ? '正式调度已开启' : '准备阶段'}
              </span>
            </div>
            <label className="pa-switch">
              <span>
                <strong>在本浏览器试用地区节点</strong>
                <small>只对超级管理员生效。开启后，工作区「会话配置」会出现接入节点选择，未开启正式调度也能测试；测试会话照常授权和计费。</small>
              </span>
              <input
                checked={preview}
                onChange={(event) => {
                  const enabled = event.target.checked
                  try { localStorage.setItem(PREVIEW_KEY, String(enabled)) } catch { /* preview stays for this page only */ }
                  setPreview(enabled)
                }}
                type="checkbox"
              />
            </label>
            {!setup.routing_enabled && (
              <details>
                <summary>测试通过后开启正式调度</summary>
                <p>先确认真实转录、历史保存和计费正常，再在主站执行。命令会检查在线节点容量。</p>
                <pre>{routingCommand}</pre>
                <button className="pa-button" onClick={() => void action(() => copy(routingCommand, '启用命令'))} type="button">复制启用命令</button>
              </details>
            )}
          </div>
        </>
      )}

      {creating && setup?.installer_ready && (
        <form
          className="pa-card pa-edge-panel"
          onSubmit={(event) => {
            event.preventDefault()
            void action(async () => {
              clearInstall()
              const result = await authFetch<{ id: string; registration_token: string }>('/api/admin/edges', { method: 'POST', body: JSON.stringify(draft) })
              setToken(result.registration_token)
              setInstallNode(result.id)
              setCreating(false)
              await refresh()
              await installer(result.id, tunnelMode)
            })
          }}
        >
          <h3>添加节点</h3>
          <p>创建后会生成一次性注册凭证和安装命令，在新机器上运行即可。</p>
          <div className="pa-form-grid">
            <label>
              <span>名称</span>
              <input required placeholder="伦敦 01" value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
            </label>
            <label>
              <span>地区</span>
              <select
                aria-label="地区"
                value={regionChoice}
                onChange={(event) => {
                  setRegionChoice(event.target.value)
                  setDraft({ ...draft, region: event.target.value === 'custom' ? '' : event.target.value })
                }}
              >
                {EDGE_REGION_CODES.map((code) => <option key={code} value={code}>{edgeRegionLabel(code)} · {code}</option>)}
                <option value="custom">其他地区</option>
              </select>
            </label>
            {regionChoice === 'custom' && (
              <label>
                <span>地区编码</span>
                <input required placeholder="例如 ap-south-1" value={draft.region} onChange={(event) => setDraft({ ...draft, region: event.target.value })} />
              </label>
            )}
            <label>
              <span>最大并发</span>
              <input required type="number" min="1" max="4096" value={draft.max_connections} onChange={(event) => setDraft({ ...draft, max_connections: Number(event.target.value) })} />
            </label>
            <label className="pa-edge-wide">
              <span>独立 HTTPS 地址</span>
              <input required type="url" placeholder="https://edge-london-01.yufolo.com" value={draft.endpoint} onChange={(event) => setDraft({ ...draft, endpoint: event.target.value })} />
            </label>
          </div>
          <label className="pa-switch">
            <span>
              <strong>使用允许训练的供应商账号</strong>
              <small>这个节点只接收已加入训练计划的用户。</small>
            </span>
            <input type="checkbox" checked={draft.training} onChange={(event) => setDraft({ ...draft, training: event.target.checked })} />
          </label>
          <p className={providerReady ? 'pa-text-good' : 'pa-text-warn'}>
            {providerReady
              ? '此账号由主站管理，安装后自动领取短期授权。'
              : '主站尚未配置对应账号。安装时需输入节点独立 Speechmatics Key，或先完善主站供应商配置。'}
          </p>
          <div className="pa-button-row">
            <button className="pa-button pa-button--primary" disabled={busy} type="submit">创建并生成注册凭证</button>
            <button type="button" className="pa-button" onClick={() => setCreating(false)}>取消</button>
          </div>
        </form>
      )}

      {token && (
        <div className="pa-card pa-edge-panel">
          <h3>安装 {installName}</h3>
          <ol className="pa-edge-steps">
            <li>选择 Tunnel 接入方式，复制安装命令到新机器运行。</li>
            <li>安装器提示时粘贴注册凭证（15 分钟内有效，输入时隐藏）。</li>
            <li>在下方节点列表看到「在线」后，点「检测入口」，再「启用调度」。</li>
          </ol>
          <label className="pa-field">
            <span>Tunnel 接入方式</span>
            <select
              aria-label="Tunnel 接入方式"
              value={tunnelMode}
              disabled={busy}
              onChange={(event) => {
                setTunnelMode(event.target.value)
                void action(() => installer(installNode, event.target.value))
              }}
            >
              <option value="existing">复用机器上的 Tunnel</option>
              {setup?.tunnel_install_available && <option value="install">安装 Tunnel 客户端，输入节点 Token</option>}
            </select>
          </label>
          <p className="pa-muted">
            {tunnelMode === 'existing'
              ? '先自行配置独立域名与 Tunnel，把它转发到宿主机的 http://127.0.0.1:16003；安装命令不会索要 Cloudflare 凭证。'
              : '自行创建该节点的独立 Tunnel，服务地址设置为 http://dreamtrans:8080；安装器会提示输入该 Tunnel 的专用 Token。'}
          </p>
          {installCommand
            ? (
              <>
                <pre>{installCommand}</pre>
                <button className="pa-button pa-button--primary" onClick={() => void action(() => copy(installCommand, '安装命令'))} type="button">复制安装命令</button>
              </>
            )
            : <button className="pa-button" disabled={busy} onClick={() => void action(() => installer(installNode, tunnelMode))} type="button">重新获取安装命令</button>}
          <label className="pa-field">
            <span>注册凭证</span>
            <input type="password" readOnly value={token} />
          </label>
          <p className="pa-muted">主站已配置对应 Speechmatics 账号时，节点自动领取短期授权，无需保存长期供应商 Key；不需要主站数据库密码或 Cloudflare API Key。凭证过期可在节点的「更多操作」里重新生成。</p>
          <div className="pa-button-row">
            <button className="pa-button" onClick={() => void action(() => copy(token, '注册凭证'))} type="button">复制凭证</button>
            <button className="pa-button" onClick={clearInstall} type="button">清除显示</button>
          </div>
        </div>
      )}

      {setup?.control_ready && (
        <div className="pa-card pa-edge-panel">
          <h3>节点</h3>
          {!nodes.length
            ? <p className="pa-empty">还没有节点。添加节点后，这里会显示安装状态、心跳和连接数。</p>
            : (
              <div className="pa-table-wrap">
                <table className="pa-edge-table">
                  <thead>
                    <tr><th>节点</th><th>状态</th><th>负载</th><th>操作</th></tr>
                  </thead>
                  <tbody>
                    {nodes.map((node) => {
                      const age = heartbeatAge(node, now)
                      const nodeOnline = isOnline(node, now)
                      const backlog = node.metrics.queue_bytes ?? 0
                      const load = node.max_connections > 0 ? Math.min(1, node.active / node.max_connections) : 0
                      return (
                        <tr key={node.id}>
                          <td>
                            <strong>{node.name}</strong>
                            <small>{edgeRegionLabel(node.region)} · {node.region}</small>
                            <small className="pa-edge-endpoint">{node.endpoint}</small>
                            <small>{node.version ? `${node.version} · 协议 ${node.protocol_min}–${node.protocol_max}` : '尚未安装'}</small>
                          </td>
                          <td>
                            <span className={`pa-status ${age === null ? 'is-muted' : nodeOnline ? 'is-good' : 'is-bad'}`}>
                              {age === null ? '等待安装注册' : nodeOnline ? '在线' : '心跳中断'}
                            </span>
                            <span className={`pa-status ${modeTones[node.mode] ?? 'is-muted'}`}>{modeLabels[node.mode] ?? node.mode}</span>
                            {age !== null && <small>最后心跳 {formatAge(age)}</small>}
                          </td>
                          <td>
                            <strong>{node.active} / {node.max_connections}</strong>
                            <span className="pa-edge-load" aria-hidden="true"><span style={{ width: `${Math.round(load * 100)}%` }} /></span>
                            {backlog > 0
                              ? <small className="pa-text-warn">待回传 {formatBytes(backlog)} · 最久 {Math.round(node.metrics.oldest_event_seconds ?? 0)} 秒</small>
                              : <small>无待回传数据</small>}
                          </td>
                          <td>
                            <div className="pa-actions">
                              <button
                                className="pa-button"
                                disabled={busy || node.mode === 'revoked'}
                                onClick={() => void action(async () => {
                                  const result = await fetch(node.endpoint + '/probe', { signal: AbortSignal.timeout(5000), credentials: 'omit', cache: 'no-store' })
                                  if (!result.ok) throw new Error(`${node.name} 入口检测失败：HTTP ${result.status}`)
                                  setNotice(`${node.name} 入口可达；仍需完成一次真实转录与保存测试`)
                                })}
                                type="button"
                              >
                                检测入口
                              </button>
                              <button
                                className="pa-button"
                                disabled={busy || node.mode === 'revoked' || node.mode === 'enabled' || !nodeOnline}
                                onClick={() => void action(() => change(node, 'enabled'))}
                                title={!nodeOnline ? '节点在线后才能启用调度' : undefined}
                                type="button"
                              >
                                启用调度
                              </button>
                              <button
                                className="pa-button"
                                disabled={busy || node.mode === 'revoked' || node.mode === 'draining'}
                                onClick={() => void action(() => change(node, 'draining'))}
                                title="不再分配新会话，已有连接自然结束"
                                type="button"
                              >
                                排空
                              </button>
                              <details className="pa-edge-more">
                                <summary>更多操作</summary>
                                <div>
                                  <button
                                    className="pa-button"
                                    disabled={busy || node.mode === 'revoked' || node.mode === 'disabled'}
                                    onClick={() => void action(() => change(node, 'disabled'))}
                                    type="button"
                                  >
                                    暂停调度
                                  </button>
                                  <button
                                    className="pa-button"
                                    disabled={busy || node.mode === 'revoked' || !(releaseImage || setup.release_image)}
                                    onClick={() => void action(async () => {
                                      await authFetch('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ image: releaseImage || setup.release_image }) })
                                      setNotice(`已请求 ${node.name} 升级；节点会在后台领取固定版本`)
                                      await refresh()
                                    })}
                                    type="button"
                                  >
                                    请求此节点升级
                                  </button>
                                  <button
                                    className="pa-button"
                                    disabled={busy || node.mode === 'revoked'}
                                    onClick={() => void action(async () => {
                                      clearInstall()
                                      const value = await authFetch<{ registration_token: string }>('/api/admin/edges/' + node.id, { method: 'POST', body: JSON.stringify({ rotate: true }) })
                                      setToken(value.registration_token)
                                      setInstallNode(node.id)
                                      await refresh()
                                      await installer(node.id, tunnelMode)
                                    })}
                                    type="button"
                                  >
                                    重新生成注册凭证
                                  </button>
                                  <button
                                    className="pa-button pa-button--danger-quiet"
                                    disabled={busy || node.mode === 'revoked'}
                                    onClick={() => setDanger(node)}
                                    type="button"
                                  >
                                    吊销身份
                                  </button>
                                </div>
                              </details>
                            </div>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
          <details>
            <summary>高级发布设置</summary>
            <p>默认使用主站配置的固定 Edge 发行版本。这里仅覆盖本次节点升级，不改变新节点安装版本。</p>
            <label className="pa-field">
              <span>地区发布镜像（repository@sha256:digest）</span>
              <input value={releaseImage} placeholder={setup.release_image} onChange={(event) => setReleaseImage(event.target.value)} />
            </label>
          </details>
        </div>
      )}

      {danger && (
        <Modal
          title={'吊销 ' + danger.name + ' 的节点身份'}
          danger
          onClose={() => setDanger(null)}
          footer={(
            <>
              <button className="pa-button" onClick={() => setDanger(null)} type="button">取消</button>
              <button
                className="pa-button pa-button--danger"
                disabled={busy}
                onClick={() => void action(async () => { await change(danger, 'revoked'); setDanger(null) })}
                type="button"
              >
                确认吊销
              </button>
            </>
          )}
        >
          <p>该节点将无法继续认证和上报。先排空连接并确认待回传数据已处理；重新启用需要创建新的节点身份。</p>
        </Modal>
      )}
    </section>
  )
}
