import {
  AdminAPIError,
  adminFetch,
  costEditorScale,
  datetimeLocalToRFC3339,
  formatHours,
  formatUSD,
  formatUsageUSD,
  getModelRateCostPerMillion,
  normalizeSystemSettings,
  putCostOverride,
  updateBillingMarkup,
  upsertPlan,
  upsertTopupTier,
  validateCostOverrideInput,
  validateMarkupInput,
  validatePlanInput,
  validateTopupTierInput,
  type AdminSystemStatsResponse,
  type BillingAnalytics,
  type CostRate,
  type CustomerRow,
  type Plan,
  type TopupTier,
} from './api'
import { clearTokens, setTokens } from '../pro/api/auth'

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(`admin verification failed: ${message}`)
}

// --- Money & time formatting -------------------------------------------------

assert(formatUSD(1.234) === 'US$1.23', 'USD defaults to two decimals')
assert(formatUSD(1234.5) === 'US$1,234.50', 'USD keeps thousands separators')
assert(formatUSD(0) === 'US$0.00', 'zero renders without a sign')
assert(formatUSD(-4.2) === '-US$4.20', 'negative amounts carry a leading minus')
assert(formatUSD(-0.001) === 'US$0.00', 'amounts that round to zero drop the minus sign')
assert(formatUSD(0.00042, 4) === 'US$0.0004', 'explicit digits are honoured')
assert(formatUSD(Number.NaN) === 'US$0.00', 'non-finite input renders as zero')
assert(!formatUSD(12).includes('DP'), 'DreamPoints never appear in money output')

assert(formatUsageUSD(0.0042) === 'US$0.0042', 'sub-cent usage charges use four decimals')
assert(formatUsageUSD(0.5) === 'US$0.50', 'usage charges at or above a cent use two decimals')
assert(formatUsageUSD(0) === 'US$0.00', 'zero usage charge uses two decimals')

assert(formatHours(12.5) === '≈ 12.5 小时', 'hours format with one decimal')
assert(formatHours(12.04) === '≈ 12 小时', 'hours drop a trailing .0')
assert(formatHours(0.75) === '≈ 45 分钟', 'sub-hour estimates render as minutes')
assert(formatHours(0) === '≈ 0 分钟', 'zero renders as zero minutes')
assert(formatHours(-3) === '≈ 0 分钟', 'negative estimates clamp to zero')

assert(costEditorScale('input_token') === 1_000_000, 'token units are edited per million')
assert(costEditorScale('hour') === 1, 'hour units are edited per unit')

const rfc3339 = datetimeLocalToRFC3339('2026-09-01T10:30')
assert(rfc3339 !== null && Number.isFinite(Date.parse(rfc3339)), 'datetime-local converts to RFC3339')
assert(rfc3339?.endsWith('Z'), 'RFC3339 output is expressed in UTC')
assert(datetimeLocalToRFC3339('   ') === null, 'blank datetime-local is null')
assert(datetimeLocalToRFC3339('not-a-date') === null, 'invalid datetime-local is null')

// --- System settings ----------------------------------------------------------

const legacy = normalizeSystemSettings({
  billing_enabled: 'false',
  allow_negative_balance: 'true',
  allow_user_api_key: 'false',
  trial_credit_usd: '3.5',
  trial_credit_days: '14',
})

assert(!legacy.values.billing_enabled, 'legacy boolean false is normalized')
assert(legacy.values.allow_negative_balance, 'legacy boolean true is normalized')
assert(legacy.values.trial_credit_usd === 3.5, 'legacy trial credit is normalized to a number')
assert(legacy.values.trial_credit_days === 14, 'legacy trial days are normalized to a number')
assert(legacy.defaults.trial_credit_usd === 1, 'safe signup default remains one dollar')
assert(legacy.defaults.trial_credit_days === 30, 'safe trial expiry default remains thirty days')
assert(legacy.values.training_discount_percent === 20, 'missing training discount falls back to twenty percent')

const typed = normalizeSystemSettings({
  values: {
    billing_enabled: true,
    allow_negative_balance: false,
    allow_user_api_key: true,
    trial_credit_usd: 2,
    trial_credit_days: 7,
    training_discount_percent: 25,
    training_program_enabled: true,
    gift_training_discount: false,
  },
  defaults: {
    billing_enabled: true,
    allow_negative_balance: false,
    allow_user_api_key: false,
    trial_credit_usd: 1,
    trial_credit_days: 30,
    training_discount_percent: 20,
    training_program_enabled: true,
    gift_training_discount: false,
  },
})

