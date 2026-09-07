// Authentication API wrapper for DreamTrans Pro
import { clearUserApiKey } from '../../utils/userApiKey'
import { messages } from '../../i18n'
import { collectSignupSignals } from './signupSignals'

const BACKEND_URL = import.meta.env.VITE_BACKEND_URL || 'http://localhost:8080'
const isProduction = BACKEND_URL === '/'
const baseUrl = isProduction ? '' : BACKEND_URL

// Token storage keys
const ACCESS_TOKEN_KEY = 'dt_access_token'
const REFRESH_TOKEN_KEY = 'dt_refresh_token'
const USER_KEY = 'dt_user'
const AUTH_SYNC_KEY = 'dt_auth_sync_v1'
export const AUTH_STATE_CHANGED_EVENT = 'dt-auth-changed'
const AUTH_REQUEST_TIMEOUT_MS = 20_000
const AUTH_REFRESH_TIMEOUT_MS = 12_000
const AUTH_REFRESH_LOCK_TIMEOUT_MS = 15_000

const authMemoryStorage = new Map<string, string>()
let authStorageUnavailable = false
let authGeneration = 0
let authSyncInstalled = false
let legacyStorageSyncTimer: ReturnType<typeof globalThis.setTimeout> | null = null
let lastObservedUserId: string | null | undefined
let authSyncSequence = 0
const authTabId = globalThis.crypto?.randomUUID?.()
  ?? `tab-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`

interface RefreshFlight {
  generation: number
  refreshToken: string
  promise: Promise<boolean>
}

let refreshInFlight: RefreshFlight | null = null

function getAuthStorage(): Storage | null {
  if (authStorageUnavailable) return null
  try {
    if (typeof localStorage === 'undefined') {
      authStorageUnavailable = true
      return null
    }
    return localStorage
  } catch {
    authStorageUnavailable = true
    return null
  }
}

function readAuthStorage(key: string): string | null {
  const storage = getAuthStorage()
  if (!storage) return authMemoryStorage.get(key) ?? null
  try {
    const value = storage.getItem(key)
    if (value === null) authMemoryStorage.delete(key)
    else authMemoryStorage.set(key, value)
    return value
  } catch {
    authStorageUnavailable = true
    return authMemoryStorage.get(key) ?? null
  }
}

function writeAuthStorage(key: string, value: string): void {
  authMemoryStorage.set(key, value)
  const storage = getAuthStorage()
  if (!storage) return
  try {
    storage.setItem(key, value)
  } catch {
    // Keep all subsequent auth reads and writes on the coherent in-memory
    // mirror instead of mixing it with a partially writable storage backend.
    authStorageUnavailable = true
  }
}

function removeAuthStorage(key: string): void {
  authMemoryStorage.delete(key)
  const storage = getAuthStorage()
  if (!storage) return
  try {
    storage.removeItem(key)
  } catch {
    authStorageUnavailable = true
  }
}

function advanceAuthGeneration(): number {
  authGeneration += 1
  // Detach the old flight without attempting to cancel its network request.
  // Its identity-checked finally block must not clear a newer flight.
  refreshInFlight = null
  return authGeneration
}

function isCurrentAuthGeneration(generation: number): boolean {
  return authGeneration === generation
}

interface TimedAuthFetch {
  response: Response
  release: () => void
}

async function fetchWithAuthTimeout(
  input: RequestInfo | URL,
  init: RequestInit = {},
  timeoutMs = AUTH_REQUEST_TIMEOUT_MS,
): Promise<TimedAuthFetch> {
  const controller = new AbortController()
  const externalSignal = init.signal
  const forwardExternalAbort = () => {
    controller.abort(externalSignal?.reason)
  }
  if (externalSignal?.aborted) {
    forwardExternalAbort()
  } else {
    externalSignal?.addEventListener('abort', forwardExternalAbort, { once: true })
  }
  const timeout = globalThis.setTimeout(() => {
    controller.abort(new DOMException(
      `Authentication request timed out after ${timeoutMs} ms`,
      'TimeoutError',
    ))
  }, timeoutMs)
  let released = false
  const release = () => {
    if (released) return
    released = true
    globalThis.clearTimeout(timeout)
    externalSignal?.removeEventListener('abort', forwardExternalAbort)
  }
  try {
    const response = await fetch(input, {
      ...init,
      signal: controller.signal,
    })
    return { response, release }
  } catch (reason) {
    release()
    throw reason
  }
}

