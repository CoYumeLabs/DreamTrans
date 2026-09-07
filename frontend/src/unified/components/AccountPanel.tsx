import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  createBillingCheckout,
  createBillingPortal,
  downloadUserStatementCSV,
  formatHours,
  formatCheckoutCharge,
  formatUSD,
  formatUsageUSD,
  getSessionCostSummaries,
  getUserBillingLedger,
  getUserBillingPlans,
  getUserStatement,
  getUserUsage,
  setUserAutoTopup,
  type AccountBalance,
  type AccountSummary,
  type BalanceTransaction,
  type PaymentRow,
  type Plan,
  type PlanHourlyExample,
  type SessionCostSummary,
  type TopupTier,
  type UserBillingPlans,
  type UserStatement,
  type UserUsageItem,
} from '../../api'
import { ApiRequestError } from '../../pro/api/auth'
import { getUserReferral, type ReferralSummary } from '../../api'
import { intlLocale, messages, useMessages } from '../../i18n'
import { Icon } from './Icon'

export interface AccountPanelProps {
  account: AccountSummary | null
  balance: AccountBalance | null
  open: boolean
  paymentsEnabled: boolean
  sessionId: string
  onRefreshAccount: () => Promise<void>
}

const FREE_PLAN_CODE = 'free'

/** How many months back the statement picker offers. */
const STATEMENT_MONTHS = 12

/** The picker's "all records" sentinel; an empty month means no month filter. */
const ALL_MONTHS = ''

/** Month keys, newest first, as YYYY-MM in UTC — the same key the API takes. */
function recentMonthKeys(now: Date): string[] {
  const keys: string[] = []
  for (let back = 0; back < STATEMENT_MONTHS; back += 1) {
    const point = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - back, 1))
    keys.push(`${point.getUTCFullYear()}-${String(point.getUTCMonth() + 1).padStart(2, '0')}`)
  }
  return keys
}

function monthLabel(key: string): string {
  const [year, month] = key.split('-').map(Number)
  if (!year || !month) return key
  try {
    return new Intl.DateTimeFormat(intlLocale(), { year: 'numeric', month: 'long', timeZone: 'UTC' })
      .format(new Date(Date.UTC(year, month - 1, 1)))
  } catch {
    return key
  }
}

/**
 * Ledger model keys are internal billing identifiers. Transcription rows get a
 * product label; AI rows only show their action, so no upstream vendor or
 * model id leaks into the customer-facing usage list.
 */
function usageModelLabel(item: { action: string; model?: string | null }): string | null {
  const model = (item.model ?? '').trim()
  if (!model) return null
  const labels: Record<string, string> = {
    'speechmatics-realtime-enhanced': messages().billing.models.realtime,
    'speechmatics-classic-token': messages().billing.models.realtime,
    'speechmatics-batch-enhanced': messages().billing.models.batch,
  }
  return labels[model] ?? null
}

/**
 * One display row of the balance ledger. Usage entries arrive as a
 * reserve-then-settle pair (debit at the estimated maximum, refund of the
 * over-reserved part seconds later); showing both raw legs reads like
 * "charged then refunded the same amount" once rounded to two decimals, so
 * rows sharing a usage id are folded into a single net-amount row.
 */
interface LedgerDisplayRow {
  key: string
  primary: BalanceTransaction
  netUSD: number
  buckets: Set<BalanceTransaction['bucket']>
  merged: boolean
}

function mergeUsageLedger(entries: BalanceTransaction[]): LedgerDisplayRow[] {
  const rows: LedgerDisplayRow[] = []
  const byUsage = new Map<string, LedgerDisplayRow>()
  for (const entry of entries) {
    const usageId = entry.reference_type === 'usage' ? entry.reference_id : null
    const foldable = usageId
      && (entry.transaction_type === 'debit' || entry.transaction_type === 'refund')
    if (!foldable) {
      rows.push({
        key: entry.id,
        primary: entry,
        netUSD: entry.amount_usd,
        buckets: new Set([entry.bucket]),
        merged: false,
      })
      continue
    }
    const existing = byUsage.get(usageId)
    if (existing) {
      existing.netUSD += entry.amount_usd
      existing.buckets.add(entry.bucket)
      existing.merged = true
      // Entries are newest-first, so the debit (the older leg) is visited
      // last; it carries the action name and the moment the usage happened.
      if (entry.transaction_type === 'debit') existing.primary = entry
      continue
    }
    const row: LedgerDisplayRow = {
      key: `usage:${usageId}`,
      primary: entry,
      netUSD: entry.amount_usd,
      buckets: new Set([entry.bucket]),
      merged: false,
    }
    byUsage.set(usageId, row)
    rows.push(row)
  }
  return rows
}