assert(typed.values.allow_user_api_key, 'typed settings remain typed')
assert(typed.values.trial_credit_days === 7, 'typed trial days are retained')
assert(typed.defaults.trial_credit_usd === 1, 'server reset defaults are retained')

const partial = normalizeSystemSettings({
  values: { billing_enabled: true, allow_negative_balance: false, allow_user_api_key: false, trial_credit_usd: 5 },
  defaults: { billing_enabled: true, allow_negative_balance: false, allow_user_api_key: false, trial_credit_usd: 1, trial_credit_days: 30 },
} as unknown as Parameters<typeof normalizeSystemSettings>[0])
assert(partial.values.trial_credit_days === 30, 'missing trial days fall back to the server default')

// --- Statistics contract -----------------------------------------------------

const stats: AdminSystemStatsResponse = {
  basic: { user_count: 3, tenant_count: 1, session_count: 7, transcript_count: 12 },
  billing: {
    total_users: 3,
    active_users: 2,
    total_sessions: 7,
    total_transcripts: 12,
    total_wallet_usd: 120.5,
    total_grant_usd: 8,
    total_charged_usd: 42.25,
    active_members: 1,
    usage_by_action: { transcription: 30 },
    usage_by_model: {},
    month_charged_usd: 10,
    month_upstream_usd: 6,
    month_margin_usd: 4,
    month_topup_usd: 50,
    month_membership_usd: 9.9,
  },
  time: '2026-08-31T00:00:00Z',
}

assert(stats.basic.user_count === 3, 'overview reads the nested basic statistics contract')
assert(formatUSD(stats.billing.month_charged_usd) === 'US$10.00', 'monthly charge is money formatted')

const analytics: BillingAnalytics = {
  month_key: '2026-08',
  topup_revenue_usd: 50,
  membership_revenue_usd: 9.9,
  refunded_usd: 0,
  charged_usd: 10,
  charged_from_grant_usd: 4,
  charged_from_wallet_usd: 6,
  upstream_cost_usd: 6,
  margin_usd: 4,
  usage_count: 12,
  byok_usage_count: 1,
  active_members: 1,
  new_members: 1,
  outstanding_wallet_usd: 120.5,
  outstanding_grant_usd: 8,
}
assert(
  Math.abs(analytics.charged_from_grant_usd + analytics.charged_from_wallet_usd - analytics.charged_usd) < 1e-9,
  'analytics charge buckets add up to the charged total',
)

const customer: CustomerRow = {
  user_id: 'u1',
  account_id: 'a1',
  email: 'a@example.com',
  name: 'A',
  role: 'user',
  plan_code: 'pro',
  member_active: true,
  member_until: '2026-12-31T00:00:00Z',
  status: 'active',
  wallet_usd: 12,
  grant_usd: 3,
  lifetime_charged_usd: 40,
  month_charged_usd: 5,
  created_at: '2026-01-01T00:00:00Z',
}
assert(customer.member_active && customer.plan_code === 'pro', 'customer rows carry membership state')

// --- Catalog rate helpers ----------------------------------------------------

const hourRate: CostRate = {
  provider: 'speechmatics',
  sku: 'speechmatics-realtime-enhanced',
  service: 'transcription',
  unit_type: 'hour',
  public_cost_per_unit_usd: 0.43,
  effective_cost_per_unit_usd: 0.31,
  cost_source: 'contract_override',
  cost_source_label: 'Enterprise 2026',
  cost_override_id: 'ov-1',
  catalog_version: '2026-08-01',
  source_url: 'https://www.speechmatics.com/pricing',
  effective_at: '2026-08-01T00:00:00Z',
  is_builtin: true,
  is_active: true,
  markup_percent: 50,
  markup_source: 'default',
  retail_per_unit_usd: 0.465,
}

assert(
  Math.abs(hourRate.effective_cost_per_unit_usd * (1 + hourRate.markup_percent / 100) - hourRate.retail_per_unit_usd) < 1e-9,
  'retail per unit equals effective cost times markup',
)