// Types
export interface User {
  id: string
  tenant_id: string
  email: string
  name: string
  role: 'user' | 'admin' | 'super_admin'
  is_active: boolean
  email_verified: boolean
  /** Training-programme answer: null = not asked yet, false = declined, true = joined. */
  training_opt_in: boolean | null
  last_login_at?: string
  created_at: string
  updated_at: string
}

export interface AuthStateChangedDetail {
  external: boolean
  identityChanged: boolean
  reason: 'login' | 'logout' | 'profile' | 'refresh' | 'storage' | 'tokens'
  userId: string | null
}

function readStoredUser(): User | null {
  const raw = readAuthStorage(USER_KEY)
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as User
    return typeof parsed?.id === 'string' && parsed.id ? parsed : null
  } catch {
    removeAuthStorage(USER_KEY)
    return null
  }
}

function dispatchAuthStateChanged(
  external: boolean,
  reason: AuthStateChangedDetail['reason'],
): void {
  const user = readStoredUser()
  const userId = user?.id ?? null
  const identityChanged = lastObservedUserId !== undefined
    && lastObservedUserId !== userId
  if (!userId || identityChanged) clearUserApiKey()
  lastObservedUserId = userId

  if (typeof window === 'undefined') return
  try {
    window.dispatchEvent(new CustomEvent<AuthStateChangedDetail>(
      AUTH_STATE_CHANGED_EVENT,
      { detail: { external, identityChanged, reason, userId } },
    ))
    if (!userId) window.dispatchEvent(new CustomEvent('dt-auth-cleared'))
  } catch {
    // Auth state is already committed even in restricted/test runtimes where
    // CustomEvent dispatch is unavailable.
  }
}

function synchronizeExternalAuthState(): void {
  const nextUserId = readStoredUser()?.id ?? null
  if (lastObservedUserId !== nextUserId) {
    // Account switches and logout invalidate every request that captured the
    // previous identity. A same-user token rotation can safely keep requests
    // alive; authFetch will retry a 401 with the newly stored access token.
    advanceAuthGeneration()
  } else {
    refreshInFlight = null
  }
  dispatchAuthStateChanged(true, 'storage')
}

function ensureAuthSyncListener(): void {
  if (
    authSyncInstalled
    || typeof window === 'undefined'
    || typeof window.addEventListener !== 'function'
  ) {
    return
  }
  authSyncInstalled = true
  lastObservedUserId = readStoredUser()?.id ?? null
  window.addEventListener('storage', (event) => {
    if (event.key === AUTH_SYNC_KEY) {
      if (legacyStorageSyncTimer !== null) {
        globalThis.clearTimeout(legacyStorageSyncTimer)
        legacyStorageSyncTimer = null
      }
      synchronizeExternalAuthState()
      return
    }
    if (event.key !== USER_KEY) return
    // New clients publish AUTH_SYNC_KEY after all three auth values have been
    // committed. This short fallback also interoperates with an already-open
    // tab running an older bundle, whose final write is dt_user.
    if (legacyStorageSyncTimer !== null) {
      globalThis.clearTimeout(legacyStorageSyncTimer)
    }
    legacyStorageSyncTimer = globalThis.setTimeout(() => {
      legacyStorageSyncTimer = null
      synchronizeExternalAuthState()
    }, 25)
  })
}

function publishAuthState(reason: AuthStateChangedDetail['reason']): void {
  ensureAuthSyncListener()
  authSyncSequence += 1
  writeAuthStorage(
    AUTH_SYNC_KEY,
    JSON.stringify({
      source: authTabId,
      sequence: authSyncSequence,
      timestamp: Date.now(),
    }),
  )
  dispatchAuthStateChanged(false, reason)
}

export interface Tenant {
  id: string
  name: string
  slug: string
  plan: 'free' | 'pro' | 'enterprise'
  api_quota_monthly: number
  storage_quota_gb: number
  max_sessions: number
  created_at: string
  updated_at: string
}

export interface AuthResponse {
  user: User
  access_token: string
  refresh_token: string
  expires_in: number
}

export interface Session {
  id: string
  user_id: string
  tenant_id: string
  title: string
  source_language: string
  target_language: string
  duration_seconds: number
  status: 'active' | 'paused' | 'completed' | 'archived'
  /** AI project (course) this session is linked to, when any. */
  project_id?: string
  started_at: string
  ended_at?: string
  created_at: string
  updated_at: string
}

