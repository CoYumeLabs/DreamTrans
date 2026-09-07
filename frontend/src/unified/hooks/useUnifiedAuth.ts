import { useCallback, useEffect, useRef, useState } from 'react'
import {
  getSystemAccess,
  getUserBalance,
  getUserBillingAccount,
  TRAINING_PROGRAM_UNAVAILABLE,
  type AccountBalance,
  type AccountSummary,
  type TrainingProgramInfo,
} from '../../api'
import {
  AUTH_STATE_CHANGED_EVENT,
  AuthRequestError,
  getStoredUser,
  initAuth,
  login as loginRequest,
  logout as logoutRequest,
  register as registerRequest,
  resendVerification as resendVerificationRequest,
  setStoredUser,
  updateTrainingOptIn as updateTrainingOptInRequest,
  verifyEmail as verifyEmailRequest,
  type User,
} from '../../pro/api/auth'
import { getSystemSettings } from '../../pro/api/system'
import { messages } from '../../i18n'

export interface RegisterInput {
  email: string
  password: string
  name: string
  inviteCode?: string
  /** Attribution only: the referral code from a friend's link. */
  referralCode?: string
}

/** An account waiting for its emailed verification link to be clicked. */
export interface PendingVerification {
  rewardReviewRequired?: boolean
  email: string
  /** The last send attempt failed; the user should try "resend". */
  deliveryFailed: boolean
  /** A mail was just sent, so the resend button starts on cooldown. */
  mailInFlight: boolean
}

/** Outcome of following a /pro?verify=<token> link. */
export type VerificationOutcome =
  | { status: 'verified' }
  | { status: 'invalid' }

export interface UnifiedAuthState {
  user: User | null
  /** Cheap balance snapshot; refreshed from WebSocket BalanceUpdated messages. */
  balance: AccountBalance | null
  /** Full account (plan, grants, membership, hourly price); loaded with the session. */
  account: AccountSummary | null
  paymentsEnabled: boolean
  checking: boolean
  submitting: boolean
  anonymousAllowed: boolean
  ragEnabled: boolean
  registrationEnabled: boolean
  emailVerificationRequired: boolean
  allowUserApiKey: boolean
  /** Whether joining the Speechmatics training programme is offered here, and its discount. */
  trainingProgram: TrainingProgramInfo
  error: string | null
  /** Set after a sign-up (or a login attempt) that still needs email verification. */
  pendingVerification: PendingVerification | null
  /** Result of a verification link opened in this tab, until dismissed. */
  verificationOutcome: VerificationOutcome | null
  login: (email: string, password: string) => Promise<boolean>
  register: (input: RegisterInput) => Promise<boolean>
  resendVerification: (email: string) => Promise<boolean>
  /** Leave the "check your inbox" screen and go back to the login form. */
  dismissVerification: () => void
  logout: () => Promise<void>
  clearError: () => void
  /** Re-reads `/api/user/balance` only. */
  refreshBalance: () => Promise<void>
  /** Re-reads `/api/user/billing/account` (also refreshes the balance). */
  refreshAccount: () => Promise<void>
  /** Applies a balance pushed over WebSocket; `null` falls back to a refresh. */
  applyBalance: (balance: AccountBalance | null) => void
  /** Records the training-programme answer; resolves false when it could not be saved. */
  setTrainingOptIn: (optIn: boolean) => Promise<boolean>
}