const sharedModelRates: CostRate[] = [
  { ...hourRate, sku: 'shared-model', service: 'llm', unit_type: 'input_token', effective_cost_per_unit_usd: 2e-6 },
  { ...hourRate, sku: 'shared-model', service: 'embedding', unit_type: 'input_token', effective_cost_per_unit_usd: 0.12e-6 },
]

assert(
  getModelRateCostPerMillion(sharedModelRates, 'llm', 'shared-model', 'input_token') === 2,
  'model cost lookup includes the llm service identity',
)
assert(
  Math.abs((getModelRateCostPerMillion(sharedModelRates, 'embedding', 'shared-model', 'input_token') ?? 0) - 0.12) < 1e-9,
  'switching service reloads the matching embedding cost',
)
assert(
  getModelRateCostPerMillion(sharedModelRates, 'llm', 'unknown-model', 'input_token') === null,
  'missing model rates are reported as null instead of zero',
)

// --- Validation --------------------------------------------------------------

const validMarkupInput = {
  default_markup_percent: 50,
  overrides: [{ scope_type: 'provider' as const, scope_key: 'openai', markup_percent: 25 }],
}
assert(validateMarkupInput(validMarkupInput) === null, 'a complete markup configuration passes validation')
assert(
  validateMarkupInput({ ...validMarkupInput, default_markup_percent: -1 }) !== null,
  'negative default markup is rejected',
)
assert(
  validateMarkupInput({
    ...validMarkupInput,
    overrides: [{ scope_type: 'provider', scope_key: '   ', markup_percent: 25 }],
  }) !== null,
  'a blank markup scope is rejected before submitting',
)
assert(
  validateMarkupInput({
    ...validMarkupInput,
    overrides: [
      { scope_type: 'sku', scope_key: 'gpt-5.6-sol', markup_percent: 25 },
      { scope_type: 'sku', scope_key: '  gpt-5.6-sol  ', markup_percent: 30 },
    ],
  }) !== null,
  'duplicate markup scopes are detected after trimming their keys',
)

const validCostOverrideInput = {
  provider: 'openai',
  sku: 'gpt-5.6-sol',
  service: 'llm',
  unit_type: 'input_token',
  cost_per_unit_usd: 2e-6,
}
const verificationNowMs = Date.parse('2026-08-02T00:00:00Z')
assert(
  validateCostOverrideInput(validCostOverrideInput, verificationNowMs) === null,
  'a cost override may omit effective_at and let the server choose the current time',
)
assert(
  validateCostOverrideInput({
    ...validCostOverrideInput,
    effective_at: '2026-08-02T00:05:01Z',
  }, verificationNowMs) !== null,
  'a cost override more than five minutes in the future is rejected client-side',
)

const validPlan: Plan = {
  code: 'pro',
  name: 'Pro',
  is_public: true,
  active: true,
  sort: 10,
  price_usd_month: 9.9,
  price_usd_year: 99,
  usage_discount_percent: 20,
  storage_gb: 10,
  retention_days: -1,
  max_concurrent_sessions: 3,
  seats: 1,
  features: { premium_models: true, byok: true },
}
assert(validatePlanInput(validPlan) === null, 'a complete plan passes validation')
assert(validatePlanInput({ ...validPlan, code: 'Pro Plan' }) !== null, 'plan codes with spaces or capitals are rejected')
assert(validatePlanInput({ ...validPlan, usage_discount_percent: 120 }) !== null, 'discount above 100% is rejected')
assert(validatePlanInput({ ...validPlan, storage_gb: -2 }) !== null, 'limits below -1 are rejected')
assert(validatePlanInput({ ...validPlan, seats: 0 }) !== null, 'zero seats is rejected')
assert(validatePlanInput({ ...validPlan, name: '' }) !== null, 'empty plan name is rejected')

const validTier: TopupTier = { amount_usd: 20, bonus_percent: 10, bonus_expiry_days: 90, active: true, sort: 10 }
assert(validateTopupTierInput(validTier) === null, 'a complete top-up tier passes validation')
assert(validateTopupTierInput({ ...validTier, amount_usd: 0 }) !== null, 'zero top-up amount is rejected')
assert(validateTopupTierInput({ ...validTier, bonus_percent: 101 }) !== null, 'bonus above 100% is rejected')
assert(validateTopupTierInput({ ...validTier, bonus_expiry_days: 0 }) !== null, 'bonus without expiry is rejected')
assert(validateTopupTierInput({ ...validTier, bonus_expiry_days: 1.5 }) !== null, 'fractional expiry days are rejected')

