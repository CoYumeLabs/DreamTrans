import { useEffect, useState } from 'react'
import { useMessages } from '../../i18n'
import { inviteApiBase, offerRewardLines, type Offer } from '../inviteOffer'

export function InviteOffer({ code, referralCode = '' }: { code: string; referralCode?: string }) {
  const m = useMessages()
  const normalized = code.trim()
  const ref = normalized ? '' : referralCode.trim()
  const key = normalized ? `code:${normalized}` : ref ? `ref:${ref}` : ''
  const [attempt, setAttempt] = useState(0)
  const [result, setResult] = useState<{ key: string; offer?: Offer; error?: string } | null>(null)
  useEffect(() => {
    if (!key) return
    const controller = new AbortController()
    let active = true
    const timeout = window.setTimeout(() => controller.abort(), 12_000)
    const timer = window.setTimeout(() => {
      const query: Record<string, string> = normalized ? { code: normalized } : { ref }
      void fetch(`${inviteApiBase()}/api/auth/invite?${new URLSearchParams(query)}`, { signal: controller.signal })
        .then(async (response) => {
          const data = await response.json() as Offer & { error?: string }
          if (!response.ok) throw new Error(data.error || m.auth.inviteFailure)
          if (active) setResult({ key, offer: data })
        })
        .catch((error: unknown) => {
          if (active) setResult({ key, error: error instanceof Error && error.name !== 'AbortError' ? error.message : m.auth.inviteFailure })
        })
        .finally(() => window.clearTimeout(timeout))
    }, 350)
    return () => { active = false; window.clearTimeout(timer); window.clearTimeout(timeout); controller.abort() }
  }, [key, normalized, ref, attempt, m.auth.inviteFailure])
  if (!key) return null
  if (!result || result.key !== key) return <p className="dt-auth__hint" role="status">{m.auth.inviteLoading}</p>
  if (result.error) {
    // A stale referral link is not worth blocking sign-up over.
    if (ref) return null
    return <div className="dt-auth__offer" role="status">{result.error} <button className="dt-button dt-button--text" type="button" onClick={() => { setResult(null); setAttempt((n) => n + 1) }}>{m.auth.inviteRetry}</button></div>
  }
  const offer = result.offer!
  if (ref) {
    return <div className="dt-auth__offer" role="status">
      <strong>{offer.referrer_name ? m.auth.referralFrom(offer.referrer_name) : m.auth.referralGeneric}</strong>
      <p className="dt-muted">{m.auth.referralNote}</p>
    </div>
  }
  const lines = offerRewardLines(offer, m)
  return <div className="dt-auth__offer" role="status">
    <strong>{offer.headline || offer.name}</strong>
    {offer.headline && offer.name && offer.headline !== offer.name && <p className="dt-muted">{offer.name}</p>}
    {lines.map((line) => <p key={line}>{line}</p>)}
    {lines.length > 0 && <p className="dt-muted">{m.auth.inviteRewards}</p>}
  </div>
}
