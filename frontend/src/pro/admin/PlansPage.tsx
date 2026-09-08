import { useCallback, useEffect, useState } from 'react'
import {
  deleteTopupTier,
  formatUSD,
  listPlans,
  listTopupTiers,
  planFeatureKeys,
  upsertPlan,
  upsertTopupTier,
  validatePlanInput,
  validateTopupTierInput,
  type Plan,
  type PlanFeatureKey,
  type TopupTier,
} from '../../admin/api'
import { formatInteger, formatPercent, limitLabel, type Runner } from './shared'
import { ErrorBanner, Modal } from './ui'

// Only byok, batch and auto_topup are checked by the backend today. The other
// flags came with the original membership schema and gate nothing yet, so the
// label says so instead of letting an administrator believe a switch works.
const featureLabels: Record<PlanFeatureKey, string> = {
  premium_models: '高级模型（未实现，开关暂无效果）',
  byok: '自带 Provider Key',
  batch: '批量处理',
  custom_prompt: '自定义提示词（未实现，开关暂无效果）',
  auto_topup: '自动充值',
  export_ledger: '导出账单（未实现，开关暂无效果）',
  api_access: 'API 访问（未实现，开关暂无效果）',
}

interface PlanDraft {
  isNew: boolean
  code: string
  name: string
  is_public: boolean
  active: boolean
  sort: string
  price_usd_month: string
  price_usd_year: string
  stripe_price_id_month: string
  stripe_price_id_year: string
  usage_discount_percent: string
  storage_gb: string
  retention_days: string
  max_concurrent_sessions: string
  seats: string
  features: Record<string, boolean>
}

interface TierDraft {
  isNew: boolean
  amount_usd: string
  bonus_percent: string
  bonus_expiry_days: string
  stripe_price_id: string
  active: boolean
  sort: string
}

function planToDraft(plan: Plan): PlanDraft {
  return {
    isNew: false,
    code: plan.code,
    name: plan.name,
    is_public: plan.is_public,
    active: plan.active,
    sort: String(plan.sort),
    price_usd_month: String(plan.price_usd_month),
    price_usd_year: String(plan.price_usd_year),
    stripe_price_id_month: plan.stripe_price_id_month || '',
    stripe_price_id_year: plan.stripe_price_id_year || '',
    usage_discount_percent: String(plan.usage_discount_percent),
    storage_gb: String(plan.storage_gb),
    retention_days: String(plan.retention_days),
    max_concurrent_sessions: String(plan.max_concurrent_sessions),
    seats: String(plan.seats),
    features: { ...(plan.features || {}) },
  }
}

function emptyPlanDraft(sort: number): PlanDraft {
  return {
    isNew: true,
    code: '',
    name: '',
    is_public: true,
    active: true,
    sort: String(sort),
    price_usd_month: '0',
    price_usd_year: '0',
    stripe_price_id_month: '',
    stripe_price_id_year: '',
    usage_discount_percent: '0',
    storage_gb: '1',
    retention_days: '-1',
    max_concurrent_sessions: '1',
    seats: '1',
    features: {},
  }
}

function draftToPlan(draft: PlanDraft): Plan {
  const integer = (value: string) => {
    const parsed = Number(value)
    return Number.isFinite(parsed) ? Math.trunc(parsed) : Number.NaN
  }
  return {
    code: draft.code.trim().toLowerCase(),
    name: draft.name.trim(),
    is_public: draft.is_public,
    active: draft.active,
    sort: integer(draft.sort),
    price_usd_month: Number(draft.price_usd_month),
    price_usd_year: Number(draft.price_usd_year),
    stripe_price_id_month: draft.stripe_price_id_month.trim() || undefined,
    stripe_price_id_year: draft.stripe_price_id_year.trim() || undefined,
    usage_discount_percent: Number(draft.usage_discount_percent),
    storage_gb: integer(draft.storage_gb),
    retention_days: integer(draft.retention_days),
    max_concurrent_sessions: integer(draft.max_concurrent_sessions),
    seats: integer(draft.seats),
    features: Object.fromEntries(
      planFeatureKeys.map((key) => [key, Boolean(draft.features[key])]),
    ),
  }
}

