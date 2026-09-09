// Typed API wrapper for the React administration console.
import { ensureValidAccessToken, getStoredUser } from '../pro/api/auth'

const BACKEND_URL = import.meta.env.VITE_BACKEND_URL || 'http://localhost:8080'
const isProduction = BACKEND_URL === '/'
const baseUrl = isProduction ? '' : BACKEND_URL
const ADMIN_READ_RETRY_DELAYS_MS = [250, 1_000] as const
const TRANSIENT_GATEWAY_STATUSES = new Set([502, 503, 504])

// ---------------------------------------------------------------------------
// Users & tenants
// ---------------------------------------------------------------------------

export interface User {
  id: string
  tenant_id: string
  email: string
  name: string
  role: 'user' | 'admin' | 'super_admin'
  is_active: boolean
  email_verified: boolean
  last_login_at?: string
  created_at: string
  updated_at: string
  /** Administrator pin to one provider account; '' follows the normal rules. */
  speechmatics_route?: '' | 'training' | 'standard'
}

export interface Tenant {
  id: string
  name: string
  slug: string
  plan: 'free' | 'pro' | 'enterprise'
  /** Institutions never reach the training provider account. */
  kind: 'personal' | 'institution'
  /** Administrator pin to one provider account; '' follows the normal rules. */
  speechmatics_route: '' | 'training' | 'standard'
  storage_quota_gb: number
  created_at: string
  updated_at: string
}

export interface UserListResponse {
  users: User[]
  total: number
  page: number
  page_size: number
}

