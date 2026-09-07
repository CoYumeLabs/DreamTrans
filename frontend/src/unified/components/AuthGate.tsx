import { useEffect, useState, type FormEvent } from 'react'
import { useMessages } from '../../i18n'
import { LocaleSwitch } from '../../i18n/LocaleSwitch'
import type {
  PendingVerification,
  RegisterInput,
  VerificationOutcome,
} from '../hooks/useUnifiedAuth'
import { BrandMark } from './BrandMark'
import { Icon } from './Icon'
import { InviteOffer } from './InviteOffer'

interface AuthGateProps {
  allowAnonymous: boolean
  emailVerificationRequired: boolean
  error: string | null
  pendingVerification: PendingVerification | null
  proEntry: boolean
  registrationEnabled: boolean
  submitting: boolean
  verificationOutcome: VerificationOutcome | null
  onContinueAnonymous: () => void
  onDismissVerification: () => void
  onLogin: (email: string, password: string) => Promise<boolean>
  onRegister: (input: RegisterInput) => Promise<boolean>
  onResendVerification: (email: string) => Promise<boolean>
}

/** Seconds the resend button stays disabled after a send, matching the server cooldown. */
const RESEND_COOLDOWN_SECONDS = 60

export function AuthGate({
  allowAnonymous,
  emailVerificationRequired,
  error,
  pendingVerification,
  proEntry,
  registrationEnabled,
  submitting,
  verificationOutcome,
  onContinueAnonymous,
  onDismissVerification,
  onLogin,
  onRegister,
  onResendVerification,
}: AuthGateProps) {
  const m = useMessages()
  const [linkedInviteCode] = useState(() => new URLSearchParams(window.location.search).get('invite')?.trim() ?? '')
  // A friend's referral link (/pro?ref=CODE) opens sign-up too; the code only
  // records where the account came from.
  const [referralCode] = useState(() => new URLSearchParams(window.location.search).get('ref')?.trim() ?? '')
  const [registering, setRegistering] = useState(() => registrationEnabled && Boolean(linkedInviteCode || referralCode))
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [name, setName] = useState('')
  const [inviteCode, setInviteCode] = useState(linkedInviteCode)
  const [agreed, setAgreed] = useState(false)
  const [agreeError, setAgreeError] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (registering && !agreed) {
      setAgreeError(true)
      return
    }
    setAgreeError(false)
    if (registering) {
      await onRegister({ email, password, name, inviteCode, referralCode })
    } else {
      await onLogin(email, password)
    }
  }

  if (pendingVerification) {
    return (
      <VerificationPending
        error={error}
        pending={pendingVerification}
        submitting={submitting}
        onBack={() => {
          // "Back to login" should land on the login form even when the
          // user arrived here from the sign-up form.
          setRegistering(false)
          setPassword('')
          onDismissVerification()
        }}
        onResend={onResendVerification}
      />
    )
  }

  return (
    <main className="dt-auth">
      <section className="dt-auth__intro" aria-labelledby="auth-title">
        <BrandMark className="dt-auth__mark" size={56} />
        <p className="dt-eyebrow">{m.common.brand}</p>
        <h1 id="auth-title">{m.auth.heroTitle}</h1>
        <p>{m.auth.heroLead}</p>
        <div className="dt-auth__signals" aria-label={m.auth.signals.join(' · ')}>
          {m.auth.signals.map((signal) => (
            <span key={signal}><Icon name="check" size={15} /> {signal}</span>
          ))}
        </div>
        <LocaleSwitch className="dt-auth__locale" />
      </section>

      <form className="dt-auth__card" onSubmit={(event) => { void submit(event) }}>
        <div>
          <p className="dt-eyebrow">{proEntry ? m.auth.proWorkspace : m.auth.workspace}</p>
          <h2>{registering ? m.auth.createAccount : m.auth.welcomeBack}</h2>
          <p className="dt-muted">
            {registering ? m.auth.createLead : m.auth.loginLead}
          </p>
        </div>

        {verificationOutcome?.status === 'invalid' && !error && (
          <div className="dt-form-error" role="alert">{m.auth.invalidLink}</div>
        )}
        {error && <div className="dt-form-error" role="alert">{error}</div>}

        {registering && emailVerificationRequired && (
          <p className="dt-auth__hint">
            <Icon name="message" size={15} />
            {m.auth.verifyHint}
          </p>
        )}

        {registering && (
          <label className="dt-field">
            <span>{m.auth.name}</span>
            <input
              autoComplete="nickname"
              onChange={(event) => setName(event.target.value)}
              placeholder={m.auth.namePlaceholder}
              required
              value={name}
            />
          </label>
        )}

        <label className="dt-field">
          <span>{m.auth.email}</span>
          <input
            autoComplete="username"
            inputMode="email"
            onChange={(event) => setEmail(event.target.value)}
            placeholder="name@example.com"
            required
            type="email"
            value={email}
          />
        </label>

        <label className="dt-field">
          <span>{m.auth.password}</span>
          <input
            autoComplete={registering ? 'new-password' : 'current-password'}
            minLength={registering ? 10 : 6}
            onChange={(event) => setPassword(event.target.value)}
            placeholder={registering ? m.auth.passwordRegister : m.auth.passwordLogin}
            required
            type="password"
            value={password}
          />
        </label>

        {registering && (
          <details className="dt-auth__invite">
            <summary>{inviteCode ? m.auth.inviteEntered : m.auth.haveInviteCode}</summary>
            <label className="dt-field">
              <span>{m.auth.inviteCode} <small>{m.auth.optional}</small></span>
              <input
                autoComplete="off"
                onChange={(event) => setInviteCode(event.target.value)}
                placeholder={m.auth.invitePlaceholder}
                value={inviteCode}
              />
            </label>
          </details>
        )}

        {registering && <InviteOffer code={inviteCode} referralCode={referralCode} />}

        {registering ? (
          <label className="dt-auth__legal">
            <input
              aria-invalid={agreeError}
              checked={agreed}
              onChange={(event) => {
                setAgreed(event.target.checked)
                if (event.target.checked) setAgreeError(false)
              }}
              type="checkbox"
            />
            <span>
              {m.auth.agree.before}
              <a href="/terms" rel="noreferrer" target="_blank">{m.legal.terms}</a>
              {m.auth.agree.mid}
              <a href="/privacy" rel="noreferrer" target="_blank">{m.legal.privacy}</a>
              {m.auth.agree.after}
            </span>
          </label>
        ) : (
          <p className="dt-auth__legal-note">
            {m.auth.legalNote.before}
            <a href="/terms" rel="noreferrer" target="_blank">{m.legal.terms}</a>
            {m.auth.legalNote.mid}
            <a href="/privacy" rel="noreferrer" target="_blank">{m.legal.privacy}</a>
            {m.auth.legalNote.after}
          </p>
        )}
        {registering && agreeError && (
          <div className="dt-form-error" role="alert">{m.auth.agree.mustAgree}</div>
        )}

        <button className="dt-button dt-button--primary dt-button--wide" disabled={submitting}>
          {submitting ? m.auth.pleaseWait : registering ? m.auth.createAccount : m.common.login}
        </button>

        {registrationEnabled ? (
          <button
            className="dt-button dt-button--text"
            onClick={() => setRegistering((value) => !value)}
            type="button"
          >
            {registering ? m.auth.haveAccount : m.auth.noAccount}
          </button>
        ) : (
          <p className="dt-muted">{m.auth.registrationClosed}</p>
        )}

        {allowAnonymous && !proEntry && (
          <>
            <div className="dt-auth__divider"><span>{m.auth.or}</span></div>
            <button
              className="dt-button dt-button--secondary dt-button--wide"
              onClick={onContinueAnonymous}
              type="button"
            >
              {m.auth.useLocalMode}
            </button>
          </>
        )}
      </form>
    </main>
  )
}

