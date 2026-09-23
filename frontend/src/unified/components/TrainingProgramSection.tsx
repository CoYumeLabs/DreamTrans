import { useState } from 'react'
import type { RouteDecision, TrainingProgramInfo } from '../../api'
import { useMessages } from '../../i18n'
import { Toggle } from './Toggle'

interface TrainingProgramSectionProps {
  route?: RouteDecision
  program: TrainingProgramInfo
  /** The account's training-programme answer; null until asked. */
  optIn: boolean | null
  onOptInChange: (optIn: boolean) => Promise<boolean>
}

/**
 * The training-programme choice. It lives with the balance because it is a
 * billing and data-use decision for the account, not a recording preference.
 */
export function TrainingProgramSection({
  route,
  program,
  optIn,
  onOptInChange,
}: TrainingProgramSectionProps) {
  const t = useMessages().settings.training
  const [status, setStatus] = useState('')
  if (!program.available && route?.reason !== 'program_off' && route?.reason !== 'single_account') {
    return null
  }
  // An institution tenant or an administrator-pinned route decides the account
  // for this user, so joining could never earn the discount.
  const ineligible = route?.reason === 'institution'
    || ((route?.reason === 'tenant_pinned' || route?.reason === 'user_pinned') && !route.training)

  async function change(next: boolean) {
    setStatus(t.saving)
    const saved = await onOptInChange(next)
    setStatus(saved ? t.saved(next) : t.saveFailed)
  }

  const routeNote = route?.reason === 'program_off' ? t.routeOff
    : route?.reason === 'institution' ? t.routeInstitution
      : route?.reason === 'tenant_pinned' || route?.reason === 'user_pinned' ? t.routePinned
        : route?.gift_funded ? t.routeGift : ''

  return (
    <section className="dt-billing-card dt-training-card" aria-label={t.title}>
      <div className="dt-billing-card__head">
        <div>
          <strong>{t.title}</strong>
          <small>
            {route?.reason === 'single_account' ? t.routeSingleAccount : t.body(program.discountPercent)}
            {' '}
            <a href="/privacy#share" rel="noreferrer" target="_blank">{t.privacyLink}</a>
          </small>
        </div>
      </div>
      {routeNote && <p className="dt-muted" role="status">{routeNote}</p>}
      {program.available && (
        <Toggle
          checked={optIn === true && !ineligible}
          description={ineligible ? t.notEligible : optIn === null ? t.unanswered : t.toggleBody}
          disabled={ineligible}
          label={ineligible ? t.notEligible : t.toggle(program.discountPercent)}
          onChange={(next) => void change(next)}
        />
      )}
      {status && <p className="dt-muted" role="status">{status}</p>}
    </section>
  )
}