// --- Transport ---------------------------------------------------------------

const verificationTokenPayload = btoa(JSON.stringify({
  exp: Math.floor(Date.now() / 1_000) + 3_600,
}))
const originalFetch = globalThis.fetch
setTokens(`header.${verificationTokenPayload}.signature`, 'verification-refresh-token')
try {
  let validationFetchAttempts = 0
  globalThis.fetch = async () => {
    validationFetchAttempts += 1
    return Response.json({})
  }
  const rejected: unknown[] = []
  for (const attempt of [
    () => updateBillingMarkup({
      ...validMarkupInput,
      overrides: [{ scope_type: 'provider', scope_key: '', markup_percent: 25 }],
    }),
    () => putCostOverride({ ...validCostOverrideInput, effective_at: 'not-a-date' }),
    () => upsertPlan({ ...validPlan, code: '' }),
    () => upsertTopupTier({ ...validTier, amount_usd: -5 }),
  ]) {
    try {
      await attempt()
    } catch (reason) {
      rejected.push(reason)
    }
  }
  assert(rejected.length === 4 && rejected.every((reason) => reason instanceof Error), 'invalid billing writes report local errors')
  assert(validationFetchAttempts === 0, 'invalid billing writes never reach fetch')

  let markupBody = ''
  globalThis.fetch = async (_input, init) => {
    markupBody = String(init?.body || '')
    return Response.json({ config: { default_markup_percent: 50, catalog_version: 'v', overrides: [], updated_at: '' }, rates: [], plan_examples: [], builtin_catalog_version: 'v' })
  }
  const markupResult = await updateBillingMarkup(validMarkupInput)
  assert(markupResult.config.default_markup_percent === 50, 'markup update returns the refreshed catalog')
  assert(!markupBody.includes('dp_per_usd'), 'markup payload no longer carries DreamPoints conversion')
  assert(markupBody.includes('"default_markup_percent":50'), 'markup payload carries the default markup')

  let gatewayAttempts = 0
  globalThis.fetch = async () => {
    gatewayAttempts += 1
    if (gatewayAttempts === 1) {
      return new Response('temporary gateway failure', { status: 502 })
    }
    return Response.json({ recovered: true })
  }
  const gatewayResult = await adminFetch<{ recovered: boolean }>('/api/admin/stats')
  assert(gatewayResult.recovered, 'an idempotent admin read recovers after a gateway 502')
  assert(gatewayAttempts === 2, 'a gateway 502 triggers one bounded read retry')

  let networkAttempts = 0
  globalThis.fetch = async () => {
    networkAttempts += 1
    if (networkAttempts === 1) throw new TypeError('Failed to fetch')
    return Response.json({ recovered: true })
  }
  const networkResult = await adminFetch<{ recovered: boolean }>('/api/admin/stats')
  assert(networkResult.recovered, 'an idempotent admin read recovers after a network failure')
  assert(networkAttempts === 2, 'a network failure triggers one bounded read retry')

  let writeAttempts = 0
  globalThis.fetch = async () => {
    writeAttempts += 1
    return Response.json({ error: 'temporary gateway failure' }, { status: 502 })
  }
  let writeError: unknown
  try {
    await adminFetch('/api/admin/users/user-1', { method: 'DELETE' })
  } catch (reason) {
    writeError = reason
  }
  assert(writeError instanceof AdminAPIError, 'a failed admin write preserves its API error')
  assert(writeAttempts === 1, 'non-idempotent admin writes are never automatically replayed')

  globalThis.fetch = async () => Response.json({ error: 'insufficient balance' }, { status: 402 })
  let insufficient: unknown
  try {
    await adminFetch('/api/admin/customers/u1/adjust', { method: 'POST', body: '{}' })
  } catch (reason) {
    insufficient = reason
  }
  assert(
    insufficient instanceof AdminAPIError && insufficient.status === 402 && insufficient.message === 'insufficient balance',
    'error bodies surface the server message and status',
  )
} finally {
  globalThis.fetch = originalFetch
  clearTokens()
}
