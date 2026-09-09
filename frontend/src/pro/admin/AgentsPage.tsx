import { useCallback, useEffect, useState } from 'react'
import { adminFetch } from '../../admin/api'
import { downloadConsoleCSV } from './csv'
import { ErrorBanner, Metric } from './ui'

interface Profile { user_id: string; email?: string; commission_percent: number; settle_threshold_usd: number; daily_code_limit: number; code_value_usd: number; grant_days: number; channel: string; status: string; source_code?: string; source_id?: string }
interface Settlement { id: string; agent_user_id?: string; email?: string; amount_usd: number; method: string; status: string; requested_at: string; review_note: string; payment_reference: string }
interface Flag { id: string; registration_id: string; reason: string; blocking?: boolean; dismissed_at: string | null; review_note: string; email?: string; minimum_seconds: number }
interface Code { id: string; code: string; face_value_usd: number; channel: string; expires_at: string; redeemed_at: string | null; voided_at: string | null }
interface Balance { earned_usd: number; reserved_usd: number; paid_usd: number; available_usd: number }
interface Rules { check_email: boolean; check_device: boolean; minimum_usage_seconds: number }
interface Summary { registered: number; via_code?: number; visits?: number; first_topup: number; revenue_12_month_usd: number; hours: number }
interface AgentPortal { profile?: Profile[]; codes?: Code[]; settlements?: Settlement[]; flags?: Flag[]; balance?: Balance; summary?: Summary[]; retention?: { week: number; eligible: number; retained: number }[] }

const blankProfile: Profile = { user_id: '', commission_percent: 10, settle_threshold_usd: 100, daily_code_limit: 100, code_value_usd: 10, grant_days: 30, channel: '', status: 'active' }
const blankBalance: Balance = { earned_usd: 0, reserved_usd: 0, paid_usd: 0, available_usd: 0 }
const money = (amount: number | undefined) => `$${(amount ?? 0).toFixed(2)}`
const isoDate = (offsetDays: number) => new Date(Date.now() + offsetDays * 86400000).toISOString().slice(0, 10)
const stateLabel: Record<string, string> = { requested: '待审核', approved: '已通过', paid: '已结算', rejected: '已驳回' }
const reasonLabel: Record<string, string> = { self_email: '自身账户或相同邮箱身份', shared_device: '与代理使用相同设备', minimum_usage: '尚未达到最低使用时长' }
const profileFields: ReadonlyArray<readonly [keyof Profile & ('commission_percent' | 'settle_threshold_usd' | 'daily_code_limit' | 'code_value_usd' | 'grant_days'), string, number, number]> = [
  ['commission_percent', '分成百分比', 0, 100],
  ['settle_threshold_usd', '现金结算门槛 USD', 0, 1000000],
  ['daily_code_limit', '每日发码限额', 0, 1000],
  ['code_value_usd', '每码面值 USD', 0.01, 10000],
  ['grant_days', '兑换后有效天数', 1, 3650],
]

function codeState(code: Code): string {
  if (code.redeemed_at) return '已使用'
  if (code.voided_at) return '已作废'
  if (new Date(code.expires_at).getTime() <= Date.now()) return '已过期'
  return '可兑换'
}

function EmptyRow({ columns, text }: { columns: number; text: string }) {
  return <tr><td className="pa-table-empty" colSpan={columns}>{text}</td></tr>
}

