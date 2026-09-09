import { authFetch } from '../api/auth'
import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { adminFetch, formatUSD, type Plan } from '../../admin/api'
import { formatDate, type Runner } from './shared'
import { Modal, Pagination } from './ui'

interface Promotion {
  id: string
  code: string
  name: string
  channel: string
  tags: string[]
  enabled: boolean
  expires_at: string
  max_registrations: number
  grant_usd: number
  grant_days: number
  plan_code: string
  plan_days: number
  headline: string
  description: string
  usage_discount_percent: number
  discount_days: number
  topup_bonus_percent: number
  topup_bonus_days: number
  milestone_session_usd: number
  milestone_topup_usd: number
  registrations: number
  verified: number
  rewarded: number
  visits: number
  paid: number
  revenue_usd: number
}
interface Registration {
  id: string
  user_id: string
  email: string
  name: string
  verified: boolean
  registered_at: string
  rewarded_at: string | null
  plan_until: string | null
  discount_until: string | null
  topup_rewarded_at: string | null
  session_rewarded_at: string | null
  paid_usd: number
}
interface Funnel {
  invite: Promotion
  sources: Array<{ source: string; medium: string; campaign: string; content: string; visits: number }>
  daily: Array<{ day: string; visits: number; registrations: number }>
}
interface Referrer {
  user_id: string
  email: string
  name: string
  code: string
  visits: number
  registered: number
  verified: number
  last_registered_at: string
}
interface ListResult { invites: Promotion[]; total: number }
interface RecipientsResult { registrations: Registration[]; total: number }
interface ReferrersResult { referrers: Referrer[]; total: number }

function newDraft() {
  return {
    name: '', channel: '', tags: '', code: '', expires_at: '', max_registrations: '100',
    grant_usd: '0', grant_days: '30', plan_code: '', plan_days: '30',
    headline: '', description: '',
    usage_discount_percent: '0', discount_days: '30', topup_bonus_percent: '0', topup_bonus_days: '30',
    milestone_session_usd: '0', milestone_topup_usd: '0',
  }
}
interface UTM { source: string; medium: string; campaign: string; content: string }
const emptyUTM: UTM = { source: '', medium: '', campaign: '', content: '' }

/** The shareable landing page for a channel code, with optional UTM tags. */
function inviteLink(code: string, utm: UTM = emptyUTM) {
  const url = new URL('/invite', window.location.origin)
  url.searchParams.set('code', code)
  if (utm.source.trim()) url.searchParams.set('utm_source', utm.source.trim())
  if (utm.medium.trim()) url.searchParams.set('utm_medium', utm.medium.trim())
  if (utm.campaign.trim()) url.searchParams.set('utm_campaign', utm.campaign.trim())
  if (utm.content.trim()) url.searchParams.set('utm_content', utm.content.trim())
  return url.href
}
function stateLabel(p: Promotion) {
  if (!p.enabled) return '已暂停'
  if (Date.parse(p.expires_at) <= Date.now()) return '已过期'
  if (p.registrations >= p.max_registrations) return '已满'
  return '启用中'
}
function rewardLines(p: Promotion): string[] {
  const lines: string[] = []
  if (p.grant_usd > 0) lines.push(`${formatUSD(p.grant_usd)} / ${p.grant_days} 天`)
  if (p.plan_code) lines.push(`${p.plan_code} / ${p.plan_days} 天`)
  if (p.usage_discount_percent > 0) lines.push(`转录 -${p.usage_discount_percent}% / ${p.discount_days} 天`)
  if (p.topup_bonus_percent > 0) lines.push(`首充 +${p.topup_bonus_percent}% / ${p.topup_bonus_days} 天内`)
  if (p.milestone_topup_usd > 0) lines.push(`首充加送 ${formatUSD(p.milestone_topup_usd)}`)
  if (p.milestone_session_usd > 0) lines.push(`首次转录送 ${formatUSD(p.milestone_session_usd)}`)
  return lines
}
function rate(numerator: number, denominator: number) {
  if (denominator <= 0) return '—'
  return `${Math.round((numerator / denominator) * 1000) / 10}%`
}