export interface Transcript {
  id: string
  session_id: string
  client_segment_id: string
  translation_group_id?: string
  speaker: string
  text: string
  translation?: string
  start_time: number
  end_time?: number
  status: 'partial' | 'confirmed' | 'translated'
  is_partial: boolean
  created_at: string
  updated_at: string
}

export interface SessionWithTranscripts extends Session {
  transcripts: Transcript[]
}

export interface GetSessionOptions {
  includeTranscripts?: boolean
}

export interface TranscriptPageCursor {
  start_time: number
  id: string
}

export interface TranscriptPageResponse {
  transcripts: Transcript[]
  has_more: boolean
  next_cursor: TranscriptPageCursor | null
}

export interface TranscriptPageOptions {
  limit?: number
  after?: TranscriptPageCursor | null
}

export interface ApiError {
  error: string
}

/**
 * Error carrying the HTTP status of a failed API request so callers (e.g. the
 * cloud transcript outbox) can distinguish permanent rejections from
 * transient failures worth retrying.
 */
export class ApiRequestError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiRequestError'
    this.status = status
  }
}

/**
 * Authentication could not be checked because the service or network is
 * temporarily unavailable. Callers must not interpret this as an invalid
 * refresh token or erase locally cached credentials.
 */
export class AuthUnavailableError extends Error {
  readonly status?: number

  constructor(message = 'Authentication service temporarily unavailable', status?: number) {
    super(message)
    this.name = 'AuthUnavailableError'
    this.status = status
  }
}

export interface SpeechmaticsPreflightResponse {
  ready: true
}

export interface TranscriptInput {
  client_segment_id: string
  translation_group_id?: string
  speaker?: string
  text: string
  translation?: string
  start_time: number
  end_time?: number
  status?: 'partial' | 'confirmed' | 'translated'
  is_partial?: boolean
}

// Token management
export function getAccessToken(): string | null {
  ensureAuthSyncListener()
  return readAuthStorage(ACCESS_TOKEN_KEY)
}

export function getRefreshToken(): string | null {
  ensureAuthSyncListener()
  return readAuthStorage(REFRESH_TOKEN_KEY)
}

function storeTokens(accessToken: string, refreshToken: string): void {
  writeAuthStorage(ACCESS_TOKEN_KEY, accessToken)
  writeAuthStorage(REFRESH_TOKEN_KEY, refreshToken)
}

function storeUser(user: User): void {
  writeAuthStorage(USER_KEY, JSON.stringify(user))
}

export function setTokens(accessToken: string, refreshToken: string): void {
  advanceAuthGeneration()
  storeTokens(accessToken, refreshToken)
  publishAuthState('tokens')
}

export function clearTokens(): void {
  advanceAuthGeneration()
  removeAuthStorage(ACCESS_TOKEN_KEY)
  removeAuthStorage(REFRESH_TOKEN_KEY)
  removeAuthStorage(USER_KEY)
  clearUserApiKey()
  publishAuthState('logout')
}

export function getStoredUser(): User | null {
  ensureAuthSyncListener()
  return readStoredUser()
}

export function setStoredUser(user: User): void {
  storeUser(user)
  publishAuthState('profile')
}

function commitAuthResponse(generation: number, data: AuthResponse): boolean {
  if (!isCurrentAuthGeneration(generation)) return false
  // A second edge at commit time invalidates refreshes that began while the
  // login/register request itself was pending.
  advanceAuthGeneration()
  storeTokens(data.access_token, data.refresh_token)
  storeUser(data.user)
  publishAuthState('login')
  return true
}

function clearTokensIfCurrent(generation: number): boolean {
  if (!isCurrentAuthGeneration(generation)) return false
  clearTokens()
  return true
}

function tokenExpiresWithin(token: string, seconds: number): boolean {
  try {
    const encoded = token.split('.')[1]
    if (!encoded) return true
    const normalized = encoded.replace(/-/g, '+').replace(/_/g, '/')
    const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, '=')
    const payload = JSON.parse(atob(padded)) as { exp?: number }
    return typeof payload.exp !== 'number' || payload.exp <= Math.floor(Date.now() / 1000) + seconds
  } catch {
    return true
  }
}

function authStateChangedError(): Error {
  return new Error('Authentication state changed')
}

/**
 * Return an access token that will remain valid for at least the requested
 * window. Refresh-token rotation is protected by a module-wide single flight,
 * so reconnecting sockets and API requests cannot invalidate each other.
 */
