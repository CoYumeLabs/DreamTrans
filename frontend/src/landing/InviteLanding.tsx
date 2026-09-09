import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import QRCode from 'qrcode'
import { useMessages } from '../i18n'
import { LocaleSwitch } from '../i18n/LocaleSwitch'
import { Icon } from '../unified/components/Icon'
import { inviteApiBase, offerRewardLines, type Offer } from '../unified/inviteOffer'
import { canonicalInviteLink, parseInviteTarget, signupPath } from './inviteLinks'
import './LandingPage.css'
import './InviteLanding.css'

function splitCountdown(ms: number) {
  const total = Math.max(0, Math.floor(ms / 1000))
  return {
    days: Math.floor(total / 86_400),
    hours: Math.floor((total % 86_400) / 3600),
    minutes: Math.floor((total % 3600) / 60),
    seconds: total % 60,
  }
}

function useCountdown(expiresAt?: string) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!expiresAt) return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [expiresAt])
  if (!expiresAt) return null
  const end = Date.parse(expiresAt)
  if (Number.isNaN(end)) return null
  return { ended: end <= now, ...splitCountdown(end - now) }
}

type Status =
  | { state: 'loading' }
  | { state: 'ready'; offer: Offer }
  | { state: 'invalid' }

export default function InviteLanding() {
  const m = useMessages()
  const t = m.invite
  const [target] = useState(() => parseInviteTarget(window.location.pathname, window.location.search))
  const [status, setStatus] = useState<Status>({ state: 'loading' })
  const [copied, setCopied] = useState(false)
  const qrRef = useRef<HTMLCanvasElement | null>(null)
  const shareLink = useMemo(() => canonicalInviteLink(window.location.origin, target), [target])
  const isReferral = !target.code && Boolean(target.ref)

  useEffect(() => {
    if (!target.code && !target.ref) {
      setStatus({ state: 'invalid' })
      return
    }
    const controller = new AbortController()
    const query: Record<string, string> = target.code ? { code: target.code } : { ref: target.ref }
    void fetch(`${inviteApiBase()}/api/auth/invite?${new URLSearchParams(query)}`, { signal: controller.signal })
      .then(async (response) => {
        if (!response.ok) throw new Error(String(response.status))
        const offer = await response.json() as Offer
        setStatus({ state: 'ready', offer })
      })
      .catch(() => { if (!controller.signal.aborted) setStatus({ state: 'invalid' }) })
    // One visit per browser session per link; the server also collapses repeats per day.
    const visitKey = `dt-invite-visit:${target.code || target.ref}`
    const seen = (() => { try { return sessionStorage.getItem(visitKey) === '1' } catch { return false } })()
    if (!seen) {
      try { sessionStorage.setItem(visitKey, '1') } catch { /* private mode */ }
      void fetch(`${inviteApiBase()}/api/auth/invite/visit`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: target.code, ref: target.ref, ...target.utm }),
        keepalive: true,
      }).catch(() => undefined)
    }
    return () => controller.abort()
  }, [target])

  useEffect(() => {
    const canvas = qrRef.current
    if (!canvas || status.state !== 'ready') return
    void QRCode.toCanvas(canvas, shareLink, { width: 168, margin: 1, color: { dark: '#111318', light: '#ffffff' } }).catch(() => undefined)
  }, [shareLink, status.state])

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(shareLink)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch { /* clipboard blocked; the link is visible in the input */ }
  }, [shareLink])

  const offer = status.state === 'ready' ? status.offer : null
  const countdown = useCountdown(offer?.expires_at)
  const rewards = useMemo(() => (offer && !isReferral ? offerRewardLines(offer, m) : []), [offer, isReferral, m])
  const closed = Boolean(countdown?.ended) || (offer?.remaining !== undefined && offer.remaining <= 0)
  const title = isReferral
    ? (offer?.referrer_name ? t.referralTitle(offer.referrer_name) : t.referralAnonymousTitle)
    : (offer?.headline || offer?.name || '')

  const savePoster = useCallback(async () => {
    if (!offer) return
    const canvas = document.createElement('canvas')
    const w = 1080
    const h = 1440
    canvas.width = w
    canvas.height = h
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    ctx.fillStyle = '#111318'
    ctx.fillRect(0, 0, w, h)
    ctx.fillStyle = '#ffffff'
    ctx.beginPath()
    ctx.roundRect(60, 60, w - 120, h - 120, 40)
    ctx.fill()
    const wrap = (text: string, x: number, y: number, maxWidth: number, lineHeight: number, maxLines: number) => {
      const chars = Array.from(text)
      let line = ''
      let lines = 0
      for (const ch of chars) {
        const next = line + ch
        if (ctx.measureText(next).width > maxWidth && line) {
          ctx.fillText(line, x, y)
          y += lineHeight
          line = ch
          if (++lines >= maxLines - 1) break
        } else {
          line = next
        }
      }
      if (line) { ctx.fillText(line, x, y); y += lineHeight }
      return y
    }
    ctx.fillStyle = '#2b2fd8'
    ctx.font = '600 34px system-ui, -apple-system, "PingFang SC", "Noto Sans CJK SC", sans-serif'
    ctx.fillText((isReferral ? t.referralEyebrow : t.eyebrow).toUpperCase(), 120, 170)
    ctx.fillStyle = '#111318'
    ctx.font = '700 64px system-ui, -apple-system, "PingFang SC", "Noto Sans CJK SC", sans-serif'
    let y = wrap(title, 120, 260, w - 240, 84, 3)
    ctx.fillStyle = '#4b5160'
    ctx.font = '400 34px system-ui, -apple-system, "PingFang SC", "Noto Sans CJK SC", sans-serif'
    y = wrap(isReferral ? t.referralLead : (offer.description || t.defaultLead), 120, y + 20, w - 240, 50, 4)
    const bullets = rewards.length ? rewards : [isReferral ? t.benefitTrialPlain : t.benefitTrial]
    ctx.font = '500 34px system-ui, -apple-system, "PingFang SC", "Noto Sans CJK SC", sans-serif'
    y += 30
    for (const line of bullets.slice(0, 6)) {
      ctx.fillStyle = '#2b2fd8'
      ctx.beginPath()
      ctx.arc(136, y - 12, 9, 0, Math.PI * 2)
      ctx.fill()
      ctx.fillStyle = '#111318'
      y = wrap(line, 170, y, w - 290, 48, 2) + 12
    }
    try {
      const qr = await QRCode.toDataURL(shareLink, { width: 360, margin: 1 })
      const image = new Image()
      await new Promise<void>((resolve, reject) => { image.onload = () => resolve(); image.onerror = () => reject(new Error('qr')); image.src = qr })
      ctx.drawImage(image, w - 120 - 360, h - 120 - 360 - 40, 360, 360)
    } catch { /* poster still useful without the code */ }
    ctx.fillStyle = '#111318'
    ctx.font = '700 44px system-ui, -apple-system, "PingFang SC", "Noto Sans CJK SC", sans-serif'
    ctx.fillText(m.common.brand, 120, h - 220)
    ctx.fillStyle = '#4b5160'
    ctx.font = '400 30px system-ui, -apple-system, "PingFang SC", "Noto Sans CJK SC", sans-serif'
    wrap(t.qrHint, 120, h - 160, 480, 40, 2)
    ctx.font = '500 28px ui-monospace, SFMono-Regular, Menlo, monospace'
    ctx.fillText(shareLink.replace(/^https?:\/\//, ''), 120, h - 300)
    const link = document.createElement('a')
    link.href = canvas.toDataURL('image/png')
    link.download = `${t.posterFile}-${target.code || target.ref}.png`
    link.click()
  }, [isReferral, m.common.brand, offer, rewards, shareLink, t, target.code, target.ref, title])

  const nativeShare = typeof navigator !== 'undefined' && typeof navigator.share === 'function'

  return (
    <div className="lp iv">
      <header className="lp-nav">
        <a className="lp-brand" href="/" title={m.landing.tagline}>
          <img alt={m.common.brand} className="lp-brand__logo" decoding="async" draggable={false} height={44} src="/brand/yufolo-logo.png" width={157} />
        </a>
        <div className="lp-nav__actions">
          <LocaleSwitch className="lp-locale" />
          <a className="lp-btn lp-btn--ghost" href="/pro">{m.landing.nav.login}</a>
        </div>
      </header>

      <main className="iv-main">
        {status.state === 'loading' && <p className="iv-status" role="status">{t.loading}</p>}

        {status.state === 'invalid' && (
          <section className="iv-card iv-card--invalid" aria-live="polite">
            <p className="lp-eyebrow">{t.eyebrow}</p>
            <h1>{t.invalidTitle}</h1>
            <p className="iv-lead">{t.invalidLead}</p>
            <div className="lp-hero__cta">
              <a className="lp-btn lp-btn--primary lp-btn--lg" href="/pro">{t.ctaFallback}</a>
              <a className="lp-btn lp-btn--secondary lp-btn--lg" href="/">{m.common.brand}</a>
            </div>
          </section>
        )}

        {offer && (
          <div className="iv-grid">
            <section className="iv-card iv-card--offer" aria-labelledby="iv-title">
              <p className="lp-eyebrow">{isReferral ? t.referralEyebrow : t.eyebrow}</p>
              <h1 id="iv-title">{title}</h1>
              {!isReferral && offer.headline && offer.name && offer.headline !== offer.name && <p className="iv-campaign">{offer.name}</p>}
              <p className="iv-lead">{isReferral ? t.referralLead : (offer.description || t.defaultLead)}</p>

              {!closed && <>
                <h2 className="iv-subhead">{t.benefits}</h2>
                <ul className="iv-benefits">
                  {rewards.map((line) => <li key={line}><Icon name="check" size={15} />{line}</li>)}
                  <li><Icon name="check" size={15} />{isReferral ? t.benefitTrialPlain : t.benefitTrial}</li>
                </ul>
              </>}

              {!isReferral && (countdown || offer.remaining !== undefined) && (
                <div className="iv-urgency" aria-live="polite">
                  {countdown && !countdown.ended && (
                    <div className="iv-countdown" aria-label={t.endsIn}>
                      <span className="iv-countdown__label">{t.endsIn}</span>
                      <span className="iv-countdown__cells">
                        <b>{countdown.days}</b><small>{t.days}</small>
                        <b>{String(countdown.hours).padStart(2, '0')}</b><small>{t.hours}</small>
                        <b>{String(countdown.minutes).padStart(2, '0')}</b><small>{t.minutes}</small>
                        <b>{String(countdown.seconds).padStart(2, '0')}</b><small>{t.seconds}</small>
                      </span>
                    </div>
                  )}
                  {countdown?.ended && <span className="iv-pill iv-pill--closed">{t.ended}</span>}
                  {offer.remaining !== undefined && (
                    <span className={`iv-pill ${offer.remaining <= 0 ? 'iv-pill--closed' : ''}`}>
                      {offer.remaining <= 0 ? t.full : t.remaining(offer.remaining)}
                    </span>
                  )}
                </div>
              )}

              <ol className="iv-steps">
                {t.steps.map((step, index) => <li key={step}><span>{index + 1}</span>{step}</li>)}
              </ol>

              <div className="lp-hero__cta iv-cta">
                <a className="lp-btn lp-btn--primary lp-btn--lg" href={closed ? '/pro' : signupPath(target)}>
                  {closed ? t.ctaFallback : isReferral ? t.ctaReferral : t.cta}
                </a>
                <a className="lp-btn lp-btn--secondary lp-btn--lg" href="/pro">{t.login}</a>
              </div>
              <p className="iv-note">{isReferral ? m.auth.referralNote : t.rewardsNote} {t.legalNote}</p>
            </section>

            <aside className="iv-card iv-card--share" aria-label={isReferral ? t.shareReferral : t.share}>
              <h2 className="iv-subhead">{isReferral ? t.shareReferral : t.share}</h2>
              <canvas aria-label={t.qrHint} className="iv-qr" height={168} ref={qrRef} width={168} />
              <p className="iv-qr-hint">{t.qrHint}</p>
              <input aria-label={isReferral ? t.shareReferral : t.share} className="iv-link" onFocus={(event) => event.target.select()} readOnly value={shareLink} />
              <div className="iv-share-actions">
                <button className="lp-btn lp-btn--primary" type="button" onClick={() => { void copy() }}>{copied ? t.copied : t.copy}</button>
                {nativeShare && (
                  <button className="lp-btn lp-btn--secondary" type="button" onClick={() => { void navigator.share({ title, url: shareLink }).catch(() => undefined) }}>{t.native}</button>
                )}
                <button className="lp-btn lp-btn--secondary" type="button" onClick={() => { void savePoster() }}>{t.poster}</button>
              </div>
            </aside>
          </div>
        )}
      </main>

      <footer className="iv-footer">
        <span>{m.landing.footer.copyright(new Date().getFullYear())}</span>
        <a href="/privacy">{m.legal.privacy}</a>
        <a href="/terms">{m.legal.terms}</a>
      </footer>
    </div>
  )
}