export interface TenantListResponse {
  tenants: Tenant[]
  total: number
  page: number
  page_size: number
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

export interface AdminBasicStats {
  user_count: number
  tenant_count: number
  session_count: number
  transcript_count: number
}

export interface AdminBillingStats {
  total_users: number
  active_users: number
  total_sessions: number
  total_transcripts: number
  total_wallet_usd: number
  total_grant_usd: number
  total_charged_usd: number
  active_members: number
  usage_by_action: Record<string, number>
  usage_by_model: Record<string, number>
  month_charged_usd: number
  month_upstream_usd: number
  month_margin_usd: number
  month_topup_usd: number
  month_membership_usd: number
}

export interface AdminSystemStatsResponse {
  basic: AdminBasicStats
  billing: AdminBillingStats
  billing_error?: string
  time: string
}

export interface BillingAnalytics {
  month_key: string
  topup_revenue_usd: number
  membership_revenue_usd: number
  refunded_usd: number
  charged_usd: number
  charged_from_grant_usd: number
  charged_from_wallet_usd: number
  upstream_cost_usd: number
  margin_usd: number
  usage_count: number
  byok_usage_count: number
  active_members: number
  new_members: number
  outstanding_wallet_usd: number
  outstanding_grant_usd: number
}

// ---------------------------------------------------------------------------
// Costs, markup & catalog
// ---------------------------------------------------------------------------

export interface MarkupOverride {
  scope_type: 'provider' | 'category' | 'sku'
  scope_key: string
  markup_percent: number
}

export interface MarkupInput {
  default_markup_percent: number
  overrides: MarkupOverride[]
}

export interface BillingConfig extends MarkupInput {
  catalog_version: string
  updated_at: string
  updated_by?: string
}

export type CostSource = 'public_catalog' | 'contract_override' | 'manual'

export interface CostRate {
  id?: string
  provider: string
  sku: string
  service: string
  unit_type: string
  public_cost_per_unit_usd: number
  effective_cost_per_unit_usd: number
  cost_source: CostSource | string
  cost_source_label: string
  cost_override_id?: string
  catalog_version: string
  source_url: string
  effective_at: string
  is_builtin: boolean
  is_active: boolean
  markup_percent: number
  markup_source: string
  retail_per_unit_usd: number
}

export interface PlanHourlyExample {
  plan_code: string
  plan_name: string
  discount_percent: number
  realtime_hour_usd: number
  realtime_upstream_usd: number
  realtime_gross_margin_percent: number
}

export interface BillingCatalog {
  config: BillingConfig
  rates: CostRate[]
  plan_examples: PlanHourlyExample[]
  builtin_catalog_version: string
}

export interface CostOverrideInput {
  provider: string
  sku: string
  service: string
  unit_type: string
  cost_per_unit_usd: number
  source_label?: string
  effective_at?: string
}

export interface ModelCostInput {
  provider: string
  model: string
  service: 'llm' | 'embedding'
  input_per_million: number
  cached_input_per_million: number
  cache_write_per_million: number
  output_per_million: number
}

// ---------------------------------------------------------------------------
// Plans, top-up tiers & accounts
// ---------------------------------------------------------------------------

export const planFeatureKeys = [
  'premium_models',
  'byok',
  'batch',
  'custom_prompt',
  'auto_topup',
  'export_ledger',
  'api_access',
] as const

export type PlanFeatureKey = typeof planFeatureKeys[number]

export interface Plan {
  code: string
  name: string
  is_public: boolean
  active: boolean
  sort: number
  price_usd_month: number
  price_usd_year: number
  stripe_price_id_month?: string
  stripe_price_id_year?: string
  usage_discount_percent: number
  storage_gb: number
  retention_days: number
  max_concurrent_sessions: number
  seats: number
  features: Record<string, boolean>
  created_at?: string
  updated_at?: string
}

export interface TopupTier {
  amount_usd: number
  bonus_percent: number
  bonus_expiry_days: number
  stripe_price_id?: string
  active: boolean
  sort: number
}

export type GrantKind = 'trial' | 'topup_bonus' | 'promo' | 'adjustment' | 'settle_return'

export interface GrantItem {
  id: string
  kind: GrantKind
  amount_usd: number
  remaining_usd: number
  expires_at?: string
  note?: string
  created_at: string
}

export interface Membership {
  id: string
  plan_code: string
  interval: 'month' | 'year'
  stripe_subscription_id?: string
  status: string
  current_period_start?: string
  current_period_end?: string
  cancel_at_period_end: boolean
}

export interface AccountBalance {
  user_id: string
  account_id: string
  wallet_usd: number
  grant_usd: number
  available_usd: number
  lifetime_charged_usd: number
  plan_code: string
  member_active: boolean
  member_until?: string
  auto_topup_enabled: boolean
}

export type AccountStatus = 'active' | 'past_due' | 'suspended'

export interface AccountSummary extends AccountBalance {
  email: string
  name: string
  status: AccountStatus
  plan: Plan
  effective_plan: Plan
  discount_percent: number
  grants: GrantItem[]
  stripe_customer_id?: string
  has_payment_method: boolean
  auto_topup_threshold_usd?: number
  auto_topup_amount_usd?: number
  storage_bytes: number
  realtime_hour_usd: number
  estimated_realtime_hours: number
  custom_discount_percent?: number
  custom_markup_percent?: number
  membership?: Membership
}

export interface UserUsageItem {
  id: string
  session_id: string | null
  action: string
  model: string
  quantity: number
  input_tokens: number
  cached_input_tokens: number
  cache_write_tokens: number
  output_tokens: number
  cost_usd: number
  grant_usd: number
  wallet_usd: number
  attribution: string
  settled: boolean
  refunded: boolean
  created_at: string
}

export interface AdminUsageItem extends UserUsageItem {
  upstream_cost_usd: number
  margin_usd: number
}

export interface BalanceTransaction {
  id: string
  user_id: string
  bucket: 'wallet' | 'grant'
  grant_id?: string | null
  amount_usd: number
  balance_after_usd: number
  transaction_type: 'credit' | 'debit' | 'refund' | 'adjustment'
  reference_type: string | null
  reference_id: string | null
  description: string
  created_by: string | null
  created_at: string
}

export interface PaymentRow {
  id: string
  kind: 'topup' | 'membership' | 'refund'
  amount_usd: number
  bonus_usd: number
  stripe_object_id?: string
  status: string
  description: string
  created_at: string
}

export interface CustomerRow {
  promotion_name?: string
  promotion_channel?: string
  promotion_tags?: string[]
  user_id: string
  account_id: string
  email: string
  name: string
  role: string
  plan_code: string
  member_active: boolean
  member_until?: string
  status: AccountStatus
  wallet_usd: number
  grant_usd: number
  lifetime_charged_usd: number
  month_charged_usd: number
  created_at: string
  referrer_email?: string
  referrer_name?: string
}

export interface CustomerListResponse {
  customers: CustomerRow[]
  total: number
}

export interface CustomerDetail {
  account: AccountSummary
  ledger: BalanceTransaction[]
  usage: AdminUsageItem[]
  payments: PaymentRow[]
}

export interface GrantCreditInput {
  amount_usd: number
  kind?: 'promo' | 'adjustment' | 'trial'
  expiry_days?: number
  expires_at?: string
  note?: string
}

export interface WalletAdjustmentInput {
  amount: number
  description: string
  allow_negative?: boolean
}

export interface PlanAssignmentInput {
  plan_code: string
  member_until?: string
  custom_discount_percent?: number | null
  custom_markup_percent?: number | null
  note?: string
}

// ---------------------------------------------------------------------------
// Model catalog
// ---------------------------------------------------------------------------

export interface ModelPolicy {
  purpose: 'translation' | 'summary' | 'chat' | 'embedding'
  model_id: string
  is_approved: boolean
  is_default: boolean
  cost_confirmed: boolean
}

export type ProviderAvailability =
  | 'provider_confirmed'
  | 'builtin_unverified'
  | 'temporarily_unavailable'
  | 'unavailable'
  | 'confirmed'
  | 'unverified'
  | 'stale'

export interface ProviderModel {
  provider: string
  model_id: string
  /** The id policies and preferences use: bare for the default provider, `provider::model` otherwise. */
  qualified_id?: string
  source: string
  provider_available: boolean
  availability_status?: ProviderAvailability
  first_seen_at: string
  last_seen_at: string
  policies: ModelPolicy[]
}

export interface ProviderSyncStatus {
  provider: string
  status?: string
  last_success_at?: string
  last_attempt_at?: string
  last_error?: string
}

export interface ModelCatalog {
  /** Default provider fields; `providers` lists every configured endpoint. */
  provider: string
  status?: string
  models: ProviderModel[]
  last_success_at?: string
  last_attempt_at?: string
  last_error?: string
  refresh_minutes: number
  providers?: ProviderSyncStatus[]
}

// ---------------------------------------------------------------------------
// System settings
// ---------------------------------------------------------------------------

export interface SystemSettingsValues {
  billing_enabled: boolean
  allow_negative_balance: boolean
  allow_user_api_key: boolean
  trial_credit_usd: number
  trial_credit_days: number
  /** Transcription discount for users who join the training programme. */
  training_discount_percent: number
  /** Global pause switch for the programme. */
  training_program_enabled: boolean
  /** Whether gift balance may earn the programme discount (default off). */
  gift_training_discount: boolean
}

export interface SystemSettingsResponse {
  values: SystemSettingsValues
  defaults: SystemSettingsValues
}

export interface SystemSettingsResetPreview {
  current: SystemSettingsValues
  defaults: SystemSettingsValues
  changes: Array<{
    key: keyof SystemSettingsValues
    from: boolean | number
    to: boolean | number
  }>
}

export type SystemSettingsPatch = Partial<SystemSettingsValues>

type LegacySettings = Partial<Record<keyof SystemSettingsValues, string | number | boolean>>

const defaultSystemSettings: SystemSettingsValues = {
  billing_enabled: true,
  allow_negative_balance: false,
  allow_user_api_key: false,
  trial_credit_usd: 1,
  trial_credit_days: 30,
  training_discount_percent: 20,
  training_program_enabled: true,
  gift_training_discount: false,
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

/** Formats a USD amount as `US$1.23` (thousands separated, fixed digits); the prefix disambiguates from the Stripe settlement currency. */
export function formatUSD(value: number, digits = 2): string {
  const amount = Number.isFinite(value) ? value : 0
  const safeDigits = Math.max(0, Math.min(8, Math.trunc(digits)))
  const magnitude = Math.abs(amount)
  const rounded = Number(magnitude.toFixed(safeDigits))
  const sign = amount < 0 && rounded > 0 ? '-' : ''
  const text = new Intl.NumberFormat('en-US', {
    minimumFractionDigits: safeDigits,
    maximumFractionDigits: safeDigits,
  }).format(rounded)
  return `${sign}US$${text}`
}

/** Formats a single usage charge: 4 digits for sub-cent amounts, otherwise 2. */
export function formatUsageUSD(value: number): string {
  const magnitude = Math.abs(Number.isFinite(value) ? value : 0)
  return formatUSD(value, magnitude > 0 && magnitude < 0.01 ? 4 : 2)
}

/** Formats an hour estimate as `≈ 12.5 小时` or `≈ 45 分钟`. */
export function formatHours(hours: number): string {
  const safeHours = Number.isFinite(hours) && hours > 0 ? hours : 0
  if (safeHours < 1) return `≈ ${Math.round(safeHours * 60)} 分钟`
  return `≈ ${Math.round(safeHours * 10) / 10} 小时`
}

/** Token-priced units are edited per million tokens; everything else per unit. */
export function costEditorScale(unitType: string): number {
  return unitType.includes('token') ? 1_000_000 : 1
}

export function getModelRateCostPerMillion(
  rates: CostRate[],
  provider: string,
  service: 'llm' | 'embedding',
  modelId: string,
  unitType: string,
): number | null {
  const rate = rates.find((item) => (
    item.provider === provider
    && item.is_active
    && item.service === service
    && item.sku === modelId
    && item.unit_type === unitType
  ))
  return rate ? rate.effective_cost_per_unit_usd * 1_000_000 : null
}

/** Converts a `datetime-local` input value into RFC3339 (UTC); null when blank/invalid. */
export function datetimeLocalToRFC3339(value: string): string | null {
  if (!value.trim()) return null
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString()
}

// ---------------------------------------------------------------------------
// Client-side validation
// ---------------------------------------------------------------------------

const PERCENT_LIMIT = 100_000

export function validateMarkupInput(input: MarkupInput): string | null {
  if (
    !Number.isFinite(input.default_markup_percent)
    || input.default_markup_percent < 0
    || input.default_markup_percent > PERCENT_LIMIT
  ) {
    return '默认加价率必须在 0 到 100000 之间'
  }
  const seen = new Set<string>()
  for (const override of input.overrides) {
    if (!['provider', 'category', 'sku'].includes(override.scope_type)) {
      return '分级加价的范围类型无效'
    }
    const scopeKey = override.scope_key.trim()
    if (!scopeKey) return '请填写每一条分级加价的匹配值，或删除空白项'
    if (scopeKey.length > 260) return '分级加价的匹配值不能超过 260 个字符'
    if (
      !Number.isFinite(override.markup_percent)
      || override.markup_percent < 0
      || override.markup_percent > PERCENT_LIMIT
    ) {
      return '分级加价率必须在 0 到 100000 之间'
    }
    const key = `${override.scope_type}\0${scopeKey}`
    if (seen.has(key)) return '同一范围不能添加重复的分级加价'
    seen.add(key)
  }
  return null
}

export function validateCostOverrideInput(
  input: CostOverrideInput,
  nowMs = Date.now(),
): string | null {
  if (!input.provider.trim() || !input.sku.trim() || !input.service.trim()) {
    return '合同成本缺少 Provider、SKU 或服务标识'
  }
  const provider = input.provider.trim()
  const sku = input.sku.trim()
  const service = input.service.trim()
  const unitType = input.unit_type.trim()
  if (provider.length > 60 || sku.length > 200 || service.length > 50) {
    return '合同成本的 Provider、SKU 或服务标识过长'
  }
  if (![
    'hour',
    'minute',
    'input_token',
    'cached_input_token',
    'cache_write_token',
    'output_token',
  ].includes(unitType)) {
    return '合同成本的计量单位无效'
  }
  if ([provider, sku, service, unitType].join(':').length > 320) {
    return '合同成本标识过长'
  }
  if (
    !Number.isFinite(input.cost_per_unit_usd)
    || input.cost_per_unit_usd < 0
    || input.cost_per_unit_usd >= 100_000_000
  ) {
    return '合同成本必须是有效的非负数字'
  }
  if ((input.source_label || '').trim().length > 120) return '成本来源不能超过 120 个字符'
  if (input.effective_at) {
    const effectiveAtMs = Date.parse(input.effective_at)
    if (!Number.isFinite(effectiveAtMs)) return '生效时间格式无效'
    if (effectiveAtMs > nowMs + 5 * 60_000) return '生效时间不能设为未来时间'
  }
  return null
}

export function validatePlanInput(plan: Plan): string | null {
  const code = plan.code.trim().toLowerCase()
  if (!code || code.length > 40) return '套餐代码必填，且不能超过 40 个字符'
  if (!/^[a-z0-9_-]+$/.test(code)) return '套餐代码只能包含 a-z、0-9、下划线和连字符'
  const name = plan.name.trim()
  if (!name || name.length > 100) return '套餐名称必填，且不能超过 100 个字符'
  const prices = [plan.price_usd_month, plan.price_usd_year]
  if (prices.some((price) => !Number.isFinite(price) || price < 0 || price > 1_000_000)) {
    return '套餐价格必须是 0 到 1000000 之间的数字'
  }
  if (
    !Number.isFinite(plan.usage_discount_percent)
    || plan.usage_discount_percent < 0
    || plan.usage_discount_percent > 100
  ) {
    return '用量折扣必须在 0 到 100 之间'
  }
  const limits = [plan.storage_gb, plan.retention_days, plan.max_concurrent_sessions]
  if (limits.some((limit) => !Number.isInteger(limit) || limit < -1)) {
    return '存储、保留天数和并发数必须是整数，-1 表示不限'
  }
  if (!Number.isInteger(plan.seats) || plan.seats < 1) return '席位数必须是不小于 1 的整数'
  if ((plan.stripe_price_id_month || '').trim().length > 120
    || (plan.stripe_price_id_year || '').trim().length > 120) {
    return 'Stripe 价格 ID 不能超过 120 个字符'
  }
  return null
}

export function validateTopupTierInput(tier: TopupTier): string | null {
  if (!Number.isFinite(tier.amount_usd) || tier.amount_usd <= 0 || tier.amount_usd > 100_000) {
    return '充值金额必须是大于 0 且不超过 100000 的数字'
  }
  if (!Number.isFinite(tier.bonus_percent) || tier.bonus_percent < 0 || tier.bonus_percent > 100) {
    return '赠送比例必须在 0 到 100 之间'
  }
  if (
    !Number.isInteger(tier.bonus_expiry_days)
    || tier.bonus_expiry_days < 1
    || tier.bonus_expiry_days > 3650
  ) {
    return '赠送有效期必须是 1 到 3650 天之间的整数'
  }
  if ((tier.stripe_price_id || '').trim().length > 120) return 'Stripe 价格 ID 不能超过 120 个字符'
  if (!Number.isInteger(tier.sort)) return '排序必须是整数'
  return null
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

export class AdminAPIError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'AdminAPIError'
    this.status = status
  }
}

function abortError(signal: AbortSignal): Error {
  return signal.reason instanceof Error
    ? signal.reason
    : new DOMException('Request aborted', 'AbortError')
}

function waitForAdminRetry(delayMs: number, signal?: AbortSignal | null): Promise<void> {
  if (signal?.aborted) return Promise.reject(abortError(signal))
  return new Promise((resolve, reject) => {
    const timer = globalThis.setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, delayMs)
    const onAbort = () => {
      globalThis.clearTimeout(timer)
      reject(abortError(signal as AbortSignal))
    }
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

function isRetryableAdminRead(options: RequestInit): boolean {
  const method = (options.method || 'GET').toUpperCase()
  return method === 'GET' || method === 'HEAD'
}

function isRetryableNetworkError(reason: unknown): boolean {
  return reason instanceof TypeError
    || (reason instanceof DOMException
      && (reason.name === 'NetworkError' || reason.name === 'TimeoutError'))
}

export async function adminFetch<T>(endpoint: string, options: RequestInit = {}): Promise<T> {
  const identity = getStoredUser()?.id
  const token = await ensureValidAccessToken()
  const retryable = isRetryableAdminRead(options)
  let response: Response
  for (let attempt = 0; ; attempt += 1) {
    try {
      if (getStoredUser()?.id !== identity) throw new AdminAPIError('账户已切换，请刷新页面', 401)
      response = await fetch(`${baseUrl}${endpoint}`, {
        ...options,
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
          ...(options.headers || {}),
        },
      })
    } catch (reason) {
      if (
        !retryable
        || attempt >= ADMIN_READ_RETRY_DELAYS_MS.length
        || options.signal?.aborted
        || !isRetryableNetworkError(reason)
      ) {
        throw reason
      }
      await waitForAdminRetry(ADMIN_READ_RETRY_DELAYS_MS[attempt], options.signal)
      continue
    }

    if (
      retryable
      && TRANSIENT_GATEWAY_STATUSES.has(response.status)
      && attempt < ADMIN_READ_RETRY_DELAYS_MS.length
    ) {
      await response.body?.cancel().catch(() => undefined)
      await waitForAdminRetry(ADMIN_READ_RETRY_DELAYS_MS[attempt], options.signal)
      continue
    }
    break
  }

  if (getStoredUser()?.id !== identity) throw new AdminAPIError('账户已切换，请刷新页面', 401)
  if (!response.ok) {
    const error = await response.json().catch(() => ({ error: 'Request failed' }))
    if (response.status === 428 && error.code === 'confirmation_required' && new Headers(options.headers).get('X-Admin-Confirm') !== 'true') {
      if (!window.confirm(typeof error.error === 'string' && error.error ? error.error : '这项操作会影响价格、赠送额度或账户余额。请核对当前表单，确认后继续。')) throw new AdminAPIError('已取消，未作任何修改', 428)
      if (getStoredUser()?.id !== identity) throw new AdminAPIError('账户已切换，请刷新页面', 401)
      return adminFetch<T>(endpoint, { ...options, headers: { ...options.headers, 'X-Admin-Confirm': 'true' } })
    }
    throw new AdminAPIError(error.error || 'Request failed', response.status)
  }

  if (response.status === 204) return undefined as T
  return response.json()
}

// ---------------------------------------------------------------------------
// Settings normalization
// ---------------------------------------------------------------------------

function asBoolean(value: unknown, fallback: boolean): boolean {
  if (typeof value === 'boolean') return value
  if (typeof value === 'string') {
    if (value === 'true') return true
    if (value === 'false') return false
  }
  return fallback
}

function asFiniteNumber(value: unknown, fallback: number): number {
  const parsed = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(parsed) ? parsed : fallback
}

function normalizeSettingsValues(
  raw: LegacySettings | undefined,
  fallback: SystemSettingsValues,
): SystemSettingsValues {
  return {
    billing_enabled: asBoolean(raw?.billing_enabled, fallback.billing_enabled),
    allow_negative_balance: asBoolean(raw?.allow_negative_balance, fallback.allow_negative_balance),
    allow_user_api_key: asBoolean(raw?.allow_user_api_key, fallback.allow_user_api_key),
    trial_credit_usd: asFiniteNumber(raw?.trial_credit_usd, fallback.trial_credit_usd),
    trial_credit_days: asFiniteNumber(raw?.trial_credit_days, fallback.trial_credit_days),
    training_discount_percent: asFiniteNumber(raw?.training_discount_percent, fallback.training_discount_percent),
    training_program_enabled: asBoolean(raw?.training_program_enabled, fallback.training_program_enabled),
    gift_training_discount: asBoolean(raw?.gift_training_discount, fallback.gift_training_discount),
  }
}

export function normalizeSystemSettings(
  raw: SystemSettingsResponse | LegacySettings,
): SystemSettingsResponse {
  if ('values' in raw && 'defaults' in raw) {
    const defaults = normalizeSettingsValues(raw.defaults, defaultSystemSettings)
    return {
      defaults,
      values: normalizeSettingsValues(raw.values, defaults),
    }
  }
  return {
    defaults: defaultSystemSettings,
    values: normalizeSettingsValues(raw as LegacySettings, defaultSystemSettings),
  }
}

// ---------------------------------------------------------------------------
// Users & tenants
// ---------------------------------------------------------------------------

export async function listUsers(page = 1, pageSize = 20): Promise<UserListResponse> {
  return adminFetch(`/api/admin/users?page=${page}&page_size=${pageSize}`)
}

export async function createUser(input: {
  tenant_id?: string
  email: string
  password: string
  name: string
  role: 'user' | 'admin'
  initial_credit_usd?: number
}): Promise<User> {
  return adminFetch('/api/admin/users', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

export async function updateUser(
  id: string,
  data: { name?: string; role?: string; is_active?: boolean; email_verified?: boolean; speechmatics_route?: '' | 'training' | 'standard' },
): Promise<User> {
  return adminFetch(`/api/admin/users/${id}`, {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

export async function getUser(id: string): Promise<{ user: User; tenant?: Tenant }> {
  return adminFetch(`/api/admin/users/${id}`)
}

export async function listTenants(page = 1, pageSize = 20): Promise<TenantListResponse> {
  return adminFetch(`/api/admin/tenants?page=${page}&page_size=${pageSize}`)
}

export async function listAllTenants(pageSize = 100): Promise<Tenant[]> {
  const requestedPageSize = Math.min(100, Math.max(1, Math.trunc(pageSize)))
  const first = await listTenants(1, requestedPageSize)
  const tenants = [...first.tenants]
  const effectivePageSize = Math.max(1, first.page_size || requestedPageSize)
  const totalPages = Math.ceil(first.total / effectivePageSize)
  for (let page = 2; page <= totalPages; page += 1) {
    const result = await listTenants(page, effectivePageSize)
    tenants.push(...result.tenants)
  }
  return tenants
}

export async function updateTenant(
  id: string,
  data: { name?: string; plan?: string; storage_quota_gb?: number; kind?: 'personal' | 'institution'; speechmatics_route?: '' | 'training' | 'standard' },
): Promise<Tenant> {
  return adminFetch(`/api/admin/tenants/${id}`, {
    method: 'PUT',
    body: JSON.stringify(data),
  })
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

export async function getGlobalStats(): Promise<AdminSystemStatsResponse> {
  return adminFetch('/api/admin/stats')
}

export async function getBillingAnalytics(month?: string): Promise<BillingAnalytics> {
  const params = new URLSearchParams()
  if (month) params.set('month', month)
  const query = params.toString()
  return adminFetch(`/api/admin/billing/analytics${query ? `?${query}` : ''}`)
}

// ---------------------------------------------------------------------------
// Costs & markup
// ---------------------------------------------------------------------------

export async function getBillingCatalog(): Promise<BillingCatalog> {
  return adminFetch('/api/admin/billing/catalog')
}

export async function updateBillingMarkup(input: MarkupInput): Promise<BillingCatalog> {
  const validationError = validateMarkupInput(input)
  if (validationError) throw new Error(validationError)
  return adminFetch('/api/admin/billing/markup', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

export async function updateModelCost(input: ModelCostInput): Promise<BillingCatalog> {
  return adminFetch('/api/admin/billing/model-cost', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

export async function putCostOverride(input: CostOverrideInput): Promise<BillingCatalog> {
  const validationError = validateCostOverrideInput(input)
  if (validationError) throw new Error(validationError)
  return adminFetch('/api/admin/billing/cost-overrides', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

export async function deleteCostOverride(input: {
  provider: string
  sku: string
  service: string
  unit_type: string
}): Promise<BillingCatalog> {
  const params = new URLSearchParams(input)
  return adminFetch(`/api/admin/billing/cost-overrides?${params}`, { method: 'DELETE' })
}

// ---------------------------------------------------------------------------
// Plans & top-up tiers
// ---------------------------------------------------------------------------

export async function listPlans(): Promise<Plan[]> {
  const result = await adminFetch<{ plans: Plan[] }>('/api/admin/billing/plans')
  return result.plans || []
}

export async function upsertPlan(plan: Plan): Promise<Plan> {
  const validationError = validatePlanInput(plan)
  if (validationError) throw new Error(validationError)
  return adminFetch('/api/admin/billing/plans', {
    method: 'PUT',
    body: JSON.stringify(plan),
  })
}

export async function listTopupTiers(): Promise<TopupTier[]> {
  const result = await adminFetch<{ tiers: TopupTier[] }>('/api/admin/billing/topup-tiers')
  return result.tiers || []
}

export async function upsertTopupTier(tier: TopupTier): Promise<TopupTier[]> {
  const validationError = validateTopupTierInput(tier)
  if (validationError) throw new Error(validationError)
  const result = await adminFetch<{ tiers: TopupTier[] }>('/api/admin/billing/topup-tiers', {
    method: 'PUT',
    body: JSON.stringify(tier),
  })
  return result.tiers || []
}

export async function deleteTopupTier(amountUsd: number): Promise<TopupTier[]> {
  const params = new URLSearchParams({ amount_usd: String(amountUsd) })
  const result = await adminFetch<{ tiers: TopupTier[] }>(
    `/api/admin/billing/topup-tiers?${params}`,
    { method: 'DELETE' },
  )
  return result.tiers || []
}

// ---------------------------------------------------------------------------
// Customers
// ---------------------------------------------------------------------------

export async function listCustomers(input: {
  search?: string
  limit?: number
  offset?: number
} = {}): Promise<CustomerListResponse> {
  const params = new URLSearchParams()
  if (input.search?.trim()) params.set('search', input.search.trim())
  if (input.limit) params.set('limit', String(input.limit))
  if (input.offset) params.set('offset', String(input.offset))
  const query = params.toString()
  return adminFetch(`/api/admin/customers${query ? `?${query}` : ''}`)
}

export async function getCustomer(userId: string): Promise<CustomerDetail> {
  return adminFetch(`/api/admin/customers/${encodeURIComponent(userId)}`)
}

export async function grantCustomerCredit(
  userId: string,
  input: GrantCreditInput,
): Promise<CustomerDetail> {
  return adminFetch(`/api/admin/customers/${encodeURIComponent(userId)}/grant`, {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

export async function adjustCustomerWallet(
  userId: string,
  input: WalletAdjustmentInput,
): Promise<CustomerDetail> {
  return adminFetch(`/api/admin/customers/${encodeURIComponent(userId)}/adjust`, {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

export async function setCustomerPlan(
  userId: string,
  input: PlanAssignmentInput,
): Promise<CustomerDetail> {
  return adminFetch(`/api/admin/customers/${encodeURIComponent(userId)}/plan`, {
    method: 'PUT',
    body: JSON.stringify(input),
  })
}

// ---------------------------------------------------------------------------
// Model catalog
// ---------------------------------------------------------------------------

export async function getModelCatalog(): Promise<ModelCatalog> {
  return adminFetch('/api/admin/models')
}

export async function refreshModelCatalog(): Promise<ModelCatalog> {
  return adminFetch('/api/admin/models/refresh', { method: 'POST' })
}

export async function updateModelPolicy(policy: ModelPolicy): Promise<ModelCatalog> {
  return adminFetch('/api/admin/models/policies', {
    method: 'PUT',
    body: JSON.stringify(policy),
  })
}

// ---------------------------------------------------------------------------
// System settings
// ---------------------------------------------------------------------------

export async function getSystemSettings(): Promise<SystemSettingsResponse> {
  const raw = await adminFetch<SystemSettingsResponse | LegacySettings>('/api/admin/settings')
  return normalizeSystemSettings(raw)
}

export async function patchSystemSettings(
  patch: SystemSettingsPatch,
): Promise<SystemSettingsResponse> {
  const raw = await adminFetch<SystemSettingsResponse | LegacySettings | { success: boolean }>(
    '/api/admin/settings',
    {
      method: 'PATCH',
      body: JSON.stringify(patch),
    },
  )
  if ('values' in raw || 'billing_enabled' in raw) {
    return normalizeSystemSettings(raw as SystemSettingsResponse | LegacySettings)
  }
  return getSystemSettings()
}

export async function previewSystemSettingsReset(): Promise<SystemSettingsResetPreview> {
  const raw = await adminFetch<SystemSettingsResetPreview>(
    '/api/admin/settings/reset/preview',
  )
  return {
    ...raw,
    defaults: normalizeSettingsValues(raw.defaults, defaultSystemSettings),
    current: normalizeSettingsValues(raw.current, defaultSystemSettings),
  }
}

export async function resetSystemSettings(): Promise<SystemSettingsResponse> {
  const raw = await adminFetch<SystemSettingsResponse | LegacySettings>(
    '/api/admin/settings/reset',
    {
      method: 'POST',
      body: JSON.stringify({ confirm: true }),
    },
  )
  return normalizeSystemSettings(raw)
}

/** Programme membership at the moments the business tracks. */
export interface TrainingProgramStats {
  enabled: boolean
  discount_percent: number
  users_opted_in: number
  users_declined: number
  users_unanswered: number
  claimed_total: number
  claimed_opted_in: number
  first_topup_total: number
  first_topup_opted_in: number
  changed_after_first_topup: number
  gift_routed_hours: number
  training_hours: number
  standard_hours: number
  route_refunds_usd: number
}

export async function getTrainingProgramStats(): Promise<TrainingProgramStats> {
  return adminFetch('/api/admin/training-program')
}