export async function ensureValidAccessToken(minValiditySeconds = 60): Promise<string> {
  const generation = authGeneration
  const token = getAccessToken()
  if (!token) throw new Error('Not authenticated')
  if (!tokenExpiresWithin(token, minValiditySeconds)) return token

  const refreshedSuccessfully = await refreshAccessToken()
  if (!isCurrentAuthGeneration(generation)) {
    throw authStateChangedError()
  }
  if (!refreshedSuccessfully) {
    clearTokensIfCurrent(generation)
    throw new Error('Session expired')
  }

  const refreshed = getAccessToken()
  if (!refreshed) {
    clearTokensIfCurrent(generation)
    throw new Error('Session expired')
  }
  return refreshed
}

// Fetch wrapper with auth
export async function authFetch<T>(
  endpoint: string,
  options: RequestInit = {},
  acceptedStatuses: readonly number[] = [],
): Promise<T> {
  const generation = authGeneration
  const token = getAccessToken()
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }

  // Merge existing headers
  if (options.headers) {
    const existingHeaders = options.headers as Record<string, string>
    Object.assign(headers, existingHeaders)
  }

  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  const request = await fetchWithAuthTimeout(`${baseUrl}${endpoint}`, {
    ...options,
    headers,
  })
  const { response } = request
  if (!isCurrentAuthGeneration(generation)) {
    request.release()
    throw authStateChangedError()
  }

  // Handle token expiration
  if (response.status === 401 && token) {
    request.release()
    // Another request may already have rotated the refresh token while this
    // request was in flight. In that case retry with the newer access token
    // instead of issuing a second refresh.
    const currentToken = getAccessToken()
    const refreshed = (!!currentToken && token !== currentToken) || await refreshAccessToken()
    if (!isCurrentAuthGeneration(generation)) {
      throw authStateChangedError()
    }
    if (refreshed) {
      // Retry with new token
      const retryToken = getAccessToken()
      if (!retryToken) {
        clearTokensIfCurrent(generation)
        throw new Error('Session expired')
      }
      headers['Authorization'] = `Bearer ${retryToken}`
      const retryRequest = await fetchWithAuthTimeout(`${baseUrl}${endpoint}`, {
        ...options,
        headers,
      })
      try {
        const retryResponse = retryRequest.response
        if (!isCurrentAuthGeneration(generation)) {
          throw authStateChangedError()
        }
        if (!retryResponse.ok && !acceptedStatuses.includes(retryResponse.status)) {
          const error = await retryResponse.json().catch(() => ({ error: 'Request failed' }))
          throw new ApiRequestError(error.error || 'Request failed', retryResponse.status)
        }
        return await retryResponse.json()
      } finally {
        retryRequest.release()
      }
    } else {
      clearTokensIfCurrent(generation)
      throw new Error('Session expired')
    }
  }

  try {
    if (!response.ok && !acceptedStatuses.includes(response.status)) {
      const error = await response.json().catch(() => ({ error: 'Request failed' }))
      throw new ApiRequestError(error.error || 'Request failed', response.status)
    }
    return await response.json()
  } finally {
    request.release()
  }
}

// Token refresh
function newerCredentialsAreUsable(refreshToken: string): boolean {
  return (
    getRefreshToken() !== refreshToken &&
    !!getAccessToken()
  )
}

async function waitForCredentialRotation(
  refreshToken: string,
  timeoutMs = 500,
): Promise<boolean> {
  if (newerCredentialsAreUsable(refreshToken)) return true
  if (typeof window === 'undefined' || typeof window.addEventListener !== 'function') {
    return false
  }
  return new Promise<boolean>((resolve) => {
    let settled = false
    const finish = () => {
      if (settled) return
      settled = true
      globalThis.clearTimeout(timeout)
      window.removeEventListener(AUTH_STATE_CHANGED_EVENT, handleChange)
      resolve(newerCredentialsAreUsable(refreshToken))
    }
    const handleChange = () => finish()
    const timeout = globalThis.setTimeout(finish, timeoutMs)
    window.addEventListener(AUTH_STATE_CHANGED_EVENT, handleChange)
  })
}