function ledgerBucketLabel(buckets: Set<BalanceTransaction['bucket']>): string {
  const b = messages().billing.buckets
  if (buckets.size > 1) return b.mixed
  return buckets.has('grant') ? b.grant : b.wallet
}

function formatDate(value?: string | null): string {
  if (!value) return ''
  const time = Date.parse(value)
  if (!Number.isFinite(time)) return value
  return new Intl.DateTimeFormat(intlLocale(), {
    year: 'numeric',
    month: 'numeric',
    day: 'numeric',
  }).format(time)
}

function formatDateTime(value?: string | null): string {
  if (!value) return ''
  const time = Date.parse(value)
  if (!Number.isFinite(time)) return value
  return new Intl.DateTimeFormat(intlLocale(), {
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(time)
}

function discountLabel(percent: number): string | null {
  if (!(percent > 0) || percent >= 100) return null
  return messages().billing.discount(percent)
}

function tierLabel(tier: TopupTier, charge: string | null): string {
  const bonus = tier.amount_usd * (tier.bonus_percent / 100)
  const suffix = charge ? ` ${charge}` : ''
  if (bonus > 0) {
    return messages().billing.tierWithBonus(formatUSD(tier.amount_usd, 0), formatUSD(bonus), tier.bonus_percent, suffix)
  }
  return messages().billing.tier(formatUSD(tier.amount_usd, 0), suffix)
}

function paidFromLabel(item: UserUsageItem): string {
  const b = messages().billing.buckets
  if (item.refunded) return b.refunded
  if (item.grant_usd > 0 && item.wallet_usd > 0) return b.mixed
  if (item.grant_usd > 0) return b.grantCredit
  if (item.wallet_usd > 0) return b.wallet
  if (!item.settled) return b.reserving
  return b.free
}

function billingErrorMessage(reason: unknown, fallback: string): string {
  const b = messages().billing
  if (reason instanceof ApiRequestError) {
    switch (reason.status) {
      case 503:
        return b.paymentsDisabled
      case 409:
        return b.errors.existingMember
      case 404:
        return b.errors.noPayments
      case 403:
        return b.errors.unavailable
      case 400:
        return reason.message.includes('payment method')
          ? b.errors.paymentMethod
          : reason.message
      default:
        return reason.message || fallback
    }
  }
  return reason instanceof Error && reason.message ? reason.message : fallback
}

function estimatedHours(account: AccountSummary | null, balance: AccountBalance | null): number {
  if (!account) return 0
  const available = balance?.available_usd ?? account.available_usd
  if (account.realtime_hour_usd > 0) return Math.max(0, available / account.realtime_hour_usd)
  return Math.max(0, account.estimated_realtime_hours)
}

/** One hairline group of the statement breakdown; renders nothing if empty. */
function StatementSplit({ rows }: { rows: ReadonlyArray<readonly [string, number]> }) {
  const shown = rows.filter(([, amount]) => amount > 0)
  if (shown.length === 0) return null
  return (
    <dl className="dt-billing-statement__split">
      {shown.map(([label, amount]) => (
        <div key={label}>
          <dt>{label}</dt>
          <dd>{formatUsageUSD(amount)}</dd>
        </div>
      ))}
    </dl>
  )
}

export function AccountPanel({
  account,
  balance,
  open,
  paymentsEnabled,
  sessionId,
  onRefreshAccount,
}: AccountPanelProps) {
  const m = useMessages()
  const b = m.billing
  const [plans, setPlans] = useState<UserBillingPlans | null>(null)
  const [usage, setUsage] = useState<UserUsageItem[]>([])
  const [sessionCost, setSessionCost] = useState<SessionCostSummary | null>(null)
  const [ledger, setLedger] = useState<{ ledger: BalanceTransaction[]; payments: PaymentRow[] } | null>(null)
  const [ledgerOpen, setLedgerOpen] = useState(false)
  const [ledgerLoading, setLedgerLoading] = useState(false)
  const [statementMonth, setStatementMonth] = useState<string>(() => recentMonthKeys(new Date())[0] ?? ALL_MONTHS)
  const [statement, setStatement] = useState<UserStatement | null>(null)
  const [statementLoading, setStatementLoading] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [autoTopupThreshold, setAutoTopupThreshold] = useState('')
  const [autoTopupAmount, setAutoTopupAmount] = useState('')
  const [referral, setReferral] = useState<ReferralSummary | null | 'failed'>(null)
  const [referralCopied, setReferralCopied] = useState(false)

  useEffect(() => {
    if (!open) return
    let active = true
    void getUserReferral()
      .then((next) => { if (active) setReferral(next) })
      .catch(() => { if (active) setReferral('failed') })
    return () => { active = false }
  }, [open])

  useEffect(() => {
    if (!open) return
    let active = true
    void getUserBillingPlans()
      .then((next) => { if (active) setPlans(next) })
      .catch(() => { if (active) setPlans(null) })
    void getUserUsage(sessionId || undefined)
      .then((items) => { if (active) setUsage(items.slice(0, 8)) })
      .catch(() => { if (active) setUsage([]) })
    if (sessionId) {
      void getSessionCostSummaries([sessionId])
        .then((summaries) => { if (active) setSessionCost(summaries[0] ?? null) })
        .catch(() => { if (active) setSessionCost(null) })
    } else {
      setSessionCost(null)
    }
    return () => { active = false }
  }, [open, sessionId, account?.lifetime_charged_usd])

  useEffect(() => {
    if (!open) {
      setNotice(null)
      setLedgerOpen(false)
      return
    }
    setAutoTopupThreshold(
      account?.auto_topup_threshold_usd !== undefined ? String(account.auto_topup_threshold_usd) : '5',
    )
    setAutoTopupAmount(
      account?.auto_topup_amount_usd !== undefined ? String(account.auto_topup_amount_usd) : '20',
    )
  }, [open, account?.auto_topup_threshold_usd, account?.auto_topup_amount_usd])

  useEffect(() => {
    if (!ledgerOpen || !open) return
    let active = true
    setLedgerLoading(true)
    void getUserBillingLedger()
      .then((next) => { if (active) setLedger(next) })
      .catch((reason: unknown) => {
        if (active) setNotice(billingErrorMessage(reason, b.errors.ledger))
      })
      .finally(() => { if (active) setLedgerLoading(false) })
    return () => { active = false }
  }, [ledgerOpen, open, account?.lifetime_charged_usd, b.errors.ledger])

  useEffect(() => {
    if (!open) return
    let active = true
    setStatementLoading(true)
    // An empty month asks for everything; the API then defaults `from` to its
    // own epoch rather than the current month.
    void getUserStatement(statementMonth ? { month: statementMonth } : { from: '2000-01-01' })
      .then((next) => { if (active) setStatement(next) })
      .catch((reason: unknown) => {
        if (!active) return
        setStatement(null)
        setNotice(billingErrorMessage(reason, b.errors.statement))
      })
      .finally(() => { if (active) setStatementLoading(false) })
    return () => { active = false }
  }, [open, statementMonth, account?.lifetime_charged_usd, b.errors.statement])

  const exportStatement = useCallback(async () => {
    setBusy('export')
    setNotice(null)
    try {
      await downloadUserStatementCSV(statementMonth ? { month: statementMonth } : { from: '2000-01-01' })
    } catch (reason: unknown) {
      setNotice(billingErrorMessage(reason, b.errors.export))
    } finally {
      setBusy(null)
    }
  }, [statementMonth, b.errors.export])

  const monthOptions = useMemo(() => recentMonthKeys(new Date()), [])

  const paymentsReady = paymentsEnabled && (plans?.payments_enabled ?? true)
  const memberActive = balance?.member_active ?? account?.member_active ?? false
  const availableUsd = balance?.available_usd ?? account?.available_usd ?? 0
  const walletUsd = balance?.wallet_usd ?? account?.wallet_usd ?? 0
  const grantUsd = balance?.grant_usd ?? account?.grant_usd ?? 0
  const hours = estimatedHours(account, balance)
  const standardHourly = useMemo<PlanHourlyExample | undefined>(
    () => plans?.hourly.find((example) => example.plan_code === FREE_PLAN_CODE),
    [plans],
  )
  const publicPlans = useMemo<Plan[]>(
    () => (plans?.plans ?? [])
      .filter((plan) => plan.is_public && plan.active && plan.code !== FREE_PLAN_CODE)
      .sort((a, b) => a.sort - b.sort),
    [plans],
  )
  const topupTiers = useMemo<TopupTier[]>(
    () => (plans?.topup_tiers ?? [])
      .filter((tier) => tier.active)
      .sort((a, b) => a.sort - b.sort || a.amount_usd - b.amount_usd),
    [plans],
  )
  const autoTopupAllowed = account?.effective_plan?.features?.auto_topup === true
  const checkoutCurrency = plans?.checkout_currency ?? 'usd'
  const checkoutRate = plans?.checkout_usd_rate ?? 1
  const chargedAs = (amountUSD: number) => formatCheckoutCharge(amountUSD, checkoutCurrency, checkoutRate)
  const settlementNote = checkoutCurrency !== 'usd'
    ? b.settlement(checkoutCurrency.toUpperCase(), checkoutRate)
    : null

  const redirect = useCallback(async (key: string, request: () => Promise<string>) => {
    setBusy(key)
    setNotice(null)
    try {
      const url = await request()
      if (!url) throw new Error(b.errors.invalidUrl)
      window.location.assign(url)
    } catch (reason) {
      setNotice(billingErrorMessage(reason, b.errors.openPayment))
      setBusy(null)
    }
  }, [b.errors.invalidUrl, b.errors.openPayment])

  const startTopup = (tier: TopupTier) => redirect(
    `topup:${tier.amount_usd}`,
    () => createBillingCheckout({ kind: 'topup', amount_usd: tier.amount_usd }),
  )
  const startMembership = (plan: Plan, interval: 'month' | 'year') => redirect(
    `plan:${plan.code}:${interval}`,
    () => createBillingCheckout({ kind: 'membership', plan_code: plan.code, interval }),
  )
  const openPortal = () => redirect('portal', createBillingPortal)

  const saveAutoTopup = async (enabled: boolean) => {
    setBusy('auto-topup')
    setNotice(null)
    try {
      const threshold = Number.parseFloat(autoTopupThreshold)
      const amount = Number.parseFloat(autoTopupAmount)
      if (enabled && (!Number.isFinite(threshold) || threshold < 0)) {
        throw new Error(b.errors.threshold)
      }
      if (enabled && (!Number.isFinite(amount) || amount <= 0)) {
        throw new Error(b.errors.amount)
      }
      await setUserAutoTopup(
        enabled
          ? { enabled: true, threshold_usd: threshold, amount_usd: amount }
          : { enabled: false },
      )
      await onRefreshAccount()
      setNotice(enabled ? b.autoTopupOn : b.autoTopupOff)
    } catch (reason) {
      setNotice(billingErrorMessage(reason, b.errors.autoTopup))
    } finally {
      setBusy(null)
    }
  }

  if (!account) {
    return (
      <div className="dt-billing-card">
        <p className="dt-muted">{b.loadingAccount}</p>
        <button
          className="dt-button dt-button--secondary dt-button--small"
          onClick={() => { void onRefreshAccount() }}
          type="button"
        >
          {b.reload}
        </button>
      </div>
    )
  }

  const discount = discountLabel(account.discount_percent)
  const membership = account.membership
  const memberUntil = balance?.member_until ?? account.member_until

  return (
    <>
      {notice && (
        <div className="dt-billing-notice" role="status">
          <span>{notice}</span>
          <button aria-label={b.closeNotice} onClick={() => setNotice(null)} type="button">
            <Icon name="close" size={15} />
          </button>
        </div>
      )}

      <section className="dt-billing-card" aria-label={b.balanceAria}>
        <div className="dt-billing-card__head">
          <div>
            <strong>{b.available}</strong>
            <small>{b.balanceHint}</small>
          </div>
          {memberActive && <span className="dt-pro-badge">Pro</span>}
        </div>
        <div className="dt-billing-amount">
          <strong>{formatUSD(availableUsd)}</strong>
          <span>{formatHours(hours)} {b.realtimeSuffix}</span>
        </div>
        <dl className="dt-billing-rows">
          <div>
            <dt>{b.wallet}</dt>
            <dd>{formatUSD(walletUsd)}</dd>
          </div>
          <div>
            <dt>{b.grant}</dt>
            <dd>{formatUSD(grantUsd)}</dd>
          </div>
          {account.grants.filter((grant) => grant.remaining_usd > 0).map((grant) => (
            <div className="dt-billing-rows__sub" key={grant.id}>
              <dt>
                {b.grants[grant.kind] ?? grant.kind}
                {grant.expires_at
                  ? <small>{b.expires(formatDate(grant.expires_at))}</small>
                  : <small>{b.neverExpires}</small>}
              </dt>
              <dd>{formatUSD(grant.remaining_usd)}</dd>
            </div>
          ))}
          <div>
            <dt>{b.hourlyPrice}</dt>
            <dd>
              {b.perHour(formatUSD(account.realtime_hour_usd))}
              {discount && <small>{discount}</small>}
              {(account.promotion_discount_percent ?? 0) > 0 && account.promotion_discount_until && (
                <small>{b.referral.discount(account.promotion_discount_percent!, formatDate(account.promotion_discount_until))}</small>
              )}
            </dd>
          </div>
        </dl>
      </section>

      {(account.signup_reward_status === 'review' || account.signup_reward_status === 'denied' || account.signup_reward_status === 'budget_hold') && <p className="dt-auth__offer" role="status">{account.signup_reward_status === 'budget_hold' ? m.auth.rewardBudgetHold : account.signup_reward_status === 'review' ? m.auth.rewardReview : m.auth.rewardDenied}</p>}

      <section className="dt-billing-card" aria-label={b.membership}>
        <div className="dt-billing-card__head">
          <div>
            <strong>{b.membership}</strong>
            <small>
              {b.currentPlan(account.effective_plan?.name || account.plan?.name || account.plan_code)}
              {memberActive && memberUntil ? b.validUntil(formatDate(memberUntil)) : ''}
            </small>
          </div>
        </div>
        {memberActive ? (
          <>
            {membership && (
              <p className="dt-muted">
                {membership.interval === 'year' ? b.yearlyBilling : b.monthlyBilling}
                {membership.current_period_start && membership.current_period_end
                  ? b.period(formatDate(membership.current_period_start), formatDate(membership.current_period_end))
                  : ''}
                {membership.cancel_at_period_end ? b.noRenew : ''}
                {membership.status && membership.status !== 'active' ? b.status(membership.status) : ''}
              </p>
            )}
            {!membership && memberUntil && (
              <p className="dt-muted">{b.manualMember(formatDate(memberUntil))}</p>
            )}
            <button
              className="dt-button dt-button--secondary dt-button--wide"
              disabled={!paymentsReady || busy === 'portal'}
              onClick={() => { void openPortal() }}
              title={paymentsReady ? undefined : b.paymentsDisabled}
              type="button"
            >
              {busy === 'portal' ? b.opening : b.manage}
            </button>
            {!paymentsReady && <p className="dt-muted">{b.paymentsDisabled}</p>}
          </>
        ) : publicPlans.length === 0 ? (
          <p className="dt-muted">{plans ? b.noPlans : b.loadingPlans}</p>
        ) : publicPlans.map((plan) => {
          const hourly = plans?.hourly.find((example) => example.plan_code === plan.code)
          const planDiscount = discountLabel(plan.usage_discount_percent)
          return (
            <div className="dt-billing-plan" key={plan.code}>
              <div className="dt-billing-plan__head">
                <strong>{plan.name}</strong>
                <span>
                  {plan.price_usd_month > 0 && `${formatUSD(plan.price_usd_month)}${chargedAs(plan.price_usd_month) ? ` (${chargedAs(plan.price_usd_month)})` : ''} ${b.perMonth}`}
                  {plan.price_usd_month > 0 && plan.price_usd_year > 0 && ' · '}
                  {plan.price_usd_year > 0 && `${formatUSD(plan.price_usd_year)}${chargedAs(plan.price_usd_year) ? ` (${chargedAs(plan.price_usd_year)})` : ''} ${b.perYear}`}
                </span>
              </div>
              {planDiscount && <span className="dt-billing-plan__discount">{planDiscount}</span>}
              {(plan.max_concurrent_sessions > 1 || plan.features.batch || plan.features.auto_topup) && (
                <ul className="dt-billing-plan__perks">
                  {plan.max_concurrent_sessions > 1 && <li>{b.perkConcurrent(plan.max_concurrent_sessions)}</li>}
                  {plan.features.batch && <li>{b.perkBatch}</li>}
                  {plan.features.auto_topup && <li>{b.perkAutoTopup}</li>}
                </ul>
              )}
              {hourly && (
                <p className="dt-muted">
                  {b.realtimeSuffix} {b.perHour(formatUSD(hourly.realtime_hour_usd))}
                  {standardHourly && standardHourly.realtime_hour_usd > hourly.realtime_hour_usd
                    ? b.standardPrice(formatUSD(standardHourly.realtime_hour_usd))
                    : ''}
                </p>
              )}
              <div className="dt-billing-actions">
                {plan.price_usd_month > 0 && (
                  <button
                    className="dt-button dt-button--primary"
                    disabled={!paymentsReady || busy !== null}
                    onClick={() => { void startMembership(plan, 'month') }}
                    title={paymentsReady ? undefined : b.paymentsDisabled}
                    type="button"
                  >
                    {busy === `plan:${plan.code}:month` ? b.redirecting : b.monthlyJoin}
                  </button>
                )}
                {plan.price_usd_year > 0 && (
                  <button
                    className="dt-button dt-button--secondary"
                    disabled={!paymentsReady || busy !== null}
                    onClick={() => { void startMembership(plan, 'year') }}
                    title={paymentsReady ? undefined : b.paymentsDisabled}
                    type="button"
                  >
                    {busy === `plan:${plan.code}:year` ? b.redirecting : b.yearlyJoin}
                  </button>
                )}
              </div>
              {!paymentsReady && <p className="dt-muted">{b.paymentsDisabled}</p>}
            </div>
          )
        })}
      </section>

      <section className="dt-billing-card" aria-label={b.topup}>
        <div className="dt-billing-card__head">
          <div>
            <strong>{b.topup}</strong>
            <small>{b.topupHint}{settlementNote ? `. ${settlementNote}` : ''}</small>
          </div>
        </div>
        {topupTiers.length === 0 ? (
          <p className="dt-muted">{plans ? b.noTiers : b.loadingTiers}</p>
        ) : (
          <div className="dt-billing-tiers">
            {topupTiers.map((tier) => (
              <button
                className="dt-button dt-button--secondary"
                disabled={!paymentsReady || busy !== null}
                key={tier.amount_usd}
                onClick={() => { void startTopup(tier) }}
                title={paymentsReady ? undefined : b.paymentsDisabled}
                type="button"
              >
                {busy === `topup:${tier.amount_usd}` ? b.redirecting : tierLabel(tier, chargedAs(tier.amount_usd))}
              </button>
            ))}
          </div>
        )}
        {!paymentsReady && <p className="dt-muted">{b.paymentsDisabled}</p>}
        {autoTopupAllowed && (
          <div className="dt-billing-autotopup">
            <label className={`dt-toggle${busy === 'auto-topup' ? ' is-disabled' : ''}`}>
              <span>
                <strong>{b.autoTopup}</strong>
                <small>
                  {account.has_payment_method
                    ? b.autoWithCard
                    : b.autoNeedsCard}
                </small>
              </span>
              <input
                checked={account.auto_topup_enabled}
                disabled={busy === 'auto-topup' || !paymentsReady}
                onChange={(event) => { void saveAutoTopup(event.target.checked) }}
                type="checkbox"
              />
              <span aria-hidden="true" className="dt-toggle__track"><span /></span>
            </label>
            <div className="dt-billing-autotopup__form">
              <label className="dt-field">
                <span>{b.below}</span>
                <input
                  inputMode="decimal"
                  min={0}
                  onChange={(event) => setAutoTopupThreshold(event.target.value)}
                  step="1"
                  type="number"
                  value={autoTopupThreshold}
                />
              </label>
              <label className="dt-field">
                <span>{b.autoAmount}</span>
                <input
                  inputMode="decimal"
                  min={1}
                  onChange={(event) => setAutoTopupAmount(event.target.value)}
                  step="1"
                  type="number"
                  value={autoTopupAmount}
                />
              </label>
              <button
                className="dt-button dt-button--secondary dt-button--small"
                disabled={busy === 'auto-topup' || !paymentsReady}
                onClick={() => { void saveAutoTopup(true) }}
                type="button"
              >
                {account.auto_topup_enabled ? b.save : b.enableSave}
              </button>
            </div>
          </div>
        )}
      </section>

      <section className="dt-billing-card" aria-label={b.referral.title}>
        <div className="dt-billing-card__head">
          <div>
            <strong>{b.referral.title}</strong>
            <small>{b.referral.lead}</small>
          </div>
        </div>
        {referral === null && <p className="dt-muted">{b.referral.loading}</p>}
        {referral === 'failed' && <p className="dt-muted">{b.referral.failed}</p>}
        {referral && referral !== 'failed' && (
          <>
            <label className="dt-field">
              <span>{b.referral.link}</span>
              <input
                aria-label={b.referral.link}
                onFocus={(event) => event.target.select()}
                readOnly
                value={`${window.location.origin}${referral.path}`}
              />
            </label>
            <div className="dt-billing-actions">
              <button
                className="dt-button dt-button--secondary dt-button--small"
                onClick={() => {
                  void navigator.clipboard.writeText(`${window.location.origin}${referral.path}`)
                    .then(() => { setReferralCopied(true); window.setTimeout(() => setReferralCopied(false), 2000) })
                    .catch(() => undefined)
                }}
                type="button"
              >
                {referralCopied ? b.referral.copied : b.referral.copy}
              </button>
              <a className="dt-button dt-button--secondary dt-button--small" href={referral.path} rel="noopener" target="_blank">
                {b.referral.poster}
              </a>
            </div>
            <dl className="dt-billing-rows">
              <div><dt>{b.referral.visits}</dt><dd>{referral.visits}</dd></div>
              <div><dt>{b.referral.registered}</dt><dd>{referral.registered}</dd></div>
              <div><dt>{b.referral.verified}</dt><dd>{referral.verified}</dd></div>
            </dl>
          </>
        )}
      </section>

      <section className="dt-billing-card dt-billing-statement" aria-label={b.statement}>
        <div className="dt-billing-statement__head">
          <strong>{b.statement}</strong>
          <select
            aria-label={b.statementPeriod}
            onChange={(event) => setStatementMonth(event.target.value)}
            value={statementMonth}
          >
            {monthOptions.map((key) => (
              <option key={key} value={key}>{monthLabel(key)}</option>
            ))}
            <option value={ALL_MONTHS}>{b.statementAll}</option>
          </select>
        </div>
        {statementLoading && !statement && <p className="dt-muted">{b.loadingStatement}</p>}
        {statement && (
          <>
            <div className="dt-billing-statement__total">
              <span>{b.statementSpend}</span>
              <strong>{formatUsageUSD(statement.totals.charged_usd)}</strong>
            </div>
            <StatementSplit rows={[
              [b.actions.transcription, statement.totals.transcription_usd],
              [b.actions.translation, statement.totals.translation_usd],
              [b.aiFeatures, statement.totals.ai_usd],
            ]} />
            {/* Money in and refunds are not spend; a second group keeps them
                from reading as another line of the total above. */}
            <StatementSplit rows={[
              [b.statementTopup, statement.totals.topup_usd],
              [b.statementMembership, statement.totals.membership_usd],
              [b.statementRefunded, statement.totals.refunded_usd],
            ]} />
            {statement.usage.length === 0 && statement.payments.length === 0 ? (
              <p className="dt-muted">{b.statementEmpty}</p>
            ) : (
              <p className="dt-muted">{b.statementCounts(statement.usage.length, statement.payments.length)}</p>
            )}
            {statement.truncated && <p className="dt-billing-statement__warn">{b.statementTruncated}</p>}
          </>
        )}
        <button
          className="dt-button dt-button--secondary dt-button--wide"
          disabled={busy === 'export'}
          onClick={() => { void exportStatement() }}
          type="button"
        >
          <Icon name="download" size={14} />
          {busy === 'export' ? b.exporting : b.exportCsv}
        </button>
        <small className="dt-billing-statement__hint">{b.exportHint}</small>
      </section>

      <section className="dt-account-usage" aria-label={b.usageAria}>
        <div>
          <strong>{b.recentUsage}</strong>
          <small>{b.usageHint}</small>
        </div>
        {sessionCost && sessionCost.total_usd > 0 && (
          <div className="dt-account-usage__row dt-account-usage__row--session">
            <span>
              <strong>{b.thisSession}</strong>
              <small>
                {[
                  sessionCost.transcription_usd > 0
                    ? b.transcriptionCost(formatUsageUSD(sessionCost.transcription_usd), Math.max(1, Math.round(sessionCost.transcription_seconds / 60)))
                    : '',
                  sessionCost.translation_usd > 0
                    ? b.translationCost(formatUsageUSD(sessionCost.translation_usd))
                    : '',
                  sessionCost.ai_usd > 0
                    ? b.aiCost(formatUsageUSD(sessionCost.ai_usd))
                    : '',
                ].filter(Boolean).join(' · ')}
              </small>
            </span>
            <strong>{formatUsageUSD(sessionCost.transcription_usd + sessionCost.translation_usd)}</strong>
          </div>
        )}
        {usage.length === 0 ? (
          <p className="dt-muted">{b.noUsage}</p>
        ) : usage.map((item) => (
          <div className="dt-account-usage__row" key={item.id}>
            <span>
              <strong>{(b.actions as Record<string, string>)[item.action] ?? item.action}</strong>
              <small>
                {[usageModelLabel(item), formatDateTime(item.created_at)]
                  .filter(Boolean)
                  .join(' · ')}
                {' · '}
                <em className={`dt-usage-source dt-usage-source--${item.grant_usd > 0 ? 'grant' : 'wallet'}`}>
                  {paidFromLabel(item)}
                </em>
              </small>
            </span>
            <strong>{formatUsageUSD(item.cost_usd)}</strong>
          </div>
        ))}
        <button
          className="dt-button dt-button--text dt-button--small"
          onClick={() => setLedgerOpen((value) => !value)}
          type="button"
        >
          {ledgerOpen ? b.collapseLedger : b.viewLedger}
        </button>
        {ledgerOpen && (
          <div className="dt-billing-ledger">
            {ledgerLoading && !ledger && <p className="dt-muted">{b.loadingLedger}</p>}
            {ledger && ledger.payments.length > 0 && (
              <>
                <small className="dt-billing-ledger__title">{b.paymentRecords}</small>
                {ledger.payments.map((payment) => (
                  <div className="dt-account-usage__row" key={payment.id}>
                    <span>
                      <strong>{b.paymentKinds[payment.kind] ?? payment.kind}</strong>
                      <small>
                        {payment.description || payment.status} · {formatDateTime(payment.created_at)}
                        {payment.bonus_usd > 0 ? b.bonus(formatUSD(payment.bonus_usd)) : ''}
                      </small>
                    </span>
                    <strong>{formatUSD(payment.amount_usd)}</strong>
                  </div>
                ))}
              </>
            )}
            {ledger && (
              <>
                <small className="dt-billing-ledger__title">{b.balanceChanges}</small>
                {ledger.ledger.length === 0 && <p className="dt-muted">{b.noChanges}</p>}
                {mergeUsageLedger(ledger.ledger).map((row) => {
                  const entry = row.primary
                  // Float residue below display precision reads as zero.
                  const netUSD = Math.abs(row.netUSD) < 0.000005 ? 0 : row.netUSD
                  const fullyRefunded = row.merged && netUSD === 0
                  return (
                    <div className="dt-account-usage__row" key={row.key}>
                      <span>
                        <strong>
                          {b.ledgerTypes[entry.transaction_type] ?? entry.transaction_type}
                          {' · '}
                          {ledgerBucketLabel(row.buckets)}
                        </strong>
                        <small>
                          {(entry.description || '—').replace(' usage settlement', '')}
                          {row.merged && (fullyRefunded ? b.fullyRefunded : b.settled)}
                          {' · '}
                          {formatDateTime(entry.created_at)}
                        </small>
                      </span>
                      <strong className={netUSD < 0 ? 'dt-billing-debit' : 'dt-billing-credit'}>
                        {netUSD > 0 ? '+' : ''}{formatUsageUSD(netUSD)}
                      </strong>
                    </div>
                  )
                })}
              </>
            )}
          </div>
        )}
      </section>
    </>
  )
}
