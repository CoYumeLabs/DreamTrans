import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from 'react'
import {
  adjustCustomerWallet,
  datetimeLocalToRFC3339,
  formatHours,
  formatUSD,
  formatUsageUSD,
  getCustomer,
  getUser,
  grantCustomerCredit,
  listCustomers,
  listPlans,
  setCustomerPlan,
  updateUser,
  type AccountStatus,
  type AccountSummary,
  type CustomerDetail,
  type CustomerRow,
  type GrantKind,
  type Plan,
  type User,
} from '../../admin/api'
import {
  errorMessage,
  formatBytes,
  formatDate,
  formatDay,
  formatInteger,
  formatNumber,
  formatPercent,
  toDateTimeLocal,
  type Runner,
} from './shared'
import { ErrorBanner, MemberBadge, Modal, Pagination, SubTabs } from './ui'

const roleLabels: Record<User['role'], string> = {
  user: '用户',
  admin: '管理员',
  super_admin: '超级管理员',
}

const grantKindLabels: Record<GrantKind, string> = {
  trial: '试用额度',
  topup_bonus: '充值赠送',
  promo: '活动赠送',
  adjustment: '人工调整',
  settle_return: '结算返还',
}

const statusCopy: Record<AccountStatus, { label: string; className: string }> = {
  active: { label: '正常', className: 'is-good' },
  past_due: { label: '欠费', className: 'is-warn' },
  suspended: { label: '已暂停', className: 'is-bad' },
}

const transactionTypeLabels: Record<string, string> = {
  credit: '入账',
  debit: '扣费',
  refund: '退款',
  adjustment: '调整',
}

const paymentKindLabels: Record<string, string> = {
  topup: '充值',
  membership: '会员',
  refund: '退款',
}

function statusPill(status: AccountStatus | string) {
  const copy = statusCopy[status as AccountStatus] ?? { label: status, className: 'is-muted' }
  return <span className={`pa-status ${copy.className}`}>{copy.label}</span>
}

function signedUSD(value: number) {
  const className = value > 0 ? 'pa-text-good' : value < 0 ? 'pa-text-bad' : ''
  return <span className={className}>{value > 0 ? '+' : ''}{formatUsageUSD(value)}</span>
}