async function performTokenRefresh(
  generation: number,
  refreshToken: string,
  signal?: AbortSignal,
): Promise<boolean> {
  try {
    const request = await fetchWithAuthTimeout(`${baseUrl}/api/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: refreshToken }),
      signal,
    }, AUTH_REFRESH_TIMEOUT_MS)
    try {
      const { response } = request
      if (!response.ok) {
        if (response.status === 401 || response.status === 403) {
          // Another tab may have consumed this rotating refresh token a few
          // milliseconds earlier. Give its storage notification time to arrive
          // before declaring the shared login invalid.
          return newerCredentialsAreUsable(refreshToken)
            || await waitForCredentialRotation(refreshToken)
        }
        throw new AuthUnavailableError(
          `Authentication service temporarily unavailable (HTTP ${response.status})`,
          response.status,
        )
      }

      const data: AuthResponse = await response.json()
      if (
        !data?.access_token
        || !data.refresh_token
        || !data.user?.id
      ) {
        throw new AuthUnavailableError('Authentication refresh returned an invalid response')
      }
      if (!isCurrentAuthGeneration(generation) || getRefreshToken() !== refreshToken) {
        return newerCredentialsAreUsable(refreshToken)
      }
      storeTokens(data.access_token, data.refresh_token)
      storeUser(data.user)
      publishAuthState('refresh')
      return true
    } finally {
      request.release()
    }
  } catch (reason) {
    if (newerCredentialsAreUsable(refreshToken)) return true
    if (reason instanceof AuthUnavailableError) throw reason
    const message = reason instanceof Error && reason.message
      ? `Authentication service temporarily unavailable: ${reason.message}`
      : 'Authentication service temporarily unavailable'
    throw new AuthUnavailableError(message)
  }
}

async function performTokenRefreshWithLock(
  generation: number,
  refreshToken: string,
): Promise<boolean> {
  const lockManager = typeof navigator === 'undefined' ? undefined : navigator.locks
  if (!lockManager) return performTokenRefresh(generation, refreshToken)
  const controller = new AbortController()
  const timeout = globalThis.setTimeout(() => {
    controller.abort(new DOMException(
      `Authentication refresh lock timed out after ${AUTH_REFRESH_LOCK_TIMEOUT_MS} ms`,
      'TimeoutError',
    ))
  }, AUTH_REFRESH_LOCK_TIMEOUT_MS)
  try {
    return await lockManager.request(
      'dreamtrans-auth-refresh',
      { signal: controller.signal },
      async () => {
        if (newerCredentialsAreUsable(refreshToken)) return true
        if (!isCurrentAuthGeneration(generation)) return false
        return performTokenRefresh(generation, refreshToken, controller.signal)
      },
    )
  } catch (reason) {
    if (newerCredentialsAreUsable(refreshToken)) return true
    if (reason instanceof AuthUnavailableError) throw reason
    throw new AuthUnavailableError(
      reason instanceof Error
        ? `Authentication service temporarily unavailable: ${reason.message}`
        : 'Authentication service temporarily unavailable',
    )
  } finally {
    globalThis.clearTimeout(timeout)
  }
}

export function refreshAccessToken(): Promise<boolean> {
  const generation = authGeneration
  const refreshToken = getRefreshToken()
  if (!refreshToken) return Promise.resolve(false)
  if (
    refreshInFlight &&
    refreshInFlight.generation === generation &&
    refreshInFlight.refreshToken === refreshToken
  ) {
    return refreshInFlight.promise
  }

  const flight: RefreshFlight = {
    generation,
    refreshToken,
    promise: Promise.resolve(false),
  }
  flight.promise = performTokenRefreshWithLock(generation, refreshToken).finally(() => {
    if (refreshInFlight === flight) {
      refreshInFlight = null
    }
  })
  refreshInFlight = flight
  return flight.promise
}

/** Error from an auth endpoint carrying the server's machine-readable code. */
export class AuthRequestError extends Error {
  readonly code: string | undefined
  readonly status: number

  constructor(message: string, status: number, code?: string) {
    super(message)
    this.name = 'AuthRequestError'
    this.status = status
    this.code = code
  }
}

async function submitAuthRequest<T = AuthResponse>(
  endpoint: '/api/auth/login' | '/api/auth/register' | '/api/auth/verify-email' | '/api/auth/resend-verification',
  payload: Record<string, string | ReturnType<typeof collectSignupSignals>>,
  action: keyof ReturnType<typeof messages>['common']['authActions'],
  fallbackError: string,
): Promise<T> {
  let request: TimedAuthFetch | undefined
  try {
    request = await fetchWithAuthTimeout(`${baseUrl}${endpoint}`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
    const { response } = request
    if (!response.ok) {
      const error = await response.json().catch(() => ({ error: fallbackError })) as {
        error?: string
        code?: string
      }
      throw new AuthRequestError(error.error || fallbackError, response.status, error.code)
    }
    return await response.json() as T
  } catch (reason) {
    if (
      reason instanceof DOMException
      && (reason.name === 'TimeoutError' || reason.name === 'AbortError')
    ) {
      throw new Error(messages().common.errors.authTimeout(
        messages().common.authActions[action],
      ), { cause: reason })
    }
    if (reason instanceof TypeError) {
      throw new Error(
        messages().common.errors.authNetwork(messages().common.authActions[action]),
        { cause: reason },
      )
    }
    throw reason
  } finally {
    request?.release()
  }
}

/** Registration created the account but the address must be confirmed first. */
export interface RegistrationPending {
  reward_review_required?: boolean
  verification_required: true
  email: string
  /** False when the account exists but the mail could not be delivered. */
  email_sent: boolean
}

export type RegisterResult =
  | { kind: 'signed-in'; session: AuthResponse }
  | { kind: 'verification-pending'; pending: RegistrationPending }

function isRegistrationPending(data: unknown): data is RegistrationPending {
  return typeof data === 'object' && data !== null
    && (data as { verification_required?: unknown }).verification_required === true
}

// Auth API
export async function register(
  email: string,
  password: string,
  name: string,
  inviteCode?: string,
  referralCode?: string,
): Promise<RegisterResult> {
  const generation = advanceAuthGeneration()
  // Refresh the signed first-party registration cookie immediately before
  // submitting. Missing/blocked cookies are handled as review by the server.
  try {
    const context = await fetchWithAuthTimeout(`${baseUrl}/api/auth/signup-context`, { credentials: 'include' })
    try { await context.response.body?.cancel() } finally { context.release() }
  } catch { /* The server will hold rewards if the device cannot be verified. */ }
  if (!isCurrentAuthGeneration(generation)) throw authStateChangedError()
  const normalizedInviteCode = inviteCode?.trim()
  const normalizedReferralCode = referralCode?.trim()
  const data = await submitAuthRequest<AuthResponse | RegistrationPending>(
    '/api/auth/register',
    {
      email,
      password,
      name,
      ...(normalizedInviteCode ? { invite_code: normalizedInviteCode } : {}),
      ...(normalizedReferralCode ? { referral_code: normalizedReferralCode } : {}),
      browser: collectSignupSignals(),
    },
    'register',
    'Registration failed',
  )
  if (isRegistrationPending(data)) {
    return { kind: 'verification-pending', pending: data }
  }
  if (!commitAuthResponse(generation, data)) {
    throw authStateChangedError()
  }
  return { kind: 'signed-in', session: data }
}

/** Redeems an emailed verification link and signs the user in. */
export async function verifyEmail(token: string): Promise<AuthResponse> {
  const generation = advanceAuthGeneration()
  const data = await submitAuthRequest(
    '/api/auth/verify-email',
    { token },
    'verify',
    'Verification failed',
  )
  if (!commitAuthResponse(generation, data)) {
    throw authStateChangedError()
  }
  return data
}

/** Asks for a new verification link; the server never reveals whether the address exists. */
export async function resendVerification(email: string): Promise<void> {
  await submitAuthRequest<{ accepted: boolean }>(
    '/api/auth/resend-verification',
    { email },
    'send',
    'Failed to send verification email',
  )
}

export async function login(email: string, password: string): Promise<AuthResponse> {
  const generation = advanceAuthGeneration()
  const data = await submitAuthRequest(
    '/api/auth/login',
    { email, password },
    'login',
    'Login failed',
  )
  if (!commitAuthResponse(generation, data)) {
    throw authStateChangedError()
  }
  return data
}

export async function logout(): Promise<void> {
  const accessToken = getAccessToken()
  const refreshToken = getRefreshToken()
  // Invalidate local credentials and every older async auth writer before
  // waiting on a best-effort server-side revocation.
  clearTokens()
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }
  if (accessToken) {
    headers.Authorization = `Bearer ${accessToken}`
  }
  const controller = new AbortController()
  const timeout = globalThis.setTimeout(() => controller.abort(), 5_000)
  try {
    await fetch(`${baseUrl}/api/auth/logout`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ refresh_token: refreshToken }),
      signal: controller.signal,
    })
  } catch {
    // Ignore logout errors
  } finally {
    globalThis.clearTimeout(timeout)
  }
}

export async function getProfile(): Promise<{ user: User; tenant?: Tenant }> {
  return authFetch('/api/user/profile')
}

export async function checkSpeechmaticsPreflight(
  clientOrigin = '',
): Promise<SpeechmaticsPreflightResponse> {
  const normalizedOrigin = clientOrigin.trim()
  const endpoint = normalizedOrigin
    ? `/api/speechmatics/preflight?origin=${encodeURIComponent(normalizedOrigin)}`
    : '/api/speechmatics/preflight'
  const response = await authFetch<SpeechmaticsPreflightResponse>(
    endpoint,
    { cache: 'no-store' },
  )
  if (response.ready !== true) {
    throw new Error('Speechmatics preflight returned an invalid response')
  }
  return response
}

export async function updateProfile(name: string): Promise<User> {
  return authFetch('/api/user/profile', {
    method: 'PUT',
    body: JSON.stringify({ name }),
  })
}

/** Records whether the user joins the Speechmatics training programme. */
export async function updateTrainingOptIn(optIn: boolean): Promise<User> {
  return authFetch('/api/user/training-program', {
    method: 'PUT',
    body: JSON.stringify({ opt_in: optIn }),
  })
}

export async function updatePassword(
  currentPassword: string,
  newPassword: string
): Promise<void> {
  await authFetch('/api/user/password', {
    method: 'PUT',
    body: JSON.stringify({
      current_password: currentPassword,
      new_password: newPassword,
    }),
  })
}

// Session API
export async function listSessions(
  page = 1,
  pageSize = 20
): Promise<{ sessions: Session[]; page: number; page_size: number }> {
  return authFetch(`/api/sessions?page=${page}&page_size=${pageSize}`)
}

export async function createSession(data?: {
  client_session_id?: string
  title?: string
  source_language?: string
  target_language?: string
}): Promise<Session> {
  return authFetch('/api/sessions', {
    method: 'POST',
    body: JSON.stringify(data || {}),
  })
}

export async function getSession(
  id: string,
  options: GetSessionOptions = {},
): Promise<SessionWithTranscripts> {
  const params = new URLSearchParams()
  if (options.includeTranscripts !== undefined) {
    params.set('include_transcripts', String(options.includeTranscripts))
  }
  const query = params.toString()
  return authFetch(
    `/api/sessions/${encodeURIComponent(id)}${query ? `?${query}` : ''}`,
  )
}

export async function getSessionTranscriptsPage(
  id: string,
  options: TranscriptPageOptions = {},
): Promise<TranscriptPageResponse> {
  const params = new URLSearchParams()
  if (options.limit !== undefined) {
    params.set('limit', String(options.limit))
  }
  if (options.after) {
    params.set('after_start_time', String(options.after.start_time))
    params.set('after_id', options.after.id)
  }
  const query = params.toString()
  return authFetch(
    `/api/sessions/${encodeURIComponent(id)}/transcripts${query ? `?${query}` : ''}`,
  )
}

export async function updateSession(
  id: string,
  data: {
    title?: string
    status?: 'active' | 'paused' | 'completed' | 'archived'
    duration_seconds?: number
  },
  signal?: AbortSignal,
): Promise<Session> {
  return authFetch(`/api/sessions/${id}`, {
    method: 'PUT',
    body: JSON.stringify(data),
    signal,
  })
}

export async function deleteSession(id: string): Promise<void> {
  // DELETE is idempotent from the client's perspective. If a previous attempt
  // reached the server but local cleanup failed, retrying must still be able to
  // remove the IndexedDB cache and durable outbox.
  const retryDelays = [500, 1_500]
  for (let attempt = 0; ; attempt += 1) {
    try {
      await authFetch(`/api/sessions/${id}`, { method: 'DELETE' }, [404])
      return
    } catch (reason) {
      const transientHTTP = reason instanceof ApiRequestError
        && reason.status >= 502
        && reason.status <= 504
      const transientNetwork = reason instanceof TypeError
        || (reason instanceof DOMException
          && (reason.name === 'AbortError' || reason.name === 'TimeoutError'))
      if (
        attempt >= retryDelays.length
        || (!transientHTTP && !transientNetwork)
      ) throw reason
      await new Promise<void>((resolve) => {
        globalThis.setTimeout(resolve, retryDelays[attempt])
      })
    }
  }
}

export async function saveTranscript(
  sessionId: string,
  transcript: TranscriptInput,
): Promise<Transcript> {
  return authFetch(`/api/sessions/${sessionId}/transcripts`, {
    method: 'POST',
    body: JSON.stringify(transcript),
  })
}

export async function saveTranscriptsBatch(
  sessionId: string,
  transcripts: TranscriptInput[],
  signal?: AbortSignal,
): Promise<{ saved: Transcript[]; count: number }> {
  return authFetch(`/api/sessions/${sessionId}/transcripts/batch`, {
    method: 'POST',
    body: JSON.stringify(transcripts),
    signal,
  })
}

export async function exportSession(
  id: string,
  format: 'json' | 'txt' | 'srt' = 'json'
): Promise<Blob> {
  let token = await ensureValidAccessToken()
  const generation = authGeneration
  const request = () => fetch(`${baseUrl}/api/sessions/${id}/export?format=${format}`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  let response = await request()
  if (!isCurrentAuthGeneration(generation)) {
    throw authStateChangedError()
  }

  if (response.status === 401) {
    const currentToken = getAccessToken()
    if (currentToken && currentToken !== token) {
      token = currentToken
    } else {
      const refreshed = await refreshAccessToken()
      if (!isCurrentAuthGeneration(generation)) {
        throw authStateChangedError()
      }
      if (!refreshed) {
        clearTokensIfCurrent(generation)
        throw new Error('Session expired')
      }
      token = getAccessToken() || ''
    }
    if (!token) {
      clearTokensIfCurrent(generation)
      throw new Error('Session expired')
    }
    response = await request()
    if (!isCurrentAuthGeneration(generation)) {
      throw authStateChangedError()
    }
  }

  if (!response.ok) {
    const message = await response.text().catch(() => '')
    throw new Error(message || 'Export failed')
  }

  return response.blob()
}

// Check if user is authenticated
export function isAuthenticated(): boolean {
  return !!getAccessToken() && !!getStoredUser()
}

// Initialize - check and refresh token if needed
export async function initAuth(): Promise<User | null> {
  const generation = authGeneration
  const cachedUser = getStoredUser()
  const token = getAccessToken()
  const refreshToken = getRefreshToken()
  if (!token && !refreshToken) {
    if (cachedUser) clearTokensIfCurrent(generation)
    return null
  }

  if (!token && refreshToken) {
    try {
      const refreshed = await refreshAccessToken()
      if (!isCurrentAuthGeneration(generation)) return getStoredUser()
      if (refreshed) return getStoredUser()
      clearTokensIfCurrent(generation)
      return null
    } catch (reason) {
      if (!isCurrentAuthGeneration(generation)) return getStoredUser()
      if (reason instanceof AuthUnavailableError) {
        // Keep the owner scope and tab-scoped provider credential visible
        // during an outage. API calls will surface the connectivity failure.
        return cachedUser
      }
      throw reason
    }
  }

  if (!token) {
    clearTokensIfCurrent(generation)
    return null
  }

  // Validate the token when possible, but a network outage or backend 5xx is
  // not evidence that a locally cached login has expired.
  try {
    const { user: freshUser } = await getProfile()
    if (!isCurrentAuthGeneration(generation)) return getStoredUser()
    storeUser(freshUser)
    publishAuthState('profile')
    return freshUser
  } catch (reason) {
    if (!isCurrentAuthGeneration(generation)) return getStoredUser()
    // authFetch already attempted a refresh after a 401. It clears credentials
    // only when /api/auth/refresh itself definitively returns 401/403.
    if (!getAccessToken() && !getRefreshToken()) return null
    if (
      reason instanceof AuthUnavailableError
      || (reason instanceof ApiRequestError && ![401, 403].includes(reason.status))
      || reason instanceof TypeError
    ) {
      return cachedUser
    }
    if (reason instanceof ApiRequestError && [401, 403].includes(reason.status)) {
      clearTokensIfCurrent(generation)
      return null
    }
    // Fetch implementations do not consistently use TypeError for aborted or
    // unreachable requests. Preserve credentials for every unknown transport
    // failure; a genuine invalid token is handled by the explicit statuses
    // above.
    if (!(reason instanceof Error) || reason.message !== 'Session expired') {
      return cachedUser
    }
    clearTokensIfCurrent(generation)
    return null
  }
}

// ==================== Admin API ====================

// List all users (admin only)
export async function adminListUsers(): Promise<{ users: User[] }> {
  return authFetch('/api/admin/users')
}

// Update user (admin only)
export async function adminUpdateUser(
  userId: string,
  updates: { is_active?: boolean; role?: string }
): Promise<User> {
  return authFetch(`/api/admin/users/${userId}`, {
    method: 'PUT',
    body: JSON.stringify(updates),
  })
}

// Delete user (admin only)
export async function adminDeleteUser(userId: string): Promise<void> {
  await authFetch(`/api/admin/users/${userId}`, { method: 'DELETE' })
}