export function AgentsPage({ mode, writable = false, canReview = false, canPay = false, canExport = false }: { mode: 'admin' | 'self'; writable?: boolean; canReview?: boolean; canPay?: boolean; canExport?: boolean }) {
  const [profiles, setProfiles] = useState<Profile[]>([])
  const [draft, setDraft] = useState<Profile>(blankProfile)
  const [settlements, setSettlements] = useState<Settlement[]>([])
  const [flags, setFlags] = useState<Flag[]>([])
  const [portal, setPortal] = useState<AgentPortal | null>(null)
  const [rules, setRules] = useState<Rules>({ check_email: true, check_device: true, minimum_usage_seconds: 600 })
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [quantity, setQuantity] = useState(10)
  const [expiry, setExpiry] = useState(() => isoDate(30))
  const [requestId, setRequestId] = useState(() => crypto.randomUUID())
  const [settlementRequest, setSettlementRequest] = useState(() => crypto.randomUUID())
  const [generated, setGenerated] = useState<string[]>([])

  const load = useCallback(async () => {
    try {
      if (mode === 'self') {
        const value = await adminFetch<AgentPortal>('/api/agent/portal')
        setPortal(value)
        setSettlements(value.settlements ?? [])
        setFlags(value.flags ?? [])
      } else {
        const [agents, requests, fraud] = await Promise.all([
          adminFetch<{ agents?: Profile[] }>('/api/admin/agents'),
          adminFetch<{ settlements?: Settlement[] }>('/api/admin/settlements'),
          adminFetch<{ rules?: Rules[]; flags?: Flag[] }>('/api/admin/agent-fraud'),
        ])
        setProfiles(agents.agents ?? [])
        setSettlements(requests.settlements ?? [])
        setFlags(fraud.flags ?? [])
        if (fraud.rules?.[0]) setRules(fraud.rules[0])
      }
      setError('')
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取失败')
    }
  }, [mode])
  useEffect(() => { void load() }, [load])

  async function mutate(path: string, method: string, body: unknown, onSuccess?: (result: unknown) => void) {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const result = await adminFetch(path, { method, body: JSON.stringify(body) })
      onSuccess?.(result)
      setNotice('操作已保存')
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  function review(item: Settlement, action: 'approve' | 'reject' | 'pay') {
    const reference = action === 'pay' && item.method === 'cash' ? window.prompt(`确认已向 ${item.email ?? item.agent_user_id} 付款 ${money(item.amount_usd)}。请输入付款凭据编号：`) : ''
    if (reference == null) return
    const note = action === 'reject' ? window.prompt('驳回原因：') : ''
    if (note == null) return
    void mutate(`/api/admin/settlements/${item.id}/${action}`, 'POST', { note, payment_reference: reference })
  }

  const profile = portal?.profile?.[0]
  const balance = portal?.balance ?? blankBalance
  const portalCodes = portal?.codes ?? []
  const summary = portal?.summary?.[0]
  const retention = portal?.retention ?? []
  const settlementColumns = mode === 'admin' ? 6 : 5

  return (
    <div className="pa-stack">
      <ErrorBanner message={error} onClose={() => setError('')} />
      {notice && <div className="pa-banner pa-banner--success" role="status">{notice}</div>}

      {mode === 'self' && portal && (
        <>
          {!profile ? (
            <section className="pa-card pa-section">
              <h2>账户概况</h2>
              <p>尚未配置代理资料，请联系管理员完成额度和分成设置。</p>
            </section>
          ) : (
            <>
              <div className="pa-metrics">
                <Metric label="可申请结算" value={money(balance.available_usd)} hint={`审核中 ${money(balance.reserved_usd)}`} />
                <Metric label="已结算" value={money(balance.paid_usd)} hint={`累计分成（扣退款）${money(balance.earned_usd)}`} />
                <Metric label="归因用户" value={summary?.registered ?? 0} hint={`链接访问 ${summary?.visits ?? 0} · 凭码 ${summary?.via_code ?? 0} · 首充 ${summary?.first_topup ?? 0} 人`} />
                <Metric label="归因 12 个月收入" value={money(summary?.revenue_12_month_usd)} hint={`累计使用 ${(summary?.hours ?? 0).toFixed(2)} 小时`} />
              </div>
              <section className="pa-card pa-section">
                <div className="pa-section__heading">
                  <div>
                    <h2>账户概况</h2>
                    <p>渠道 {profile.channel} · 分成 {profile.commission_percent}% · {profile.status === 'active' ? '正常' : '已暂停'}</p>
                  </div>
                  <button
                    className="pa-button pa-button--primary"
                    disabled={busy || balance.available_usd < 0.01 || profile.status !== 'active'}
                    type="button"
                    onClick={() => { void mutate('/api/agent/settlements', 'POST', { client_request_id: settlementRequest }, () => setSettlementRequest(crypto.randomUUID())) }}
                  >
                    申请结算全部可结金额
                  </button>
                </div>
                <p className="pa-form-note">通过你的链接注册或兑换你发的码的账户都归因给你；每笔真实充值冻结当时比例，归因用户注册后的 12 个月内有效。未满足风控条件的分成暂不可结。低于 {money(profile.settle_threshold_usd)} 以一年有效的赠送额度结算，达到门槛以现金结算。</p>
                {profile.source_code && (
                  <div className="pa-subsection">
                    <h3>我的邀请链接</h3>
                    <p className="pa-form-note">通过这个链接注册的用户会获得 {money(profile.code_value_usd)} 赠送额度（有效 {profile.grant_days} 天），与兑换码相同，并归因给你。链接注册占用每日限额，当天满额后链接自动关闭、次日恢复。落地页自带二维码和海报。</p>
                    <div className="pa-toolbar">
                      <input aria-label="代理邀请链接" readOnly value={`${window.location.origin}/invite?code=${profile.source_code}`} onFocus={e => e.target.select()} />
                      <button className="pa-button" type="button" onClick={() => { void navigator.clipboard.writeText(`${window.location.origin}/invite?code=${profile.source_code}`); setNotice('链接已复制') }}>复制链接</button>
                      <a className="pa-button" href={`/invite?code=${profile.source_code}`} rel="noopener" target="_blank">打开落地页 / 海报</a>
                    </div>
                  </div>
                )}
                {retention.length > 0 && (
                  <div className="pa-footnotes">
                    {retention.map(row => <span key={row.week}>第 {row.week} 周留存 {row.retained} / {row.eligible}</span>)}
                  </div>
                )}
              </section>
              <section className="pa-card pa-section">
                <div className="pa-section__heading">
                  <div>
                    <h2>生成我的一次性兑换码</h2>
                    <p>给没有通过链接注册的用户线下发码：每码 {money(profile.code_value_usd)}，兑换后有效 {profile.grant_days} 天，截止日最多 90 天。每日限额 {profile.daily_code_limit} 个由链接注册和发码合计占用，满额后当天链接关闭。已经通过活动或推荐归因过的账户不能再兑换。客户价格、赠送额度使用规则及条款与普通客户一致。</p>
                  </div>
                </div>
                <form
                  className="pa-form-grid"
                  onSubmit={e => { e.preventDefault(); void mutate('/api/agent/codes', 'POST', { client_request_id: requestId, quantity, expires_at: `${expiry}T23:59:59Z` }, result => { setGenerated((result as { codes?: string[] }).codes ?? []); setRequestId(crypto.randomUUID()) }) }}
                >
                  <label><span>数量</span><input type="number" min="1" max="1000" value={quantity} onChange={e => setQuantity(Number(e.target.value))} required /></label>
                  <label><span>兑换截止日（UTC，最多 90 天）</span><input type="date" max={isoDate(90)} value={expiry} onChange={e => setExpiry(e.target.value)} required /></label>
                  <button className="pa-button pa-button--primary" disabled={busy || profile.status !== 'active'} type="submit">生成</button>
                </form>
                {generated.length > 0 && (
                  <div className="pa-form-result">
                    <div className="pa-toolbar">
                      <p role="status">已生成 {generated.length} 个兑换码</p>
                      <button className="pa-button" type="button" onClick={() => downloadConsoleCSV('my-new-codes.csv', [['兑换码'], ...generated.map(code => [code])])}>下载本批 CSV</button>
                    </div>
                    <textarea aria-label="代理新生成兑换码" readOnly value={generated.join('\n')} rows={Math.min(8, generated.length)} />
                  </div>
                )}
                <div className="pa-list-heading pa-subsection">
                  <h3>我的兑换码</h3>
                  <div className="pa-toolbar">
                    <span className="pa-count">最近 1000 个</span>
                    <button className="pa-button" disabled={portalCodes.length === 0} type="button" onClick={() => downloadConsoleCSV('my-codes.csv', [['兑换码', '面值 USD', '截止日期', '兑换时间'], ...portalCodes.map(code => [code.code, code.face_value_usd, code.expires_at, code.redeemed_at])])}>下载我的码 CSV</button>
                  </div>
                </div>
                <div className="pa-table-wrap">
                  <table className="pa-table">
                    <thead><tr><th>兑换码</th><th>面值</th><th>截止日期</th><th>状态</th></tr></thead>
                    <tbody>
                      {portalCodes.length === 0 && <EmptyRow columns={4} text="还没有生成兑换码" />}
                      {portalCodes.map(code => (
                        <tr key={code.id}>
                          <td><code>{code.code}</code></td>
                          <td>{money(code.face_value_usd)}</td>
                          <td>{code.expires_at.slice(0, 10)}</td>
                          <td>{codeState(code)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </section>
            </>
          )}
        </>
      )}

      {mode === 'admin' && (
        <>
          <section className="pa-card pa-section">
            <div className="pa-section__heading">
              <div>
                <h2>代理配置</h2>
                <p>保存后会为代理生成自己的邀请来源（链接和兑换码共用同一套赠送条件），可在「推广邀请」列表查看。分成比例的修改只影响之后的充值。配置完成后，在角色权限页为账户分配「代理」角色。</p>
              </div>
              {writable && <button className="pa-button" type="button" onClick={() => setDraft(blankProfile)}>新建配置</button>}
            </div>
            <div className="pa-table-wrap">
              <table className="pa-table">
                <thead><tr><th>账户</th><th>渠道</th><th>比例</th><th>现金门槛</th><th>状态</th><th>操作</th></tr></thead>
                <tbody>
                  {profiles.length === 0 && <EmptyRow columns={6} text="还没有代理" />}
                  {profiles.map(item => (
                    <tr key={item.user_id}>
                      <td>{item.email || item.user_id}</td>
                      <td>{item.channel}</td>
                      <td>{item.commission_percent}%</td>
                      <td>{money(item.settle_threshold_usd)}</td>
                      <td>{item.status === 'active' ? '正常' : '已暂停'}</td>
                      <td>{writable && <button className="pa-button pa-button--quiet" type="button" onClick={() => setDraft(item)}>编辑</button>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {writable && (
              <form className="pa-subsection" onSubmit={e => { e.preventDefault(); void mutate('/api/admin/agents', 'PUT', draft) }}>
                <fieldset className="pa-permission-scope" disabled={busy}>
                  <h3>{profiles.some(item => item.user_id === draft.user_id && draft.user_id) ? '编辑代理' : '新建代理'}</h3>
                  <div className="pa-form-grid">
                    <label><span>用户邮箱或 ID</span><input required placeholder="name@example.com" value={draft.user_id} onChange={e => setDraft({ ...draft, user_id: e.target.value })} /></label>
                    <label><span>来源渠道</span><input required maxLength={100} value={draft.channel} onChange={e => setDraft({ ...draft, channel: e.target.value })} /></label>
                    {profileFields.map(([key, label, min, max]) => (
                      <label key={key}>
                        <span>{label}</span>
                        <input type="number" required min={min} max={max} step={key.endsWith('days') || key.endsWith('limit') ? 1 : 0.01} value={draft[key]} onChange={e => setDraft({ ...draft, [key]: Number(e.target.value) })} />
                      </label>
                    ))}
                    <label>
                      <span>状态</span>
                      <select value={draft.status} onChange={e => setDraft({ ...draft, status: e.target.value })}>
                        <option value="active">正常</option>
                        <option value="suspended">暂停发码与结算</option>
                      </select>
                    </label>
                  </div>
                  <div className="pa-toolbar pa-button-row">
                    <button className="pa-button pa-button--primary" type="submit">保存代理配置</button>
                  </div>
                </fieldset>
              </form>
            )}
          </section>

          <section className="pa-card pa-section">
            <div className="pa-section__heading">
              <div>
                <h2>代理刷号规则</h2>
                <p>规则在领码时快照，不影响客户权益。最低使用时长达到后自动允许结算；误判可填写理由解除。</p>
              </div>
            </div>
            <form onSubmit={e => { e.preventDefault(); void mutate('/api/admin/agent-fraud', 'PUT', rules) }}>
              <fieldset className="pa-permission-scope" disabled={!writable || busy}>
                <div className="pa-form-grid">
                  <label className="pa-checkbox"><input type="checkbox" checked={rules.check_email} onChange={e => setRules({ ...rules, check_email: e.target.checked })} />检测相同邮箱身份</label>
                  <label className="pa-checkbox"><input type="checkbox" checked={rules.check_device} onChange={e => setRules({ ...rules, check_device: e.target.checked })} />检测相同设备</label>
                  <label><span>最低转录秒数</span><input type="number" min="0" max="86400" value={rules.minimum_usage_seconds} onChange={e => setRules({ ...rules, minimum_usage_seconds: Number(e.target.value) })} /></label>
                </div>
                <div className="pa-toolbar pa-button-row">
                  <button className="pa-button pa-button--primary" type="submit">保存规则</button>
                </div>
              </fieldset>
            </form>
          </section>
        </>
      )}

      <section className="pa-card pa-section">
        <div className="pa-section__heading">
          <div>
            <h2>结算记录</h2>
            <p>低于现金门槛的申请以赠送额度结算，达到门槛后以现金结算。</p>
          </div>
          {canExport && (
            <button className="pa-button" disabled={settlements.length === 0} type="button" onClick={() => downloadConsoleCSV('agent-settlements.csv', [['ID', '账户', '金额 USD', '方式', '状态', '申请时间', '凭据'], ...settlements.map(item => [item.id, item.email, item.amount_usd, item.method, item.status, item.requested_at, item.payment_reference])])}>下载 CSV</button>
          )}
        </div>
        <div className="pa-table-wrap">
          <table className="pa-table">
            <thead>
              <tr>
                <th>申请时间 / ID</th>
                {mode === 'admin' && <th>账户</th>}
                <th>金额</th>
                <th>方式</th>
                <th>状态 / 说明</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {settlements.length === 0 && <EmptyRow columns={settlementColumns} text="还没有结算申请" />}
              {settlements.map(item => (
                <tr key={item.id}>
                  <td>{item.requested_at.slice(0, 10)}<small className="pa-table-sub">{item.id}</small></td>
                  {mode === 'admin' && <td>{item.email || item.agent_user_id}</td>}
                  <td>{money(item.amount_usd)}</td>
                  <td>{item.method === 'credit' ? '赠送额度' : '现金'}</td>
                  <td>
                    {stateLabel[item.status] ?? item.status}
                    {(item.review_note || item.payment_reference) && <small className="pa-table-sub">{[item.review_note, item.payment_reference].filter(Boolean).join(' · ')}</small>}
                  </td>
                  <td>
                    <div className="pa-toolbar">
                      {canReview && item.status === 'requested' && <button className="pa-button pa-button--quiet" disabled={busy} type="button" onClick={() => review(item, 'approve')}>审核通过</button>}
                      {canReview && ['requested', 'approved'].includes(item.status) && <button className="pa-button pa-button--danger-quiet" disabled={busy} type="button" onClick={() => review(item, 'reject')}>驳回</button>}
                      {canPay && item.status === 'approved' && <button className="pa-button pa-button--primary" disabled={busy} type="button" onClick={() => review(item, 'pay')}>{item.method === 'credit' ? '发放额度' : '登记已付款'}</button>}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="pa-card pa-section">
        <div className="pa-section__heading">
          <div>
            <h2>分成风控原因</h2>
            <p>被标记的分成在条件满足或人工解除前不可结算。</p>
          </div>
        </div>
        <div className="pa-table-wrap">
          <table className="pa-table">
            <thead><tr><th>归因账户</th><th>原因</th><th>状态 / 说明</th><th>操作</th></tr></thead>
            <tbody>
              {flags.length === 0 && <EmptyRow columns={4} text="没有被标记的分成" />}
              {flags.map(flag => (
                <tr key={flag.id}>
                  <td>{flag.email || <code>{flag.registration_id}</code>}</td>
                  <td>{reasonLabel[flag.reason] ?? flag.reason}{flag.reason === 'minimum_usage' ? `（${flag.minimum_seconds} 秒）` : ''}</td>
                  <td>
                    {flag.dismissed_at ? '已人工解除' : flag.blocking === false ? '已达到条件' : '待满足条件或审核'}
                    {flag.review_note && <small className="pa-table-sub">{flag.review_note}</small>}
                  </td>
                  <td>{writable && !flag.dismissed_at && <button className="pa-button pa-button--quiet" disabled={busy} type="button" onClick={() => { const note = window.prompt('请输入解除此标记的依据：'); if (note) void mutate('/api/admin/agent-fraud', 'PUT', { flag_id: flag.id, note }) }}>解除标记</button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  )
}