export function PromotionsPage({ run, scoped = false }: { run: Runner; scoped?: boolean }) {
  const [result, setResult] = useState<ListResult>({ invites: [], total: 0 })
  const [page, setPage] = useState(1)
  const [search, setSearch] = useState('')
  const [query, setQuery] = useState('')
  const [generation, setGeneration] = useState(0)
  const [creating, setCreating] = useState(false)
  const [busy, setBusy] = useState(false)
  const [plans, setPlans] = useState<Plan[]>([])
  const [draft, setDraft] = useState(newDraft)
  const [selected, setSelected] = useState<Promotion | null>(null)
  const [recipientPage, setRecipientPage] = useState(1)
  const [recipients, setRecipients] = useState<RecipientsResult>({ registrations: [], total: 0 })
  const [funnelFor, setFunnelFor] = useState<Promotion | null>(null)
  const [funnel, setFunnel] = useState<Funnel | null>(null)
  const [editing, setEditing] = useState<Promotion | null>(null)
  const [copyDraft, setCopyDraft] = useState({ headline: '', description: '' })
  const [created, setCreated] = useState<Promotion | null>(null)
  // The same panel is reused by 复制链接 on an existing row, where the link is
  // not a newly created one.
  const [createdIsNew, setCreatedIsNew] = useState(false)
  const [utm, setUtm] = useState<UTM>(emptyUTM)
  const [referrers, setReferrers] = useState<ReferrersResult>({ referrers: [], total: 0 })
  const [referrerPage, setReferrerPage] = useState(1)
  const reload = useCallback(() => setGeneration((n) => n + 1), [])

  useEffect(() => {
    let current = true
    const params = new URLSearchParams({ page: String(page), search: query })
    void run(() => adminFetch<ListResult>(`/api/admin/promotions?${params}`)).then((data) => { if (current && data) setResult(data) })
    return () => { current = false }
  }, [page, query, generation, run])
  useEffect(() => {
    let current = true
    void run(() => authFetch<{ plans: Plan[] }>('/api/user/billing/plans')).then((data) => { if (current && data) setPlans(data.plans) })
    return () => { current = false }
  }, [run])
  useEffect(() => {
    if (!selected) return
    let current = true
    void run(() => adminFetch<RecipientsResult>(`/api/admin/promotions/${selected.id}?page=${recipientPage}`)).then((data) => { if (current && data) setRecipients(data) })
    return () => { current = false }
  }, [selected, recipientPage, run])
  useEffect(() => {
    if (!funnelFor) { setFunnel(null); return }
    let current = true
    void run(() => adminFetch<Funnel>(`/api/admin/promotions/${funnelFor.id}/funnel`)).then((data) => { if (current && data) setFunnel(data) })
    return () => { current = false }
  }, [funnelFor, run])
  useEffect(() => {
    let current = true
    if (scoped) return
    const params = new URLSearchParams({ page: String(referrerPage), search: query })
    void run(() => adminFetch<ReferrersResult>(`/api/admin/referrals?${params}`)).then((data) => { if (current && data) setReferrers(data) })
    return () => { current = false }
  }, [referrerPage, query, generation, run, scoped])

  async function create(event: FormEvent) {
    event.preventDefault()
    if (busy) return
    setBusy(true)
    const saved = await run(() => adminFetch<Promotion>('/api/admin/promotions', {
      method: 'POST', body: JSON.stringify({ ...draft,
        tags: draft.tags.split(/[,，]/).map((tag) => tag.trim()).filter(Boolean),
        expires_at: new Date(draft.expires_at).toISOString(),
        max_registrations: Number(draft.max_registrations), grant_usd: Number(draft.grant_usd),
        grant_days: Number(draft.grant_days), plan_days: Number(draft.plan_days),
        usage_discount_percent: Number(draft.usage_discount_percent), discount_days: Number(draft.discount_days),
        topup_bonus_percent: Number(draft.topup_bonus_percent), topup_bonus_days: Number(draft.topup_bonus_days),
        milestone_session_usd: Number(draft.milestone_session_usd), milestone_topup_usd: Number(draft.milestone_topup_usd),
      }),
    }), '推广邀请已创建')
    setBusy(false)
    if (saved) { setCreating(false); setCreated(saved); setCreatedIsNew(true); setUtm(emptyUTM); reload() }
  }

  async function saveCopy(event: FormEvent) {
    event.preventDefault()
    if (!editing || busy) return
    setBusy(true)
    const saved = await run(() => adminFetch(`/api/admin/promotions/${editing.id}`, { method: 'PATCH', body: JSON.stringify(copyDraft) }), '落地页文案已更新')
    setBusy(false)
    if (saved) { setEditing(null); reload() }
  }

  const draftReward = Number(draft.grant_usd) * Number(draft.max_registrations)
    + Number(draft.milestone_session_usd) * Number(draft.max_registrations)
    + Number(draft.milestone_topup_usd) * Number(draft.max_registrations)

  return <div className="pa-stack">
    <section className="pa-card pa-promotion-intro">
      <div className="pa-list-heading"><div><h2>渠道活动</h2><p>用独立邀请链接追踪来源，为新用户提供活动权益。链接指向带二维码和倒计时的落地页。</p></div><span className="pa-count">{result.total} 个匹配活动</span></div>
      <p className="pa-form-note pa-promotion-help">成功注册即占用名额，赠送需通过邮箱验证与风控审核。暂停或到期不影响已接受的邀请。折扣与首充/首次转录奖励在领取注册权益后按各自窗口生效。</p>
      <div className="pa-promotion-actions">
        <form onSubmit={(event) => { event.preventDefault(); setQuery(search.trim()); setPage(1); setReferrerPage(1) }}>
          <input aria-label="搜索活动、渠道、标签或邀请码" onChange={(event) => setSearch(event.target.value)} placeholder="活动、渠道、标签或邀请码" value={search} />
          <button className="pa-button" type="submit">搜索</button>
        </form>
        <button className="pa-button pa-button--primary" onClick={() => { setDraft(newDraft()); setCreating(true) }} type="button">创建推广邀请</button>
        <button className="pa-button" onClick={reload} type="button">刷新</button>
      </div>
    </section>
    {created && <section className="pa-card pa-promotion-intro" role="status">
      <strong>{created.name} · {created.channel}</strong>
      <p>邀请码：{created.code}</p>
      <div className="pa-promotion-actions pa-promotion-utm">
        <input aria-label="utm_source" onChange={(event) => setUtm({ ...utm, source: event.target.value })} placeholder="utm_source（如 xiaohongshu）" value={utm.source} />
        <input aria-label="utm_medium" onChange={(event) => setUtm({ ...utm, medium: event.target.value })} placeholder="utm_medium（如 poster）" value={utm.medium} />
        <input aria-label="utm_campaign" onChange={(event) => setUtm({ ...utm, campaign: event.target.value })} placeholder="utm_campaign" value={utm.campaign} />
        <input aria-label="utm_content" onChange={(event) => setUtm({ ...utm, content: event.target.value })} placeholder="utm_content（如 博主A）" value={utm.content} />
      </div>
      <input aria-label={createdIsNew ? '新建邀请链接' : `${created.name} 邀请链接`} readOnly value={inviteLink(created.code, utm)} onFocus={(event) => event.target.select()} />
      <div className="pa-promotion-actions">
        <button className="pa-button" onClick={() => { void run(() => navigator.clipboard.writeText(inviteLink(created.code, utm)), '链接已复制') }} type="button">复制邀请链接</button>
        <a className="pa-button" href={inviteLink(created.code, utm)} rel="noopener" target="_blank">打开落地页 / 海报</a>
      </div>
      <small className="pa-form-note">UTM 参数只用于统计来源，落地页访问会记入该活动的漏斗；同一访客一天只计一次。</small>
    </section>}
    <section className="pa-card">
      <div className="pa-table-wrap"><table className="pa-table"><thead><tr>
        <th>活动 / 渠道 / 标签</th><th>赠送权益</th><th>访问 → 注册 → 验证 → 领取 → 付费</th><th>状态 / 截止时间</th><th>操作</th>
      </tr></thead><tbody>
        {result.invites.map((p) => <tr key={p.id}>
          <td><strong>{p.name}</strong><div>{p.channel}</div><div className="pa-tag-list">{p.tags.map((tag) => <span className="pa-tag" key={tag}>{tag}</span>)}</div><small><code>{p.code}</code></small></td>
          <td>{rewardLines(p).map((line) => <div key={line}>{line}</div>)}{rewardLines(p).length === 0 && '仅渠道归因'}</td>
          <td><strong className="pa-tabular">{p.visits} → {p.registrations} → {p.verified} → {p.rewarded} → {p.paid}</strong><small>注册上限 {p.max_registrations} 人 · 访问转化 {rate(p.registrations, p.visits)} · 付费转化 {rate(p.paid, p.verified)} · 收入 {formatUSD(p.revenue_usd)}</small><progress className="pa-progress" aria-label={`${p.name} 注册名额使用情况`} value={p.registrations} max={p.max_registrations} /></td>
          <td><span className={`pa-status ${stateLabel(p) === '启用中' ? 'pa-status--good' : ''}`}>{stateLabel(p)}</span><small>{formatDate(p.expires_at)}</small></td>
          <td><div className="pa-promotion-actions">
            <button className="pa-button" type="button" onClick={() => { setCreated(p); setCreatedIsNew(false); setUtm(emptyUTM); void run(() => navigator.clipboard.writeText(inviteLink(p.code)), '链接已复制') }}>复制链接</button>
            <button className="pa-button" type="button" onClick={() => setFunnelFor(p)}>漏斗</button>
            <button className="pa-button" type="button" onClick={() => { setRecipientPage(1); setRecipients({ registrations: [], total: 0 }); setSelected(p) }}>注册记录</button>
            <button className="pa-button" type="button" onClick={() => { setCopyDraft({ headline: p.headline, description: p.description }); setEditing(p) }}>文案</button>
            <button className="pa-button" disabled={busy} type="button" onClick={() => {
              setBusy(true)
              void run(() => adminFetch(`/api/admin/promotions/${p.id}`, { method: 'PATCH', body: JSON.stringify({ enabled: !p.enabled }) }), p.enabled ? '已暂停新注册' : '已启用').then(() => { setBusy(false); reload() })
            }}>{p.enabled ? '暂停' : '启用'}</button>
          </div></td>
        </tr>)}
        {result.invites.length === 0 && <tr><td colSpan={5} className="pa-table-empty">暂无匹配的推广邀请</td></tr>}
      </tbody></table></div>
      <Pagination page={page} pageSize={20} total={result.total} onChange={setPage} />
    </section>
    {!scoped && <section className="pa-card">
      <div className="pa-list-heading"><div><h2>用户推荐</h2><p>每个账户都有自己的邀请链接（/invite?ref=CODE）。这里只记录来源，不发放奖励。</p></div><span className="pa-count">{referrers.total} 位推荐人</span></div>
      <div className="pa-table-wrap"><table className="pa-table"><thead><tr><th>推荐人</th><th>推荐码</th><th>访问 / 注册 / 已验证</th><th>最近一次注册</th></tr></thead><tbody>
        {referrers.referrers.map((r) => <tr key={r.user_id}><td><strong>{r.name || r.email}</strong><small>{r.email}</small></td><td><code>{r.code}</code></td><td className="pa-tabular">{r.visits} / {r.registered} / {r.verified}</td><td>{formatDate(r.last_registered_at)}</td></tr>)}
        {referrers.referrers.length === 0 && <tr><td colSpan={4} className="pa-table-empty">还没有用户通过推荐链接带来注册</td></tr>}
      </tbody></table></div>
      <Pagination page={referrerPage} pageSize={20} total={referrers.total} onChange={setReferrerPage} />
    </section>}
    {creating && <Modal wide title="创建推广邀请" onClose={() => { if (!busy) setCreating(false) }} footer={<button className="pa-button pa-button--primary" disabled={busy} form="create-promotion" type="submit">{busy ? '创建中…' : '创建邀请'}</button>}>
      <form className="pa-dialog-form pa-promotion-form" id="create-promotion" onSubmit={(event) => { void create(event) }}>
        <label><span>活动名称</span><input required maxLength={100} value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="2026 开学季" /></label>
        <label><span>渠道</span><input required maxLength={100} value={draft.channel} onChange={(event) => setDraft({ ...draft, channel: event.target.value })} placeholder="小红书 / 博主 A" /></label>
        <label><span>用户来源标签</span><input value={draft.tags} onChange={(event) => setDraft({ ...draft, tags: event.target.value })} placeholder="开学季, 小红书, 博主A" /><small>逗号分隔，最多 20 个；注册来源固定保留。</small></label>
        <label><span>邀请码（留空自动生成）</span><input maxLength={48} minLength={6} pattern="[A-Za-z0-9][A-Za-z0-9_-]{5,47}" value={draft.code} onChange={(event) => setDraft({ ...draft, code: event.target.value })} placeholder="XHS2026A" /></label>
        <label><span>注册截止时间</span><input required type="datetime-local" value={draft.expires_at} onChange={(event) => setDraft({ ...draft, expires_at: event.target.value })} /></label>
        <label><span>最多注册人数</span><input required type="number" min={1} max={1000000} value={draft.max_registrations} onChange={(event) => setDraft({ ...draft, max_registrations: event.target.value })} /></label>
        <label><span>落地页标题</span><input maxLength={120} value={draft.headline} onChange={(event) => setDraft({ ...draft, headline: event.target.value })} placeholder="开学季专属：注册即享 Pro 30 天" /><small>留空时使用活动名称；创建后可修改。</small></label>
        <label className="pa-full-width"><span>落地页说明</span><textarea maxLength={600} rows={3} value={draft.description} onChange={(event) => setDraft({ ...draft, description: event.target.value })} placeholder="面向新生的限时活动，注册并验证邮箱后自动到账。" /></label>
        <label><span>额外活动余额（USD）</span><input required type="number" min={0} max={10000} step="0.01" value={draft.grant_usd} onChange={(event) => setDraft({ ...draft, grant_usd: event.target.value })} /></label>
        <label><span>余额有效天数（从领取起）</span><input required type="number" min={1} max={3650} value={draft.grant_days} onChange={(event) => setDraft({ ...draft, grant_days: event.target.value })} /><small>也用于首充、首次转录加送余额的有效期。</small></label>
        <label><span>赠送套餐</span><select aria-label="赠送套餐" value={draft.plan_code} onChange={(event) => setDraft({ ...draft, plan_code: event.target.value })}><option value="">不赠送套餐</option>{plans.filter((p) => p.active && p.code !== 'free').map((p) => <option key={p.code} value={p.code}>{p.name}</option>)}</select></label>
        {draft.plan_code && <label><span>套餐有效天数（从领取起）</span><input required type="number" min={1} max={3650} value={draft.plan_days} onChange={(event) => setDraft({ ...draft, plan_days: event.target.value })} /></label>}
        <label><span>实时转录折扣（%）</span><input required type="number" min={0} max={100} step="0.5" value={draft.usage_discount_percent} onChange={(event) => setDraft({ ...draft, usage_discount_percent: event.target.value })} /><small>在会员折扣与训练计划折扣之后再减，只作用于转录。</small></label>
        <label><span>折扣有效天数（从领取起）</span><input required type="number" min={1} max={3650} value={draft.discount_days} onChange={(event) => setDraft({ ...draft, discount_days: event.target.value })} /></label>
        <label><span>首次充值加赠（%）</span><input required type="number" min={0} max={200} step="1" value={draft.topup_bonus_percent} onChange={(event) => setDraft({ ...draft, topup_bonus_percent: event.target.value })} /><small>只对 Stripe 真实付款生效，与充值档位赠送叠加。</small></label>
        <label><span>首充窗口（领取后天数）</span><input required type="number" min={1} max={3650} value={draft.topup_bonus_days} onChange={(event) => setDraft({ ...draft, topup_bonus_days: event.target.value })} /></label>
        <label><span>首次充值加送余额（USD）</span><input required type="number" min={0} max={10000} step="0.01" value={draft.milestone_topup_usd} onChange={(event) => setDraft({ ...draft, milestone_topup_usd: event.target.value })} /></label>
        <label><span>首次转录加送余额（USD）</span><input required type="number" min={0} max={10000} step="0.01" value={draft.milestone_session_usd} onChange={(event) => setDraft({ ...draft, milestone_session_usd: event.target.value })} /><small>第一次产生转录扣费后到账。</small></label>
        <p className="pa-form-note pa-full-width pa-callout">固定赠送最多 {formatUSD(draftReward)} 活动余额{draft.plan_code ? `，以及 ${draft.max_registrations} 份 ${draft.plan_days} 天套餐` : ''}{Number(draft.topup_bonus_percent) > 0 ? `，首充加赠按实际充值金额计算` : ''}。余额额外叠加注册试用额度；套餐不含消费余额、不自动续费，付费套餐优先生效。活动创建后渠道和权益不可修改（落地页文案除外），如需调整请暂停并新建。</p>
      </form>
    </Modal>}
    {editing && <Modal title={`${editing.name} · 落地页文案`} onClose={() => { if (!busy) setEditing(null) }} footer={<button className="pa-button pa-button--primary" disabled={busy} form="edit-promotion-copy" type="submit">{busy ? '保存中…' : '保存文案'}</button>}>
      <form className="pa-dialog-form" id="edit-promotion-copy" onSubmit={(event) => { void saveCopy(event) }}>
        <label><span>落地页标题</span><input maxLength={120} value={copyDraft.headline} onChange={(event) => setCopyDraft({ ...copyDraft, headline: event.target.value })} placeholder={editing.name} /></label>
        <label><span>落地页说明</span><textarea maxLength={600} rows={4} value={copyDraft.description} onChange={(event) => setCopyDraft({ ...copyDraft, description: event.target.value })} /></label>
        <p className="pa-form-note">文案不是承诺的权益，可以随时修改；权益字段保持不变。</p>
      </form>
    </Modal>}
    {funnelFor && <Modal wide footer={null} title={`${funnelFor.name} · ${funnelFor.channel} · 转化漏斗`} onClose={() => setFunnelFor(null)}>
      {!funnel && <p>正在读取漏斗…</p>}
      {funnel && <>
        <div className="pa-metrics pa-promotion-funnel">
          <div className="pa-card pa-metric"><small>访问（去重/天）</small><strong>{funnel.invite.visits}</strong></div>
          <div className="pa-card pa-metric"><small>注册</small><strong>{funnel.invite.registrations}</strong><p>访问转化 {rate(funnel.invite.registrations, funnel.invite.visits)}</p></div>
          <div className="pa-card pa-metric"><small>验证 / 领取</small><strong>{funnel.invite.verified} / {funnel.invite.rewarded}</strong><p>验证率 {rate(funnel.invite.verified, funnel.invite.registrations)}</p></div>
          <div className="pa-card pa-metric"><small>付费用户 / 收入</small><strong>{funnel.invite.paid}</strong><p>{formatUSD(funnel.invite.revenue_usd)} · 付费转化 {rate(funnel.invite.paid, funnel.invite.verified)}</p></div>
        </div>
        <h3 className="pa-subtitle">来源（UTM）</h3>
        <div className="pa-table-wrap"><table className="pa-table"><thead><tr><th>utm_source</th><th>utm_medium</th><th>utm_campaign</th><th>utm_content</th><th>访问</th></tr></thead><tbody>
          {funnel.sources.map((s, index) => <tr key={index}><td>{s.source || '—'}</td><td>{s.medium || '—'}</td><td>{s.campaign || '—'}</td><td>{s.content || '—'}</td><td className="pa-tabular">{s.visits}</td></tr>)}
          {funnel.sources.length === 0 && <tr><td colSpan={5} className="pa-table-empty">还没有访问记录</td></tr>}
        </tbody></table></div>
        <h3 className="pa-subtitle">最近 30 天</h3>
        <div className="pa-table-wrap"><table className="pa-table"><thead><tr><th>日期（UTC）</th><th>访问</th><th>注册</th></tr></thead><tbody>
          {funnel.daily.filter((d) => d.visits > 0 || d.registrations > 0).map((d) => <tr key={d.day}><td>{d.day}</td><td className="pa-tabular">{d.visits}</td><td className="pa-tabular">{d.registrations}</td></tr>)}
          {funnel.daily.every((d) => d.visits === 0 && d.registrations === 0) && <tr><td colSpan={3} className="pa-table-empty">最近 30 天没有活动</td></tr>}
        </tbody></table></div>
      </>}
    </Modal>}
    {selected && <Modal wide footer={null} title={`${selected.name} · ${selected.channel} · 注册记录`} onClose={() => setSelected(null)}>
      <p>{selected.tags.join(' · ')}</p>
      <div className="pa-table-wrap"><table className="pa-table"><thead><tr><th>昵称 / 邮箱</th><th>注册时间</th><th>验证 / 权益</th><th>阶段奖励 / 付费</th></tr></thead><tbody>
        {recipients.registrations.map((r) => <tr key={r.id}><td>{r.user_id ? <>{r.name}<div>{r.email}</div></> : '账号已删除'}</td><td>{formatDate(r.registered_at)}</td><td>{r.verified ? '已验证' : '待验证'}<div>{r.rewarded_at ? `已领取 ${formatDate(r.rewarded_at)}` : '待领取'}</div>{r.plan_until && <small>套餐至 {formatDate(r.plan_until)}</small>}{r.discount_until && <small>折扣至 {formatDate(r.discount_until)}</small>}</td><td>{r.session_rewarded_at ? <div>首次转录已发</div> : null}{r.topup_rewarded_at ? <div>首充已发</div> : null}<small>累计付费 {formatUSD(r.paid_usd)}</small></td></tr>)}
        {!recipients.registrations.length && <tr><td colSpan={4}>暂无注册记录</td></tr>}
      </tbody></table></div>
      <Pagination page={recipientPage} pageSize={20} total={recipients.total} onChange={setRecipientPage} />
    </Modal>}
  </div>
}