export function useUnifiedAuth(): UnifiedAuthState {
  const [user, setUser] = useState<User | null>(null)
  const [balance, setBalance] = useState<AccountBalance | null>(null)
  const [account, setAccount] = useState<AccountSummary | null>(null)
  const [paymentsEnabled, setPaymentsEnabled] = useState(false)
  const [checking, setChecking] = useState(true)
  const [submitting, setSubmitting] = useState(false)
  const [anonymousAllowed, setAnonymousAllowed] = useState(false)
  const [ragEnabled, setRagEnabled] = useState(false)
  const [registrationEnabled, setRegistrationEnabled] = useState(false)
  const [emailVerificationRequired, setEmailVerificationRequired] = useState(false)
  const [allowUserApiKey, setAllowUserApiKey] = useState(false)
  const [trainingProgram, setTrainingProgram] = useState<TrainingProgramInfo>(TRAINING_PROGRAM_UNAVAILABLE)
  const [error, setError] = useState<string | null>(null)
  const [pendingVerification, setPendingVerification] = useState<PendingVerification | null>(null)
  const [verificationOutcome, setVerificationOutcome] = useState<VerificationOutcome | null>(null)
  const balanceRequestRef = useRef(0)

  const clearBilling = useCallback(() => {
    setBalance(null)
    setAccount(null)
  }, [])

  const refreshBalance = useCallback(async () => {
    const request = ++balanceRequestRef.current
    const ownerId = getStoredUser()?.id ?? null
    if (!ownerId) {
      if (request === balanceRequestRef.current) clearBilling()
      return
    }
    try {
      const nextBalance = await getUserBalance()
      if (
        request !== balanceRequestRef.current
        || getStoredUser()?.id !== ownerId
      ) {
        return
      }
      if (nextBalance.user_id !== ownerId) {
        clearBilling()
        return
      }
      setBalance(nextBalance)
    } catch {
      if (
        request === balanceRequestRef.current
        && getStoredUser()?.id === ownerId
      ) {
        setBalance(null)
      }
    }
  }, [clearBilling])

  const refreshAccount = useCallback(async () => {
    const request = ++balanceRequestRef.current
    const ownerId = getStoredUser()?.id ?? null
    if (!ownerId) {
      if (request === balanceRequestRef.current) clearBilling()
      return
    }
    try {
      const next = await getUserBillingAccount()
      if (
        request !== balanceRequestRef.current
        || getStoredUser()?.id !== ownerId
      ) {
        return
      }
      if (next.account.user_id !== ownerId) {
        clearBilling()
        return
      }
      setAccount(next.account)
      setBalance(next.account)
      setPaymentsEnabled(next.payments_enabled === true)
    } catch {
      if (
        request === balanceRequestRef.current
        && getStoredUser()?.id === ownerId
      ) {
        clearBilling()
      }
    }
  }, [clearBilling])

  const applyBalance = useCallback((next: AccountBalance | null) => {
    const ownerId = getStoredUser()?.id ?? null
    if (!next || !ownerId || next.user_id !== ownerId) {
      void refreshBalance()
      return
    }
    // Do not bump balanceRequestRef: an account load in flight during the
    // first push must still land so the panel has plan and grant details.
    setBalance(next)
  }, [refreshBalance])

  const initialize = useCallback(async () => {
    setChecking(true)
    try {
      const verifyToken = consumeVerifyToken()
      const [initialUser, access, systemSettings] = await Promise.all([
        initAuth(),
        getSystemAccess(),
        getSystemSettings().catch(() => ({ allow_user_api_key: false })),
      ])
      let authenticatedUser = initialUser
      if (verifyToken) {
        // A verification link signs the (possibly different) account in,
        // replacing whatever session this browser held.
        try {
          const session = await verifyEmailRequest(verifyToken)
          authenticatedUser = session.user
          setVerificationOutcome({ status: 'verified' })
        } catch (reason) {
          if (reason instanceof AuthRequestError && reason.code === 'verification_token_invalid') {
            setVerificationOutcome({ status: 'invalid' })
          } else {
            setError(reason instanceof Error ? reason.message : messages().auth.errors.verifyFailed)
          }
        }
      }
      setUser(authenticatedUser)
      setRagEnabled(access.ragEnabled)
      setRegistrationEnabled(access.registrationEnabled)
      setEmailVerificationRequired(access.emailVerificationRequired)
      setTrainingProgram(access.trainingProgram)
      setAllowUserApiKey(systemSettings.allow_user_api_key === true)
      if (authenticatedUser) {
        setAnonymousAllowed(false)
        await refreshAccount()
      } else {
        clearBilling()
        setAnonymousAllowed(access.anonymousAPIEnabled)
      }
    } finally {
      setChecking(false)
    }
  }, [clearBilling, refreshAccount])

  useEffect(() => {
    void initialize()
    const refreshAnonymousAccess = () => {
      void getSystemAccess().then((access) => {
        setAnonymousAllowed(access.anonymousAPIEnabled)
        setRagEnabled(access.ragEnabled)
        setRegistrationEnabled(access.registrationEnabled)
        setEmailVerificationRequired(access.emailVerificationRequired)
        setTrainingProgram(access.trainingProgram)
      })
      void getSystemSettings()
        .then((settings) => setAllowUserApiKey(settings.allow_user_api_key === true))
        .catch(() => setAllowUserApiKey(false))
    }
    const handleAuthChanged = () => {
      // Storage/Broadcast events run before the next render. Adopting the
      // stored identity synchronously prevents the workspace from retaining
      // account A as owner while authenticated requests already use B's token.
      const nextUser = getStoredUser()
      balanceRequestRef.current += 1
      setUser(nextUser)
      clearBilling()
      if (nextUser) {
        setAnonymousAllowed(false)
        void refreshAccount()
      } else {
        refreshAnonymousAccess()
      }
    }
    window.addEventListener(AUTH_STATE_CHANGED_EVENT, handleAuthChanged)
    return () => {
      window.removeEventListener(AUTH_STATE_CHANGED_EVENT, handleAuthChanged)
    }
  }, [clearBilling, initialize, refreshAccount])

  const login = useCallback(async (email: string, password: string) => {
    setSubmitting(true)
    setError(null)
    try {
      const response = await loginRequest(email.trim(), password)
      setUser(response.user)
      setAnonymousAllowed(false)
      void refreshAccount()
      return true
    } catch (reason) {
      if (reason instanceof AuthRequestError && reason.code === 'email_not_verified') {
        setPendingVerification({ email: email.trim().toLowerCase(), deliveryFailed: false, mailInFlight: false })
        return false
      }
      setError(reason instanceof Error ? reason.message : messages().auth.errors.loginFailed)
      return false
    } finally {
      setSubmitting(false)
    }
  }, [refreshAccount])

  const register = useCallback(async (input: RegisterInput) => {
    if (!registrationEnabled) {
      setError(messages().auth.errors.registrationClosed)
      return false
    }
    setSubmitting(true)
    setError(null)
    try {
      const result = await registerRequest(
        input.email.trim(),
        input.password,
        input.name.trim(),
        input.inviteCode?.trim() || undefined,
        input.referralCode?.trim() || undefined,
      )
      if (result.kind === 'verification-pending') {
        setPendingVerification({
          email: result.pending.email,
          deliveryFailed: !result.pending.email_sent,
          mailInFlight: result.pending.email_sent,
          rewardReviewRequired: result.pending.reward_review_required,
        })
        return false
      }
      setUser(result.session.user)
      setAnonymousAllowed(false)
      void refreshAccount()
      return true
    } catch (reason) {
      const errors = messages().auth.errors
      const message = reason instanceof Error ? reason.message : errors.registerFailed
      if (/self-registration is disabled/i.test(message)) {
        setError(errors.registrationClosed)
      } else if (/invalid registration invite code/i.test(message)) {
        setError(errors.inviteInvalid)
      } else if (/disposable email/i.test(message)) {
        setError(errors.disposableEmail)
      } else if (/email domain is not allowed/i.test(message)) {
        setError(errors.domainNotAllowed)
      } else if (/email already registered/i.test(message)) {
        setError(errors.alreadyRegistered)
      } else if (/email delivery is not configured/i.test(message)) {
        setError(errors.mailNotConfigured)
      } else if (/rate limit exceeded/i.test(message)) {
        setError(errors.tooFrequent)
      } else {
        setError(message)
      }
      return false
    } finally {
      setSubmitting(false)
    }
  }, [refreshAccount, registrationEnabled])

  const resendVerification = useCallback(async (email: string) => {
    setSubmitting(true)
    setError(null)
    try {
      await resendVerificationRequest(email.trim().toLowerCase())
      setPendingVerification({ email: email.trim().toLowerCase(), deliveryFailed: false, mailInFlight: true })
      return true
    } catch (reason) {
      const errors = messages().auth.errors
      const message = reason instanceof Error ? reason.message : errors.sendFailed
      if (/rate limit exceeded/i.test(message)) {
        setError(errors.resendTooFrequent)
      } else if (/failed to send verification email/i.test(message)) {
        setError(errors.resendFailed)
      } else {
        setError(message)
      }
      return false
    } finally {
      setSubmitting(false)
    }
  }, [])

  const dismissVerification = useCallback(() => {
    setPendingVerification(null)
    setVerificationOutcome(null)
    setError(null)
  }, [])

  const logout = useCallback(async () => {
    setSubmitting(true)
    try {
      await logoutRequest()
      // Server-side revocation is best effort and may take several seconds.
      // Another tab can log in during that window, so converge on the identity
      // that is stored now instead of letting the stale logout completion
      // overwrite the newer account with an anonymous React state.
      const currentUser = getStoredUser()
      balanceRequestRef.current += 1
      setUser(currentUser)
      clearBilling()
      if (currentUser) {
        setAnonymousAllowed(false)
        void refreshAccount()
        return
      }
      const access = await getSystemAccess()
      setAnonymousAllowed(access.anonymousAPIEnabled)
      setRagEnabled(access.ragEnabled)
      setRegistrationEnabled(access.registrationEnabled)
      setAllowUserApiKey(
        await getSystemSettings()
          .then((settings) => settings.allow_user_api_key === true)
          .catch(() => false),
      )
    } finally {
      setSubmitting(false)
    }
  }, [clearBilling, refreshAccount])

  const setTrainingOptIn = useCallback(async (optIn: boolean) => {
    const ownerId = getStoredUser()?.id ?? null
    if (!ownerId) return false
    try {
      const updated = await updateTrainingOptInRequest(optIn)
      if (getStoredUser()?.id !== updated.id) return false
      // The stored copy feeds every tab; the hourly rate changes with the answer.
      setStoredUser(updated)
      setUser(updated)
      void refreshAccount()
      return true
    } catch {
      return false
    }
  }, [refreshAccount])

  return {
    user,
    balance,
    account,
    paymentsEnabled,
    checking,
    submitting,
    anonymousAllowed,
    ragEnabled,
    registrationEnabled,
    emailVerificationRequired,
    allowUserApiKey,
    trainingProgram,
    error,
    pendingVerification,
    verificationOutcome,
    login,
    register,
    resendVerification,
    dismissVerification,
    logout,
    clearError: () => setError(null),
    refreshBalance,
    refreshAccount,
    applyBalance,
    setTrainingOptIn,
  }
}

/** Reads and strips `?verify=<token>` left by the emailed verification link. */
function consumeVerifyToken(): string | null {
  if (typeof window === 'undefined') return null
  const url = new URL(window.location.href)
  const token = url.searchParams.get('verify')?.trim()
  if (!token) return null
  url.searchParams.delete('verify')
  window.history.replaceState(window.history.state, '', `${url.pathname}${url.search}${url.hash}`)
  return token
}