export function CustomersPage({
  run,
  initialUserId,
  actions,
}: {
  run: Runner
  initialUserId?: string | null
  /** Extra toolbar content (e.g. the create-user button) shown in the list heading. */
  actions?: ReactNode
}) {
  const pageSize = 50
  const [search, setSearch] = useState('')
  const [appliedSearch, setAppliedSearch] = useState('')
  const [page, setPage] = useState(1)
  const [rows, setRows] = useState<CustomerRow[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [selected, setSelected] = useState<string | null>(initialUserId ?? null)

  const load = useCallback(async () => {
    setLoading(true)
    const result = await run(() => listCustomers({
      search: appliedSearch,
      limit: pageSize,
      offset: (page - 1) * pageSize,
    }))
    if (result) {
      setRows(result.customers || [])
      setTotal(result.total)
    }
    setLoading(false)
  }, [appliedSearch, page, run])

  useEffect(() => { void load() }, [load])

  function submitSearch(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setPage(1)
    setAppliedSearch(search.trim())
  }

  if (selected) {
    return (
      <CustomerDetailView
        onBack={() => {
          setSelected(null)
          void load()
        }}
        run={run}
        userId={selected}
      />
    )
  }

  return (
    <section className="pa-card pa-section">
      <div className="pa-section__heading">
        <div><h2>用户与计费</h2><p>每个用户对应一个计费账户：赠送额度优先扣减，其次是钱包；点开详情可管理账号与套餐。</p></div>
        <div className="pa-header__actions">
          <form className="pa-toolbar" onSubmit={submitSearch}>
            <input
              aria-label="搜索用户"
              onChange={(event) => setSearch(event.target.value)}
              placeholder="搜索邮箱、昵称、活动、渠道或标签"
              type="search"
              value={search}
            />
            <button className="pa-button pa-button--quiet" type="submit">搜索</button>
          </form>
          {actions}
        </div>
      </div>
      <div className="pa-table-wrap"><table className="pa-table--wide">
        <thead><tr><th>用户</th><th>套餐</th><th>状态</th><th>钱包</th><th>赠送</th><th>本月扣费</th><th>累计扣费</th><th>操作</th></tr></thead>
        <tbody>
          {loading && <tr><td className="pa-table-empty" colSpan={8}>正在加载用户…</td></tr>}
          {!loading && rows.length === 0 && <tr><td className="pa-table-empty" colSpan={8}>{appliedSearch ? '没有匹配的用户。' : '还没有用户。'}</td></tr>}
          {!loading && rows.map((row) => (
            <tr className="pa-row-link" key={row.user_id} onClick={() => setSelected(row.user_id)}>
              <td><strong>{row.name || row.email}</strong><small>{row.email}{row.role !== 'user' ? ` · ${row.role}` : ''}</small>{row.promotion_channel && <small>来源：{row.promotion_name} · {row.promotion_channel}</small>}{Boolean(row.promotion_tags?.length) && <small>{row.promotion_tags?.join(' · ')}</small>}{row.referrer_email && <small>推荐人：{row.referrer_name || row.referrer_email}</small>}</td>
              <td>
                <MemberBadge active={row.member_active} planCode={row.plan_code} />
                {row.member_until && row.plan_code !== 'free' && <small>至 {formatDay(row.member_until)}</small>}
              </td>
              <td>{statusPill(row.status)}</td>
              <td>{formatUSD(row.wallet_usd)}</td>
              <td>{formatUSD(row.grant_usd)}</td>
              <td>{formatUSD(row.month_charged_usd)}</td>
              <td>{formatUSD(row.lifetime_charged_usd)}</td>
              <td className="pa-actions"><button onClick={(event) => {
                event.stopPropagation()
                setSelected(row.user_id)
              }} type="button">详情</button></td>
            </tr>
          ))}
        </tbody>
      </table></div>
      <Pagination onChange={setPage} page={page} pageSize={pageSize} total={total} />
    </section>
  )
}

type DetailTab = 'ledger' | 'usage' | 'payments'

interface GrantDraft {
  amount: string
  kind: 'promo' | 'adjustment' | 'trial'
  expiryDays: string
  note: string
}

interface AdjustDraft {
  amount: string
  description: string
  allowNegative: boolean
}

interface PlanDraft {
  planCode: string
  memberUntil: string
  customDiscount: string
  customMarkup: string
  note: string
}

function planDraftFrom(account: AccountSummary): PlanDraft {
  return {
    planCode: account.plan_code || 'free',
    memberUntil: account.member_until ? toDateTimeLocal(account.member_until) : '',
    customDiscount: account.custom_discount_percent === undefined ? '' : String(account.custom_discount_percent),
    customMarkup: account.custom_markup_percent === undefined ? '' : String(account.custom_markup_percent),
    note: '',
  }
}

function optionalPercent(value: string): number | null | 'invalid' {
  if (!value.trim()) return null
  const parsed = Number(value)
  if (!Number.isFinite(parsed) || parsed < 0 || parsed > 100_000) return 'invalid'
  return parsed
}

function CustomerDetailView({
  userId,
  run,
  onBack,
}: {
  userId: string
  run: Runner
  onBack: () => void
}) {
  const [detail, setDetail] = useState<CustomerDetail | null>(null)
  const [plans, setPlans] = useState<Plan[]>([])
  const [userInfo, setUserInfo] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [tab, setTab] = useState<DetailTab>('ledger')
  const [grantDraft, setGrantDraft] = useState<GrantDraft | null>(null)
  const [adjustDraft, setAdjustDraft] = useState<AdjustDraft | null>(null)
  const [planDraft, setPlanDraft] = useState<PlanDraft | null>(null)
  const [dialogError, setDialogError] = useState('')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    let active = true
    setLoading(true)
    setLoadError('')
    void Promise.all([getCustomer(userId), listPlans(), getUser(userId)]).then(([nextDetail, nextPlans, nextUser]) => {
      if (!active) return
      setDetail(nextDetail)
      setPlans(nextPlans)
      setUserInfo(nextUser.user)
    }).catch((reason) => {
      if (active) setLoadError(errorMessage(reason))
    }).finally(() => {
      if (active) setLoading(false)
    })
    return () => { active = false }
  }, [userId])

  async function toggleUserActive() {
    if (!userInfo) return
    const next = await run(
      () => updateUser(userInfo.id, { is_active: !userInfo.is_active }),
      userInfo.is_active ? '账号已停用' : '账号已启用',
    )
    if (next) setUserInfo(next)
  }

  async function performAction(
    operation: () => Promise<CustomerDetail>,
    success: string,
    close: () => void,
  ) {
    setDialogError('')
    setSaving(true)
    try {
      const result = await run(operation, success, setDialogError)
      if (result) {
        setDetail(result)
        close()
      }
    } finally {
      setSaving(false)
    }
  }

  function submitGrant() {
    if (!grantDraft) return
    const amount = Number(grantDraft.amount)
    if (!Number.isFinite(amount) || amount <= 0) {
      setDialogError('赠送金额必须大于 0')
      return
    }
    const expiryDays = grantDraft.expiryDays.trim() ? Number(grantDraft.expiryDays) : undefined
    if (expiryDays !== undefined && (!Number.isInteger(expiryDays) || expiryDays <= 0)) {
      setDialogError('有效期必须是正整数天数，或留空表示永不过期')
      return
    }
    void performAction(() => grantCustomerCredit(userId, {
      amount_usd: amount,
      kind: grantDraft.kind,
      ...(expiryDays !== undefined ? { expiry_days: expiryDays } : {}),
      ...(grantDraft.note.trim() ? { note: grantDraft.note.trim() } : {}),
    }), '赠送额度已发放', () => setGrantDraft(null))
  }

  function submitAdjust() {
    if (!adjustDraft) return
    const amount = Number(adjustDraft.amount)
    if (!Number.isFinite(amount) || amount === 0) {
      setDialogError('调整金额必须是非零数字')
      return
    }
    void performAction(() => adjustCustomerWallet(userId, {
      amount,
      description: adjustDraft.description.trim() || '管理员后台调整',
      allow_negative: adjustDraft.allowNegative,
    }), '钱包余额已调整', () => setAdjustDraft(null))
  }

  function submitPlan() {
    if (!planDraft) return
    const isFree = planDraft.planCode === 'free'
    const memberUntil = isFree ? null : datetimeLocalToRFC3339(planDraft.memberUntil)
    if (!isFree && !memberUntil) {
      setDialogError('付费套餐需要填写会员到期时间')
      return
    }
    const customDiscount = optionalPercent(planDraft.customDiscount)
    const customMarkup = optionalPercent(planDraft.customMarkup)
    if (customDiscount === 'invalid' || customMarkup === 'invalid') {
      setDialogError('自定义折扣与加价必须是非负数字，或留空表示沿用套餐默认')
      return
    }
    void performAction(() => setCustomerPlan(userId, {
      plan_code: planDraft.planCode,
      ...(memberUntil ? { member_until: memberUntil } : {}),
      custom_discount_percent: customDiscount,
      custom_markup_percent: customMarkup,
      ...(planDraft.note.trim() ? { note: planDraft.note.trim() } : {}),
    }), '套餐已更新', () => setPlanDraft(null))
  }

  const account = detail?.account
  const closeDialogs = () => {
    if (saving) return
    setGrantDraft(null)
    setAdjustDraft(null)
    setPlanDraft(null)
    setDialogError('')
  }

  return (
    <>
      <div className="pa-stack">
        <section className="pa-card pa-section">
          <div className="pa-section__heading">
            <div className="pa-detail-head">
              <button className="pa-link-button pa-link-button--inline" onClick={onBack} type="button">← 返回用户列表</button>
              {account ? (
                <>
                  <h2>{account.name || account.email}</h2>
                  <p>
                    {account.email} · {statusPill(account.status)} · <MemberBadge active={account.member_active} planCode={account.plan_code} planName={account.effective_plan?.name || account.plan?.name} />
                    {userInfo && (
                      <>
                        {' · '}<span className="pa-pill">{roleLabels[userInfo.role] || userInfo.role}</span>
                        {' · '}<span className={`pa-status ${userInfo.is_active ? 'is-good' : 'is-muted'}`}>{userInfo.is_active ? '账号启用' : '账号停用'}</span>
                        {userInfo.last_login_at ? <small> · 最近登录 {formatDate(userInfo.last_login_at)}</small> : null}
                      </>
                    )}
                  </p>
                </>
              ) : <h2>用户详情</h2>}
            </div>
            {account && (
              <div className="pa-header__actions">
                {userInfo && userInfo.role !== 'super_admin' && (
                  <button className="pa-button pa-button--quiet" onClick={() => void toggleUserActive()} type="button">
                    {userInfo.is_active ? '停用账号' : '启用账号'}
                  </button>
                )}
                <button className="pa-button pa-button--quiet" onClick={() => { setDialogError(''); setGrantDraft({ amount: '', kind: 'promo', expiryDays: '30', note: '' }) }} type="button">赠送额度</button>
                <button className="pa-button pa-button--quiet" onClick={() => { setDialogError(''); setAdjustDraft({ amount: '', description: '', allowNegative: false }) }} type="button">调整钱包</button>
                <button className="pa-button pa-button--primary" onClick={() => { setDialogError(''); setPlanDraft(planDraftFrom(account)) }} type="button">设置套餐</button>
              </div>
            )}
          </div>
          {loadError && <ErrorBanner message={`客户信息加载失败：${loadError}`} />}
          {loading && (
            <div className="pa-summary-grid pa-summary-grid--four">
              {[0, 1, 2, 3].map((item) => <span className="pa-skeleton pa-skeleton--panel" key={item} />)}
            </div>
          )}
          {!loading && account && (
            <>
              <div className="pa-summary-grid pa-summary-grid--four">
                <div>
                  <small>可用余额</small>
                  <strong>{formatUSD(account.available_usd)}</strong>
                  <em>{formatHours(account.estimated_realtime_hours)} 实时转写</em>
                </div>
                <div><small>钱包（已付费）</small><strong>{formatUSD(account.wallet_usd)}</strong></div>
                <div><small>赠送额度（优先扣减）</small><strong>{formatUSD(account.grant_usd)}</strong></div>
                <div><small>累计扣费</small><strong>{formatUSD(account.lifetime_charged_usd)}</strong></div>
              </div>
              <div className="pa-summary-grid pa-summary-grid--four pa-summary-grid--spaced">
                <div>
                  <small>生效套餐</small>
                  <strong>{account.effective_plan?.name || account.effective_plan?.code || 'Free'}</strong>
                  <em>
                    {account.plan_code !== 'free'
                      ? `${account.member_active ? '有效' : '已到期'} · 至 ${formatDate(account.member_until)}`
                      : '免费套餐'}
                  </em>
                </div>
                <div>
                  <small>用量折扣</small>
                  <strong>{formatPercent(account.discount_percent)}</strong>
                  <em>{account.custom_discount_percent !== undefined ? `自定义折扣 ${formatPercent(account.custom_discount_percent)}` : '沿用套餐默认'}</em>
                </div>
                <div>
                  <small>实时转写售价</small>
                  <strong>{formatUSD(account.realtime_hour_usd)} / 小时</strong>
                  <em>{account.custom_markup_percent !== undefined ? `自定义加价 ${formatPercent(account.custom_markup_percent)}` : '沿用全局加价'}</em>
                </div>
                <div>
                  <small>自动充值</small>
                  <strong>{account.auto_topup_enabled ? '已开启' : '关闭'}</strong>
                  <em>
                    {account.auto_topup_enabled && account.auto_topup_threshold_usd !== undefined && account.auto_topup_amount_usd !== undefined
                      ? `低于 ${formatUSD(account.auto_topup_threshold_usd)} 时充值 ${formatUSD(account.auto_topup_amount_usd)}`
                      : account.has_payment_method ? '已绑定支付方式' : '未绑定支付方式'}
                  </em>
                </div>
              </div>
              <div className="pa-footnotes">
                <span>Stripe 客户：{account.stripe_customer_id || '—'}</span>
                <span>知识库存储：{formatBytes(account.storage_bytes)}</span>
                {account.membership && (
                  <span>
                    订阅：{account.membership.plan_code} / {account.membership.interval === 'year' ? '按年' : '按月'} · {account.membership.status}
                    {account.membership.current_period_end ? ` · 当前周期至 ${formatDay(account.membership.current_period_end)}` : ''}
                    {account.membership.cancel_at_period_end ? ' · 周期结束后取消' : ''}
                  </span>
                )}
              </div>
            </>
          )}
        </section>

        {!loading && account && (
          <section className="pa-card pa-section">
            <div className="pa-section__heading">
              <div><h2>赠送额度</h2><p>按过期时间先后消耗；过期后剩余额度自动作废。</p></div>
            </div>
            <div className="pa-table-wrap"><table>
              <thead><tr><th>类型</th><th>发放</th><th>剩余</th><th>到期</th><th>备注</th><th>发放时间</th></tr></thead>
              <tbody>
                {account.grants.length === 0 && <tr><td className="pa-table-empty" colSpan={6}>没有有效的赠送额度。</td></tr>}
                {account.grants.map((grant) => (
                  <tr key={grant.id}>
                    <td><span className="pa-pill">{grantKindLabels[grant.kind] || grant.kind}</span></td>
                    <td>{formatUSD(grant.amount_usd)}</td>
                    <td>{formatUSD(grant.remaining_usd)}</td>
                    <td>{grant.expires_at ? formatDate(grant.expires_at) : '永不过期'}</td>
                    <td>{grant.note || '—'}</td>
                    <td>{formatDate(grant.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table></div>
          </section>
        )}

        {!loading && detail && (
          <section className="pa-card pa-section">
            <SubTabs
              items={[
                { id: 'ledger', label: '流水', count: detail.ledger.length },
                { id: 'usage', label: '用量', count: detail.usage.length },
                { id: 'payments', label: '支付', count: detail.payments.length },
              ]}
              onChange={setTab}
              value={tab}
            />
            {tab === 'ledger' && (
              <div className="pa-table-wrap"><table className="pa-table--wide">
                <thead><tr><th>时间</th><th>类型</th><th>账户</th><th>金额</th><th>变动后余额</th><th>说明</th><th>操作者</th></tr></thead>
                <tbody>
                  {detail.ledger.length === 0 && <tr><td className="pa-table-empty" colSpan={7}>还没有余额流水。</td></tr>}
                  {detail.ledger.map((item) => (
                    <tr key={item.id}>
                      <td>{formatDate(item.created_at)}</td>
                      <td><span className="pa-pill">{transactionTypeLabels[item.transaction_type] || item.transaction_type}</span></td>
                      <td>{item.bucket === 'grant' ? '赠送' : '钱包'}</td>
                      <td>{signedUSD(item.amount_usd)}</td>
                      <td>{formatUSD(item.balance_after_usd)}</td>
                      <td>{item.description || '—'}{item.reference_type ? <small>{item.reference_type}{item.reference_id ? ` · ${item.reference_id}` : ''}</small> : null}</td>
                      <td>{item.created_by ? <small>{item.created_by}</small> : '系统'}</td>
                    </tr>
                  ))}
                </tbody>
              </table></div>
            )}
            {tab === 'usage' && (
              <div className="pa-table-wrap"><table className="pa-table--wide">
                <thead><tr><th>时间</th><th>用途</th><th>模型</th><th>用量</th><th>扣费</th><th>上游成本</th><th>毛利</th><th>状态</th></tr></thead>
                <tbody>
                  {detail.usage.length === 0 && <tr><td className="pa-table-empty" colSpan={8}>还没有用量记录。</td></tr>}
                  {detail.usage.map((item) => (
                    <tr key={item.id}>
                      <td>{formatDate(item.created_at)}</td>
                      <td>{item.action}{item.attribution && item.attribution !== 'platform' ? <small>{item.attribution}</small> : null}</td>
                      <td>{item.model || '—'}</td>
                      <td>
                        {formatNumber(item.quantity, 4)}
                        {(item.input_tokens > 0 || item.output_tokens > 0) && (
                          <small>输入 {formatInteger(item.input_tokens)} · 输出 {formatInteger(item.output_tokens)}{item.cached_input_tokens > 0 ? ` · 缓存 ${formatInteger(item.cached_input_tokens)}` : ''}</small>
                        )}
                      </td>
                      <td>
                        {formatUsageUSD(item.cost_usd)}
                        <small>赠送 {formatUsageUSD(item.grant_usd)} · 钱包 {formatUsageUSD(item.wallet_usd)}</small>
                      </td>
                      <td>{formatUsageUSD(item.upstream_cost_usd)}</td>
                      <td>{signedUSD(item.margin_usd)}</td>
                      <td>
                        {item.refunded
                          ? <span className="pa-status is-muted">已退款</span>
                          : item.settled ? <span className="pa-status is-good">已结算</span> : <span className="pa-status is-warn">预扣</span>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table></div>
            )}
            {tab === 'payments' && (
              <div className="pa-table-wrap"><table>
                <thead><tr><th>时间</th><th>类型</th><th>金额</th><th>赠送</th><th>状态</th><th>说明</th><th>Stripe</th></tr></thead>
                <tbody>
                  {detail.payments.length === 0 && <tr><td className="pa-table-empty" colSpan={7}>还没有支付记录。</td></tr>}
                  {detail.payments.map((item) => (
                    <tr key={item.id}>
                      <td>{formatDate(item.created_at)}</td>
                      <td><span className="pa-pill">{paymentKindLabels[item.kind] || item.kind}</span></td>
                      <td>{formatUSD(item.amount_usd)}</td>
                      <td>{item.bonus_usd ? formatUSD(item.bonus_usd) : '—'}</td>
                      <td><span className={`pa-status ${item.status === 'succeeded' ? 'is-good' : item.status === 'failed' ? 'is-bad' : 'is-muted'}`}>{item.status}</span></td>
                      <td>{item.description || '—'}</td>
                      <td>{item.stripe_object_id ? <small>{item.stripe_object_id}</small> : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table></div>
            )}
          </section>
        )}
      </div>

      {grantDraft && account && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" disabled={saving} onClick={closeDialogs} type="button">取消</button>
              <button className="pa-button pa-button--primary" disabled={saving || !Number(grantDraft.amount)} onClick={submitGrant} type="button">{saving ? '正在发放…' : '确认赠送'}</button>
            </>
          )}
          onClose={closeDialogs}
          title="赠送额度"
        >
          <div className="pa-dialog-form">
            {dialogError && <ErrorBanner message={dialogError} />}
            <div className="pa-callout"><strong>{account.email}</strong><span>当前赠送余额 {formatUSD(account.grant_usd)}</span></div>
            <div className="pa-dialog-grid">
              <label><span>金额（USD）</span><input autoFocus min="0.01" onChange={(event) => setGrantDraft({ ...grantDraft, amount: event.target.value })} step="0.01" type="number" value={grantDraft.amount} /></label>
              <label><span>类型</span><select onChange={(event) => setGrantDraft({ ...grantDraft, kind: event.target.value as GrantDraft['kind'] })} value={grantDraft.kind}>
                <option value="promo">活动赠送</option>
                <option value="adjustment">人工调整</option>
                <option value="trial">试用额度</option>
              </select></label>
            </div>
            <label>
              <span>有效期（天）</span>
              <input min="1" onChange={(event) => setGrantDraft({ ...grantDraft, expiryDays: event.target.value })} placeholder="留空表示永不过期" step="1" type="number" value={grantDraft.expiryDays} />
            </label>
            <label><span>备注</span><input maxLength={200} onChange={(event) => setGrantDraft({ ...grantDraft, note: event.target.value })} placeholder="说明本次赠送原因" value={grantDraft.note} /></label>
            <p className="pa-form-note">赠送额度会先于钱包被扣减；发放后立即可用，并记录到余额流水。</p>
          </div>
        </Modal>
      )}

      {adjustDraft && account && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" disabled={saving} onClick={closeDialogs} type="button">取消</button>
              <button className="pa-button pa-button--primary" disabled={saving || !Number(adjustDraft.amount)} onClick={submitAdjust} type="button">{saving ? '正在调整…' : '确认调整'}</button>
            </>
          )}
          onClose={closeDialogs}
          title="调整钱包余额"
        >
          <div className="pa-dialog-form">
            {dialogError && <ErrorBanner message={dialogError} />}
            <div className="pa-callout"><strong>{account.email}</strong><span>当前钱包 {formatUSD(account.wallet_usd)}</span></div>
            <label><span>调整金额（USD）</span><input autoFocus onChange={(event) => setAdjustDraft({ ...adjustDraft, amount: event.target.value })} placeholder="正数增加，负数扣减" step="0.01" type="number" value={adjustDraft.amount} /></label>
            <label><span>操作备注</span><input maxLength={200} onChange={(event) => setAdjustDraft({ ...adjustDraft, description: event.target.value })} placeholder="说明本次调整原因" value={adjustDraft.description} /></label>
            <label className="pa-checkbox">
              <input checked={adjustDraft.allowNegative} onChange={(event) => setAdjustDraft({ ...adjustDraft, allowNegative: event.target.checked })} type="checkbox" />
              <span>允许扣减后余额为负</span>
            </label>
            <p className="pa-form-note">钱包调整不设有效期；如需发放会过期的额度，请使用“赠送额度”。</p>
          </div>
        </Modal>
      )}

      {planDraft && account && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" disabled={saving} onClick={closeDialogs} type="button">取消</button>
              <button className="pa-button pa-button--primary" disabled={saving} onClick={submitPlan} type="button">{saving ? '正在保存…' : '保存套餐'}</button>
            </>
          )}
          onClose={closeDialogs}
          title="设置套餐"
        >
          <div className="pa-dialog-form">
            {dialogError && <ErrorBanner message={dialogError} />}
            <label><span>套餐</span><select autoFocus onChange={(event) => setPlanDraft({ ...planDraft, planCode: event.target.value })} value={planDraft.planCode}>
              {plans.map((plan) => (
                <option key={plan.code} value={plan.code}>{plan.name} ({plan.code}){plan.active ? '' : ' · 已停用'}</option>
              ))}
              {!plans.some((plan) => plan.code === planDraft.planCode) && <option value={planDraft.planCode}>{planDraft.planCode}</option>}
            </select></label>
            {planDraft.planCode !== 'free' && (
              <label>
                <span>会员到期时间</span>
                <input onChange={(event) => setPlanDraft({ ...planDraft, memberUntil: event.target.value })} required type="datetime-local" value={planDraft.memberUntil} />
                <small>手动设置的会员不会自动续费；到期后回落到免费套餐。</small>
              </label>
            )}
            <div className="pa-dialog-grid">
              <label><span>自定义折扣 %</span><input min="0" onChange={(event) => setPlanDraft({ ...planDraft, customDiscount: event.target.value })} placeholder="留空沿用套餐" step="0.1" type="number" value={planDraft.customDiscount} /></label>
              <label><span>自定义加价 %</span><input min="0" onChange={(event) => setPlanDraft({ ...planDraft, customMarkup: event.target.value })} placeholder="留空沿用全局" step="0.1" type="number" value={planDraft.customMarkup} /></label>
            </div>
            <label><span>备注</span><input maxLength={200} onChange={(event) => setPlanDraft({ ...planDraft, note: event.target.value })} placeholder="记录到审计日志" value={planDraft.note} /></label>
            {account.membership?.stripe_subscription_id && (
              <div className="pa-callout pa-callout--warning">该客户存在 Stripe 订阅，手动修改套餐不会同步到 Stripe；续费时会以订阅为准。</div>
            )}
          </div>
        </Modal>
      )}
    </>
  )
}
