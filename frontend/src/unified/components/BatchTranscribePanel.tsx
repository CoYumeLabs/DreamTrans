import { useCallback, useEffect, useRef, useState } from 'react'
import { formatUsageUSD } from '../../api'
import { useMessages } from '../../i18n'
import { ApiRequestError, getStoredUser } from '../../pro/api/auth'
import { batchStatus, listBatchJobs, retryBatchJob, MAX_BATCH_BYTES, prepareBatchAudio, quoteBatch, saveBatchResult, submitBatch } from '../workspace/batchTranscription'
import { languageOptions } from '../workspace/languageOptions'
import { Icon } from './Icon'
import { Sheet } from './Sheet'
import './BatchTranscribePanel.css'

type JobState = 'ready' | 'uploading' | 'running' | 'saving' | 'done' | 'failed' | 'uncertain' | 'interrupted'
interface Job { id: string; name: string; language: string; seconds: number; cost?: number; jobId?: string; state: JobState; error?: string; audio?: Blob }
interface Props { ownerId: string | null; allowed: boolean; open: boolean; sourceLanguage: string; onClose: () => void; onAccount: () => void; onHistory: () => void; onSaved: () => Promise<void> }

const jobTone: Record<JobState, string> = { ready: 'neutral', uploading: 'working', running: 'working', saving: 'working', done: 'success', failed: 'danger', uncertain: 'warning', interrupted: 'warning' }
function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.ceil(seconds))
  const minutes = Math.floor(total / 60), rest = total % 60
  return minutes ? `${minutes}:${String(rest).padStart(2, '0')}` : `${rest}s`
}

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
  const dismissed = useRef<Set<string>>(new Set())
  useEffect(() => {
    try { dismissed.current = new Set(JSON.parse(localStorage.getItem(`${storageKey(ownerId)}:dismissed`) ?? '[]')) } catch { dismissed.current = new Set() }
  }, [ownerId])
  const [language, setLanguage] = useState(sourceLanguage)
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)
  const mounted = useRef(false)
  const checking = useRef(new Set<string>())
  const [error, setError] = useState('')
  const [dragOver, setDragOver] = useState(false)
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
          if (!result.server_managed && !result.transcript) throw new Error('Missing transcript')
          patch(job.id, { state: 'saving', error: undefined })
          if (!result.server_managed && result.transcript) await saveBatchResult(ownerId!, job.id, job.name, job.language, result.transcript)
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

  useEffect(() => {
    if (!ownerId || !open) return
    let disposed = false
    const recover = async () => {
      try {
        const remote = await listBatchJobs()
        if (disposed || !current() || !Array.isArray(remote)) return
        const next = [...jobsRef.current]
        let completed = false
        for (const row of remote) {
          if (dismissed.current.has(row.id)) continue
          const index = next.findIndex(job => job.id === row.id || (row.job_id && job.jobId === row.job_id))
          const previous = index >= 0 ? next[index] : undefined
          // An upload still being transmitted keeps its local progress.
          if (previous?.state === 'uploading') continue
          const state: JobState = row.status === 'done' ? 'done' : row.status === 'error' ? 'failed' : row.status === 'saving' ? 'saving' : row.job_id ? 'running' : 'uncertain'
          const recovered: Job = { ...previous, id: row.id, name: row.name, language: row.language, seconds: row.seconds, jobId: row.job_id || undefined, state, error: row.error ? (state === 'failed' ? callbacks.current.b.failed : callbacks.current.b.error) : undefined }
          if (state === 'done' && previous?.state !== 'done') completed = true
          if (index >= 0) next[index] = recovered
          else next.push(recovered)
        }
        commit(next)
        if (completed) await callbacks.current.onSaved().catch(() => {})
      } catch { /* Local jobs remain usable during a temporary list outage. */ }
    }
    void recover()
    const timer = window.setInterval(() => { void recover() }, 5000)
    return () => { disposed = true; window.clearInterval(timer) }
  }, [ownerId, current, commit, open])

  async function choose(files: File[]) {
    if (busyRef.current || !allowed) return
    const existing = jobsRef.current.filter(job => job.audio)
    if (files.length + existing.length > 10 || files.reduce((sum, file) => sum + file.size, 0) > MAX_BATCH_BYTES || jobsRef.current.filter(job => !['done', 'failed', 'interrupted'].includes(job.state)).length + files.length > 30) { setError(b.tooMany); return }
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
          const result = await submitBatch(job.audio, job.language, job.id, job.name)
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
  const estimate = ready.every(job => job.cost !== undefined) ? formatUsageUSD(ready.reduce((sum, job) => sum + (job.cost ?? 0), 0)) : '—'
  const pickFiles = (list: FileList | null) => { const files = Array.from(list ?? []); if (files.length) void choose(files) }
  const forget = (job: Job) => {
    try {
      dismissed.current.add(job.id)
      localStorage.setItem(`${storageKey(ownerId)}:dismissed`, JSON.stringify([...dismissed.current].slice(-200)))
      commit(jobsRef.current.filter(item => item.id !== job.id))
    } catch { setError(b.storage) }
  }

  return (
    <Sheet open={open} onClose={onClose} title={b.title} description={b.description} eyebrow="PRO" wide>
      {!allowed ? (
        <div className="dt-batch">
          <div className="dt-batch__locked">
            <p>{b.locked}</p>
            <button className="dt-button dt-button--primary" type="button" onClick={onAccount}>{b.upgrade}</button>
          </div>
        </div>
      ) : (
        <div className="dt-batch">
          <div className="dt-batch__controls">
            <label className="dt-field">
              <span>{b.language}</span>
              <select aria-label={b.language} disabled={busy} value={language} onChange={event => setLanguage(event.target.value)}>
                {languageOptions().map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
              </select>
            </label>
            <label
              className={`dt-batch__drop${dragOver ? ' is-over' : ''}${busy ? ' is-disabled' : ''}`}
              onDragEnter={event => { event.preventDefault(); if (!busy) setDragOver(true) }}
              onDragOver={event => { event.preventDefault() }}
              onDragLeave={() => setDragOver(false)}
              onDrop={event => { event.preventDefault(); setDragOver(false); if (!busy) pickFiles(event.dataTransfer.files) }}
            >
              <Icon name="paperclip" size={20} />
              <strong>{b.choose}</strong>
              <small>{b.drop}</small>
              <input
                accept="audio/*,.mp3,.m4a,.wav,.ogg,.flac,.webm,.aac"
                aria-label={b.choose}
                disabled={busy}
                multiple
                type="file"
                onChange={event => { const list = event.target.files; pickFiles(list); event.target.value = '' }}
              />
            </label>
          </div>
          <p className="dt-muted">{b.limits}</p>

          {busy && <p className="dt-muted" role="status">{jobs.some(job => job.state === 'uploading') ? b.uploading : b.prepare}</p>}
          {error && <p className="dt-batch__alert" role="alert">{error}</p>}
          {!jobs.length && <p className="dt-muted">{b.empty}</p>}

          {jobs.length > 0 && (
            <ul className="dt-batch__jobs" aria-label={b.queue}>
              {jobs.map(job => (
                <li className="dt-batch__job" key={job.id}>
                  <div className="dt-batch__job-head">
                    <strong>{job.name}</strong>
                    <small>{job.cost === undefined ? '—' : formatUsageUSD(job.cost)}</small>
                  </div>
                  <div className="dt-batch__job-meta">
                    <span className={`dt-status dt-status--${jobTone[job.state]}`}><i /><span role="status">{b[job.state]}</span></span>
                    <span>{formatDuration(job.seconds)}</span>
                    {(job.jobId || job.state === 'uncertain') && <span className="dt-batch__job-id">ID {job.jobId || job.id}</span>}
                  </div>
                  {job.error && <p className="dt-batch__job-error" role="alert">{job.error}</p>}
                  {(job.state === 'uncertain' || (job.error && job.jobId && job.state !== 'failed') || ['done', 'failed', 'ready', 'interrupted'].includes(job.state)) && (
                    <div className="dt-batch__actions">
                      {job.state === 'uncertain' && (
                        <button className="dt-button dt-button--secondary dt-button--small" type="button" onClick={() => { void retryBatchJob(job.id).catch(() => setError(b.error)) }}>{b.retry}</button>
                      )}
                      {job.error && job.jobId && job.state !== 'failed' && (
                        <button className="dt-button dt-button--secondary dt-button--small" type="button" onClick={() => { try { patch(job.id, { error: undefined }) } catch { setError(b.storage) } }}>{b.retry}</button>
                      )}
                      {['done', 'failed', 'ready', 'interrupted'].includes(job.state) && (
                        <button className="dt-button dt-button--text dt-button--small" disabled={busy} type="button" onClick={() => forget(job)}>{b.remove}</button>
                      )}
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}

          {ready.length > 0 && (
            <div className="dt-batch__summary">
              <div>
                <small>{b.estimate} · {ready.length} {b.files}</small>
                <strong>{estimate}</strong>
                <small>{b.pricing}</small>
              </div>
              <button className="dt-button dt-button--primary" disabled={busy} type="button" onClick={() => { void start() }}>
                {ready.some(job => job.cost === undefined) ? b.quoteRetry : b.start}
              </button>
            </div>
          )}

          <div className="dt-batch__footer">
            <p className="dt-muted">{b.resume}</p>
            <div className="dt-batch__actions">
              <button className="dt-button dt-button--secondary" type="button" onClick={onAccount}>{b.topup}</button>
              <button className="dt-button dt-button--secondary" type="button" onClick={onHistory}>{b.history}</button>
            </div>
          </div>
        </div>
      )}
    </Sheet>
  )
}
