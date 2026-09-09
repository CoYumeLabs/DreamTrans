import { useState } from 'react'
import { authFetch } from '../../pro/api/auth'
import { useMessages } from '../../i18n'

export function RedeemCodeForm({ onRefresh }: { onRefresh: () => Promise<void> }) {
  const m = useMessages().redeem
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState('')

  async function redeem() {
    if (busy || !code.trim()) return
    setBusy(true)
    setMessage('')
    try {
      const result = await authFetch<{ pending?: boolean }>('/api/user/redeem', { method: 'POST', body: JSON.stringify({ code: code.trim() }) })
      setMessage(result.pending ? m.pending : m.success)
      setCode('')
      await onRefresh()
    } catch (reason) {
      setMessage(reason instanceof Error ? reason.message : m.failed)
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="dt-billing-card" aria-label={m.title}>
      <div className="dt-billing-card__head">
        <div>
          <strong>{m.title}</strong>
          <small>{m.description}</small>
        </div>
      </div>
      <form className="dt-redeem" onSubmit={(event) => { event.preventDefault(); void redeem() }}>
        <label className="dt-field">
          <span>{m.code}</span>
          <input
            autoComplete="off"
            disabled={busy}
            maxLength={48}
            onChange={(event) => setCode(event.target.value)}
            required
            spellCheck={false}
            value={code}
          />
        </label>
        <button
          className="dt-button dt-button--primary"
          disabled={busy || !code.trim()}
          type="submit"
        >
          {busy ? m.busy : m.submit}
        </button>
      </form>
      {message && <p className="dt-muted" role="status">{message}</p>}
    </section>
  )
}
