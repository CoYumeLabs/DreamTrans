import { useCallback, useEffect, useRef, useState } from 'react'
import { formatUsageUSD } from '../../api'
import { useMessages } from '../../i18n'
import { ApiRequestError, getStoredUser } from '../../pro/api/auth'
import { batchStatus, MAX_BATCH_BYTES, prepareBatchAudio, quoteBatch, saveBatchResult, submitBatch } from '../workspace/batchTranscription'
import { languageOptions } from '../workspace/languageOptions'
import { Sheet } from './Sheet'
import './BatchTranscribePanel.css'

type JobState = 'ready' | 'uploading' | 'running' | 'saving' | 'done' | 'failed' | 'uncertain' | 'interrupted'
interface Job { id: string; name: string; language: string; seconds: number; cost?: number; jobId?: string; state: JobState; error?: string; audio?: Blob }
interface Props { ownerId: string | null; allowed: boolean; open: boolean; sourceLanguage: string; onClose: () => void; onAccount: () => void; onHistory: () => void; onSaved: () => Promise<void> }

function storageKey(ownerId: string | null) { return `dt_batch_jobs_v1:${ownerId ?? 'guest'}` }
function restoreJobs(ownerId: string | null): Job[] {
  if (!ownerId) return []
  try {
    const stored: Job[] = JSON.parse(localStorage.getItem(storageKey(ownerId)) ?? '[]')
    return stored.filter(job => typeof job.id === 'string' && typeof job.name === 'string').slice(0, 30).map(job => ({ ...job, state: job.state === 'uploading' ? 'uncertain' : job.state === 'ready' ? 'interrupted' : job.state }))
  } catch { return [] }
}

