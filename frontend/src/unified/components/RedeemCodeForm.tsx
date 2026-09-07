import { useState } from 'react'
import { authFetch } from '../../pro/api/auth'
import { useMessages } from '../../i18n'

export function RedeemCodeForm({ onRefresh }: { onRefresh: () => Promise<void> }) {
  const m = useMessages().redeem
  const [code, setCode] = useState(''), [busy, setBusy] = useState(false), [message, setMessage] = useState('')
  async function redeem() {
    if (busy || !code.trim()) return
    setBusy(true); setMessage('')
    try {
      await authFetch('/api/user/redeem', { method: 'POST', body: JSON.stringify({ code: code.trim() }) })
      setMessage(m.success); setCode(''); await onRefresh()
    } catch (reason) { setMessage(reason instanceof Error ? reason.message : m.failed) }
    finally { setBusy(false) }
  }
  return <section className="dt-billing-card"><h3>{m.title}</h3><p className="dt-muted">{m.description}</p><form onSubmit={event => { event.preventDefault(); void redeem() }}><label>{m.code}<input aria-label={m.code} autoComplete="off" maxLength={48} value={code} onChange={event => setCode(event.target.value)} disabled={busy} required /></label><button className="dt-primary-button" disabled={busy || !code.trim()} type="submit">{busy ? m.busy : m.submit}</button></form>{message && <p role="status">{message}</p>}</section>
}