function tierToDraft(tier: TopupTier): TierDraft {
  return {
    isNew: false,
    amount_usd: String(tier.amount_usd),
    bonus_percent: String(tier.bonus_percent),
    bonus_expiry_days: String(tier.bonus_expiry_days),
    stripe_price_id: tier.stripe_price_id || '',
    active: tier.active,
    sort: String(tier.sort),
  }
}

function draftToTier(draft: TierDraft): TopupTier {
  return {
    amount_usd: Number(draft.amount_usd),
    bonus_percent: Number(draft.bonus_percent || 0),
    bonus_expiry_days: Number(draft.bonus_expiry_days),
    stripe_price_id: draft.stripe_price_id.trim() || undefined,
    active: draft.active,
    sort: Number(draft.sort || 0),
  }
}

export function PlansPage({ run, onOpenSettings }: { run: Runner; onOpenSettings: () => void }) {
  const [plans, setPlans] = useState<Plan[]>([])
  const [tiers, setTiers] = useState<TopupTier[]>([])
  const [loading, setLoading] = useState(true)
  const [planDraft, setPlanDraft] = useState<PlanDraft | null>(null)
  const [tierDraft, setTierDraft] = useState<TierDraft | null>(null)
  const [dialogError, setDialogError] = useState('')
  const [saving, setSaving] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<TopupTier | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    const result = await run(() => Promise.all([listPlans(), listTopupTiers()]))
    if (result) {
      setPlans(result[0])
      setTiers(result[1])
    }
    setLoading(false)
  }, [run])

  useEffect(() => { void load() }, [load])

  const closeDialogs = () => {
    if (saving) return
    setPlanDraft(null)
    setTierDraft(null)
    setPendingDelete(null)
    setDialogError('')
  }

  async function savePlan() {
    if (!planDraft) return
    const plan = draftToPlan(planDraft)
    const validationError = validatePlanInput(plan)
    if (validationError) {
      setDialogError(validationError)
      return
    }
    if (planDraft.isNew && plans.some((item) => item.code === plan.code)) {
      setDialogError('套餐代码已存在，请直接编辑现有套餐')
      return
    }
    setDialogError('')
    setSaving(true)
    try {
      const saved = await run(() => upsertPlan(plan), '套餐已保存', setDialogError)
      if (saved) {
        setPlans((current) => {
          const others = current.filter((item) => item.code !== saved.code)
          return [...others, saved].sort((left, right) => left.sort - right.sort || left.code.localeCompare(right.code))
        })
        setPlanDraft(null)
      }
    } finally {
      setSaving(false)
    }
  }

  async function saveTier() {
    if (!tierDraft) return
    const tier = draftToTier(tierDraft)
    const validationError = validateTopupTierInput(tier)
    if (validationError) {
      setDialogError(validationError)
      return
    }
    if (tierDraft.isNew && tiers.some((item) => item.amount_usd === tier.amount_usd)) {
      setDialogError('该金额的档位已存在，请直接编辑')
      return
    }
    setDialogError('')
    setSaving(true)
    try {
      const saved = await run(() => upsertTopupTier(tier), '充值档位已保存', setDialogError)
      if (saved) {
        setTiers(saved)
        setTierDraft(null)
      }
    } finally {
      setSaving(false)
    }
  }

  async function removeTier() {
    if (!pendingDelete) return
    setSaving(true)
    try {
      const saved = await run(() => deleteTopupTier(pendingDelete.amount_usd), '充值档位已删除')
      if (saved) {
        setTiers(saved)
        setPendingDelete(null)
      }
    } finally {
      setSaving(false)
    }
  }

  const nextPlanSort = plans.reduce((max, plan) => Math.max(max, plan.sort), 0) + 10
  const nextTierSort = tiers.reduce((max, tier) => Math.max(max, tier.sort), 0) + 10

  return (
    <>
      <div className="pa-stack">
        <section className="pa-card pa-section">
          <div className="pa-section__heading">
            <div><h2>会员套餐</h2><p>会员只提供用量折扣、配额与功能，不包含小时数；价格改动只影响之后的购买。</p></div>
            <button className="pa-button pa-button--primary" onClick={() => { setDialogError(''); setPlanDraft(emptyPlanDraft(nextPlanSort)) }} type="button">新建套餐</button>
          </div>
          <div className="pa-table-wrap"><table className="pa-table--wide">
            <thead><tr><th>套餐</th><th>可见性</th><th>价格</th><th>用量折扣</th><th>配额</th><th>功能</th><th>操作</th></tr></thead>
            <tbody>
              {loading && <tr><td className="pa-table-empty" colSpan={7}>正在加载套餐…</td></tr>}
              {!loading && plans.length === 0 && <tr><td className="pa-table-empty" colSpan={7}>还没有套餐。</td></tr>}
              {!loading && plans.map((plan) => {
                const enabledFeatures = planFeatureKeys.filter((key) => plan.features?.[key])
                return (
                  <tr key={plan.code}>
                    <td><strong>{plan.name}</strong><small>{plan.code} · 排序 {formatInteger(plan.sort)}</small></td>
                    <td>
                      <span className={`pa-status ${plan.active ? 'is-good' : 'is-muted'}`}>{plan.active ? '启用' : '停用'}</span>
                      <small>{plan.is_public ? '公开展示' : '隐藏（仅管理员分配）'}</small>
                    </td>
                    <td>
                      {plan.code === 'free' ? '免费' : (
                        <>
                          <span>{formatUSD(plan.price_usd_month)} / 月</span>
                          <small>{formatUSD(plan.price_usd_year)} / 年</small>
                        </>
                      )}
                    </td>
                    <td>{formatPercent(plan.usage_discount_percent)}</td>
                    <td>
                      <span>存储 {limitLabel(plan.storage_gb, 'GB')} · 自动清理未启用</span>
                      <small>并发 {limitLabel(plan.max_concurrent_sessions)} · 席位 {formatInteger(plan.seats)}</small>
                    </td>
                    <td>
                      {enabledFeatures.length === 0 ? <small>基础功能</small> : (
                        <div className="pa-feature-pills">
                          {enabledFeatures.map((key) => <span className="pa-pill pa-pill--accent" key={key}>{featureLabels[key]}</span>)}
                        </div>
                      )}
                    </td>
                    <td className="pa-actions"><button onClick={() => { setDialogError(''); setPlanDraft(planToDraft(plan)) }} type="button">编辑</button></td>
                  </tr>
                )
              })}
            </tbody>
          </table></div>
        </section>

        <section className="pa-card pa-section">
          <div className="pa-section__heading">
            <div><h2>充值档位</h2><p>用户按档位充值进入钱包；赠送部分作为有有效期的赠送额度发放。</p></div>
            <button className="pa-button pa-button--quiet" onClick={() => {
              setDialogError('')
              setTierDraft({ isNew: true, amount_usd: '', bonus_percent: '0', bonus_expiry_days: '90', stripe_price_id: '', active: true, sort: String(nextTierSort) })
            }} type="button">添加档位</button>
          </div>
          <div className="pa-table-wrap"><table>
            <thead><tr><th>充值金额</th><th>赠送比例</th><th>赠送有效期</th><th>Stripe 价格</th><th>状态</th><th>排序</th><th>操作</th></tr></thead>
            <tbody>
              {loading && <tr><td className="pa-table-empty" colSpan={7}>正在加载充值档位…</td></tr>}
              {!loading && tiers.length === 0 && <tr><td className="pa-table-empty" colSpan={7}>还没有充值档位，用户将无法充值。</td></tr>}
              {!loading && tiers.map((tier) => (
                <tr key={tier.amount_usd}>
                  <td><strong>{formatUSD(tier.amount_usd)}</strong>{tier.bonus_percent > 0 && <small>到账 {formatUSD(tier.amount_usd * (1 + tier.bonus_percent / 100))}</small>}</td>
                  <td>{formatPercent(tier.bonus_percent)}</td>
                  <td>{formatInteger(tier.bonus_expiry_days)} 天</td>
                  <td>{tier.stripe_price_id ? <small>{tier.stripe_price_id}</small> : '—'}</td>
                  <td><span className={`pa-status ${tier.active ? 'is-good' : 'is-muted'}`}>{tier.active ? '启用' : '停用'}</span></td>
                  <td>{formatInteger(tier.sort)}</td>
                  <td className="pa-actions">
                    <button onClick={() => { setDialogError(''); setTierDraft(tierToDraft(tier)) }} type="button">编辑</button>
                    <button className="pa-actions__danger" onClick={() => setPendingDelete(tier)} type="button">删除</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table></div>
        </section>

        <section className="pa-card pa-section pa-info-section">
          <div className="pa-section__heading">
            <div><h2>新用户试用额度</h2><p>注册赠送的试用金额与有效期在“系统设置”中维护，只影响之后创建的账户。</p></div>
            <button className="pa-button pa-button--quiet" onClick={onOpenSettings} type="button">前往系统设置</button>
          </div>
        </section>
      </div>

      {planDraft && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" disabled={saving} onClick={closeDialogs} type="button">取消</button>
              <button className="pa-button pa-button--primary" disabled={saving} onClick={() => void savePlan()} type="button">{saving ? '正在保存…' : '保存套餐'}</button>
            </>
          )}
          onClose={closeDialogs}
          title={planDraft.isNew ? '新建套餐' : `编辑套餐 · ${planDraft.code}`}
          wide
        >
          <div className="pa-dialog-form">
            {dialogError && <ErrorBanner message={dialogError} />}
            <div className="pa-dialog-grid">
              <label><span>套餐代码</span><input autoFocus={planDraft.isNew} disabled={!planDraft.isNew} maxLength={40} onChange={(event) => setPlanDraft({ ...planDraft, code: event.target.value })} pattern="[a-z0-9_-]+" placeholder="例如 pro" value={planDraft.code} /></label>
              <label><span>名称</span><input autoFocus={!planDraft.isNew} maxLength={100} onChange={(event) => setPlanDraft({ ...planDraft, name: event.target.value })} value={planDraft.name} /></label>
            </div>
            <div className="pa-dialog-grid pa-dialog-grid--three">
              <label className="pa-checkbox"><input checked={planDraft.is_public} onChange={(event) => setPlanDraft({ ...planDraft, is_public: event.target.checked })} type="checkbox" /><span>公开展示</span></label>
              <label className="pa-checkbox"><input checked={planDraft.active} disabled={planDraft.code === 'free'} onChange={(event) => setPlanDraft({ ...planDraft, active: event.target.checked })} type="checkbox" /><span>启用</span></label>
              <label><span>排序</span><input onChange={(event) => setPlanDraft({ ...planDraft, sort: event.target.value })} step="1" type="number" value={planDraft.sort} /></label>
            </div>
            <div className="pa-dialog-grid pa-dialog-grid--three">
              <label><span>月费（USD）</span><input disabled={planDraft.code === 'free'} min="0" onChange={(event) => setPlanDraft({ ...planDraft, price_usd_month: event.target.value })} step="0.01" type="number" value={planDraft.price_usd_month} /></label>
              <label><span>年费（USD）</span><input disabled={planDraft.code === 'free'} min="0" onChange={(event) => setPlanDraft({ ...planDraft, price_usd_year: event.target.value })} step="0.01" type="number" value={planDraft.price_usd_year} /></label>
              <label><span>用量折扣 %</span><input max="100" min="0" onChange={(event) => setPlanDraft({ ...planDraft, usage_discount_percent: event.target.value })} step="0.1" type="number" value={planDraft.usage_discount_percent} /></label>
            </div>
            <div className="pa-dialog-grid pa-dialog-grid--four">
              <label><span>存储（GB）</span><input min="-1" onChange={(event) => setPlanDraft({ ...planDraft, storage_gb: event.target.value })} step="1" type="number" value={planDraft.storage_gb} /></label>
              <label><span>保留天数（未实施自动清理）</span><input disabled value="未启用" /></label>
              <label><span>并发转录</span><input min="-1" onChange={(event) => setPlanDraft({ ...planDraft, max_concurrent_sessions: event.target.value })} step="1" type="number" value={planDraft.max_concurrent_sessions} /></label>
              <label><span>席位</span><input min="1" onChange={(event) => setPlanDraft({ ...planDraft, seats: event.target.value })} step="1" type="number" value={planDraft.seats} /></label>
            </div>
            <p className="pa-form-note">存储与并发填 -1 表示不限。保留天数未实施，不作为套餐权益或自动删除承诺。</p>
            <fieldset className="pa-fieldset">
              <legend>功能开关</legend>
              <div className="pa-checkbox-grid">
                {planFeatureKeys.map((key) => (
                  <label className="pa-checkbox" key={key}>
                    <input checked={Boolean(planDraft.features[key])} onChange={(event) => setPlanDraft({ ...planDraft, features: { ...planDraft.features, [key]: event.target.checked } })} type="checkbox" />
                    <span>{featureLabels[key]}</span>
                  </label>
                ))}
              </div>
            </fieldset>
            <div className="pa-dialog-grid">
              <label><span>Stripe 月付价格 ID（可选）</span><input maxLength={120} onChange={(event) => setPlanDraft({ ...planDraft, stripe_price_id_month: event.target.value })} placeholder="price_…" value={planDraft.stripe_price_id_month} /></label>
              <label><span>Stripe 年付价格 ID（可选）</span><input maxLength={120} onChange={(event) => setPlanDraft({ ...planDraft, stripe_price_id_year: event.target.value })} placeholder="price_…" value={planDraft.stripe_price_id_year} /></label>
            </div>
            {planDraft.code === 'free' && <div className="pa-callout">免费套餐始终启用且不能定价；这里只能调整配额、折扣与功能。</div>}
          </div>
        </Modal>
      )}

      {tierDraft && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" disabled={saving} onClick={closeDialogs} type="button">取消</button>
              <button className="pa-button pa-button--primary" disabled={saving} onClick={() => void saveTier()} type="button">{saving ? '正在保存…' : '保存档位'}</button>
            </>
          )}
          onClose={closeDialogs}
          title={tierDraft.isNew ? '添加充值档位' : `编辑充值档位 · ${formatUSD(Number(tierDraft.amount_usd))}`}
        >
          <div className="pa-dialog-form">
            {dialogError && <ErrorBanner message={dialogError} />}
            <div className="pa-dialog-grid">
              <label><span>充值金额（USD）</span><input autoFocus disabled={!tierDraft.isNew} min="1" onChange={(event) => setTierDraft({ ...tierDraft, amount_usd: event.target.value })} step="1" type="number" value={tierDraft.amount_usd} /></label>
              <label><span>赠送比例 %</span><input max="100" min="0" onChange={(event) => setTierDraft({ ...tierDraft, bonus_percent: event.target.value })} step="1" type="number" value={tierDraft.bonus_percent} /></label>
            </div>
            <div className="pa-dialog-grid">
              <label><span>赠送有效期（天）</span><input max="3650" min="1" onChange={(event) => setTierDraft({ ...tierDraft, bonus_expiry_days: event.target.value })} step="1" type="number" value={tierDraft.bonus_expiry_days} /></label>
              <label><span>排序</span><input onChange={(event) => setTierDraft({ ...tierDraft, sort: event.target.value })} step="1" type="number" value={tierDraft.sort} /></label>
            </div>
            <label><span>Stripe 价格 ID（可选）</span><input maxLength={120} onChange={(event) => setTierDraft({ ...tierDraft, stripe_price_id: event.target.value })} placeholder="留空时按金额动态创建" value={tierDraft.stripe_price_id} /></label>
            <label className="pa-checkbox"><input checked={tierDraft.active} onChange={(event) => setTierDraft({ ...tierDraft, active: event.target.checked })} type="checkbox" /><span>对用户开放</span></label>
            <p className="pa-form-note">金额是档位的唯一标识；要改金额请删除后重新添加。</p>
          </div>
        </Modal>
      )}

      {pendingDelete && (
        <Modal
          danger
          footer={(
            <>
              <button className="pa-button pa-button--quiet" disabled={saving} onClick={closeDialogs} type="button">取消</button>
              <button className="pa-button pa-button--danger" disabled={saving} onClick={() => void removeTier()} type="button">{saving ? '正在删除…' : '确认删除'}</button>
            </>
          )}
          onClose={closeDialogs}
          title="删除充值档位"
        >
          <div className="pa-dialog-form">
            <div className="pa-callout pa-callout--danger">将删除 {formatUSD(pendingDelete.amount_usd)} 档位。已完成的充值不受影响，但用户将不能再按该金额充值。</div>
          </div>
        </Modal>
      )}
    </>
  )
}