export function BatchTranscribePanel({ ownerId, allowed, open, sourceLanguage, onClose, onAccount, onHistory, onSaved }: Props) {
  const b = useMessages().batch
  const [jobs, setJobs] = useState<Job[]>(() => restoreJobs(ownerId))
  const jobsRef = useRef(jobs)
  const [language, setLanguage] = useState(sourceLanguage)
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)
  const mounted = useRef(false)
  const checking = useRef(new Set<string>())
  const [error, setError] = useState('')
  const callbacks = useRef({ onSaved, b })
  useEffect(() => { callbacks.current = { onSaved, b } }, [onSaved, b])
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  const current = useCallback(() => mounted.current && !!ownerId && getStoredUser()?.id === ownerId, [ownerId])
  const commit = useCallback((next: Job[]) => {
    if (!current()) return
    // Persist synchronously before a paid upload so refresh cannot silently
    // turn an in-flight submission into a fresh, chargeable retry.
    jobsRef.current = next
    setJobs(next)
    localStorage.setItem(storageKey(ownerId), JSON.stringify(next.map(job => ({ ...job, audio: undefined }))))
  }, [current, ownerId])
  const patch = useCallback((id: string, changes: Partial<Job>) => {
    commit(jobsRef.current.map(job => job.id === id ? { ...job, ...changes } : job))
  }, [commit])

  useEffect(() => {
    const check = async (job: Job) => {
      if (!job.jobId || checking.current.has(job.id) || !current()) return
      checking.current.add(job.id)
      try {
        const result = await batchStatus(job.jobId)
        if (!current()) return
        if (['rejected', 'deleted', 'error'].includes(result.status)) {
          patch(job.id, { state: 'failed', error: callbacks.current.b.failed })
        } else if (result.status === 'done') {
          if (!result.transcript) throw new Error('Missing transcript')
          patch(job.id, { state: 'saving', error: undefined })
          await saveBatchResult(ownerId!, job.id, job.name, job.language, result.transcript)
          if (!current()) return
          patch(job.id, { state: 'done', error: undefined })
          await callbacks.current.onSaved().catch(() => {})
        }
      } catch (reason) {
        if (current()) {
          const message = reason instanceof ApiRequestError && reason.status === 402 ? callbacks.current.b.insufficient : callbacks.current.b.error
          try { patch(job.id, { error: message }) } catch { setError(callbacks.current.b.storage) }
        }
      } finally { checking.current.delete(job.id) }
    }
    const tick = () => {
      for (const job of jobsRef.current) {
        if (['running', 'saving'].includes(job.state) && !job.error) void check(job)
      }
    }
    tick()
    const timer = window.setInterval(tick, 3000)
    return () => window.clearInterval(timer)
  }, [current, ownerId, patch])

  async function choose(files: File[]) {
    if (busyRef.current || !allowed) return
    const existing = jobsRef.current.filter(job => job.audio)
    if (files.length + existing.length > 10 || files.reduce((sum, file) => sum + file.size, 0) > MAX_BATCH_BYTES || jobsRef.current.length + files.length > 30) { setError(b.tooMany); return }
    busyRef.current = true; setBusy(true); setError('')
    try {
      for (const file of files) {
        const prepared = await prepareBatchAudio(file)
        if (!current()) return
        const bytes = jobsRef.current.reduce((sum, job) => sum + (job.audio?.size ?? 0), 0)
        if (bytes + prepared.audio.size > MAX_BATCH_BYTES) throw new Error('size')
        const job: Job = { id: crypto.randomUUID(), name: file.name, language, seconds: prepared.seconds, audio: prepared.audio, state: 'ready' }
        try { job.cost = (await quoteBatch(job.seconds)).reservation_usd } catch { job.error = b.unavailable }
        commit([...jobsRef.current, job])
      }
    } catch (reason) { if (current()) setError(reason instanceof DOMException && reason.name === 'QuotaExceededError' ? b.storage : b.invalid) }
    finally { busyRef.current = false; if (mounted.current) setBusy(false) }
  }

  async function start() {
    if (busyRef.current || !allowed) return
    busyRef.current = true; setBusy(true); setError('')
    try {
      for (const job of jobsRef.current) {
        if (!current()) return
        if (job.state !== 'ready' || !job.audio) continue
        const quote = await quoteBatch(job.seconds)
        if (!current()) return
        patch(job.id, { cost: quote.reservation_usd, error: undefined })
        if (!quote.affordable) { setError(b.insufficient); break }
        // Re-quote changes must be reviewed before spending at a higher rate.
        if (job.cost === undefined || quote.reservation_usd > job.cost + 0.000001) { setError(b.priceChanged); break }
        patch(job.id, { state: 'uploading' })
        try {
          const result = await submitBatch(job.audio, job.language)
          if (!current()) return
          if (!result.job_id) throw new Error('Missing job id')
          patch(job.id, { jobId: result.job_id, state: 'running', audio: undefined })
          await callbacks.current.onSaved().catch(() => {})
        } catch (reason) {
          if (!current()) return
          const rejected = reason instanceof ApiRequestError && [400, 401, 402, 403, 413, 429].includes(reason.status)
          patch(job.id, { state: rejected ? 'ready' : 'uncertain', error: reason instanceof ApiRequestError && reason.status === 402 ? b.insufficient : rejected ? b.unavailable : b.uncertain })
          break
        }
      }
    } catch (reason) { if (current()) setError(reason instanceof DOMException ? b.storage : b.unavailable) }
    finally { busyRef.current = false; if (mounted.current) setBusy(false) }
  }

  const ready = jobs.filter(job => job.state === 'ready')
  return <Sheet open={open} onClose={onClose} title={b.title} description={b.description} eyebrow="PRO" wide>
    {!allowed ? <div className="dt-batch"><p>{b.locked}</p><button className="dt-primary-button" type="button" onClick={onAccount}>{b.upgrade}</button></div> : <div className="dt-batch">
      <p className="dt-muted">{b.limits}</p>
      <div className="dt-batch__controls">
        <label>{b.language}<select aria-label={b.language} value={language} onChange={event => setLanguage(event.target.value)} disabled={busy}>{languageOptions().map(option => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>
        <label>{b.choose}<input aria-label={b.choose} type="file" accept="audio/*,.mp3,.m4a,.wav,.ogg,.flac,.webm,.aac" multiple disabled={busy} onChange={event => { const files = Array.from(event.target.files ?? []); event.target.value = ''; void choose(files) }} /></label>
      </div>
      {busy && <p role="status">{jobs.some(job => job.state === 'uploading') ? b.uploading : b.prepare}</p>}
      {error && <p role="alert">{error}</p>}
      {!jobs.length && <p className="dt-muted">{b.empty}</p>}
      <ul className="dt-batch__jobs">{jobs.map(job => <li key={job.id}>
        <div><strong>{job.name}</strong><small>{Math.ceil(job.seconds)} s · {job.cost === undefined ? '—' : formatUsageUSD(job.cost)}</small></div>
        <p role="status">{b[job.state]}</p>
        {job.error && <p role="alert">{job.error}</p>}
        {job.jobId && <small>ID: {job.jobId}</small>}
        <div className="dt-batch__actions">
          {job.error && job.jobId && job.state !== 'failed' && <button type="button" onClick={() => { try { patch(job.id, { error: undefined }) } catch { setError(b.storage) } }}>{b.retry}</button>}
          {['done', 'failed', 'ready', 'interrupted'].includes(job.state) && <button disabled={busy} type="button" onClick={() => { try { commit(jobsRef.current.filter(item => item.id !== job.id)) } catch { setError(b.storage) } }}>{b.remove}</button>}
        </div>
      </li>)}</ul>
      {ready.length > 0 && <><p>{b.estimate}: {ready.every(job => job.cost !== undefined) ? formatUsageUSD(ready.reduce((sum, job) => sum + (job.cost ?? 0), 0)) : '—'}</p><p className="dt-muted">{b.pricing}</p><button className="dt-primary-button" disabled={busy} type="button" onClick={() => { void start() }}>{ready.some(job => job.cost === undefined) ? b.quoteRetry : b.start}</button></>}
      <p className="dt-muted">{b.resume}</p>
      <div className="dt-batch__actions"><button type="button" onClick={onAccount}>{b.topup}</button><button type="button" onClick={onHistory}>{b.history}</button></div>
    </div>}
  </Sheet>
}