interface VerificationPendingProps {
  error: string | null
  pending: PendingVerification
  submitting: boolean
  onBack: () => void
  onResend: (email: string) => Promise<boolean>
}

function VerificationPending({
  error,
  pending,
  submitting,
  onBack,
  onResend,
}: VerificationPendingProps) {
  const m = useMessages()
  const v = m.auth.verify
  // A fresh sign-up already has a mail in flight; a failed delivery or a
  // login bounce starts with the button enabled.
  const [cooldown, setCooldown] = useState(pending.mailInFlight ? RESEND_COOLDOWN_SECONDS : 0)
  const [sentAgain, setSentAgain] = useState(false)

  useEffect(() => {
    if (cooldown <= 0) return
    const timer = window.setTimeout(() => setCooldown((value) => value - 1), 1_000)
    return () => window.clearTimeout(timer)
  }, [cooldown])

  const resend = async () => {
    const ok = await onResend(pending.email)
    if (ok) {
      setSentAgain(true)
      setCooldown(RESEND_COOLDOWN_SECONDS)
    }
  }

  return (
    <main className="dt-auth">
      <section className="dt-auth__intro" aria-labelledby="verify-title">
        <BrandMark className="dt-auth__mark" size={56} />
        <p className="dt-eyebrow">{m.common.brand}</p>
        <h1 id="verify-title">{v.title}</h1>
        <p>{v.lead}</p>
        <LocaleSwitch className="dt-auth__locale" />
      </section>

      <div className="dt-auth__card dt-auth__card--verify" data-testid="verification-pending">
        <div className="dt-auth__verify-icon"><Icon name="message" size={26} /></div>
        <div>
          <p className="dt-eyebrow">{v.eyebrow}</p>
          <h2>{pending.deliveryFailed ? v.failedTitle : v.sentTitle}</h2>
          <p className="dt-muted">
            {splitEmail(
              pending.deliveryFailed ? v.failedBody(pending.email) : v.sentBody(pending.email),
              pending.email,
            )}
          </p>
        </div>

        {error && <div className="dt-form-error" role="alert">{error}</div>}
        {pending.rewardReviewRequired && <p className="dt-auth__offer" role="status">{m.auth.rewardReview}</p>}
        {sentAgain && !error && (
          <p className="dt-auth__hint" role="status">
            <Icon name="check" size={15} />
            {v.resentNotice}
          </p>
        )}

        <button
          className="dt-button dt-button--primary dt-button--wide"
          disabled={submitting || cooldown > 0}
          onClick={() => { void resend() }}
          type="button"
        >
          {submitting ? v.sending : cooldown > 0 ? v.resendIn(cooldown) : v.resend}
        </button>
        <button className="dt-button dt-button--text" onClick={onBack} type="button">
          {v.backToLogin}
        </button>
      </div>
    </main>
  )
}

/** Renders the email address in bold inside a translated sentence. */
function splitEmail(sentence: string, email: string) {
  const index = sentence.indexOf(email)
  if (index < 0) return sentence
  return (
    <>
      {sentence.slice(0, index)}
      <strong>{email}</strong>
      {sentence.slice(index + email.length)}
    </>
  )
}
