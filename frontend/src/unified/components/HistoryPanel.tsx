import { useRef, useState } from 'react'
import { formatUsageUSD, type SessionCostSummary } from '../../api'
import { intlLocale, messages, useMessages } from '../../i18n'
import { Icon } from './Icon'

export interface HistorySession {
  id: string
  title: string
  createdAt: number
  durationSeconds: number
  status: string
  location: 'cloud' | 'local'
}

export interface HistoryOpenProgress {
  sessionId: string
  label: string
  /** 0–100 when known; null means indeterminate. */
  percent: number | null
}

interface HistoryPanelProps {
  activeSessionId: string
  /** Per-session cost summaries keyed by session id; cloud sessions only. */
  costs?: Record<string, SessionCostSummary>
  /** Older sessions exist beyond the loaded list. */
  hasMore?: boolean
  loading: boolean
  opening?: HistoryOpenProgress | null
  sessions: HistorySession[]
  onDelete: (session: HistorySession) => Promise<void>
  onLoad: (session: HistorySession) => Promise<void>
  onLoadMore?: () => Promise<void>
  /** Ends a stuck/remote active cloud session and cuts its live stream. */
  onEndSession?: (session: HistorySession) => Promise<void>
  /** Uploads a local-only session's transcripts to the cloud. */
  onUploadToCloud?: (session: HistorySession) => Promise<void>
}

function formatDate(value: number): string {
  return new Intl.DateTimeFormat(intlLocale(), {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(value)
}

function formatDuration(seconds: number): string {
  const { format } = messages()
  if (!seconds) return format.lessThanMinute
  const hours = Math.floor(seconds / 3_600)
  const minutes = Math.floor(seconds % 3_600 / 60)
  if (hours > 0) return format.hoursMinutes(hours, minutes)
  return format.minutes(Math.max(1, minutes))
}

export function HistoryPanel({
  activeSessionId,
  costs,
  hasMore = false,
  loading,
  opening = null,
  sessions,
  onDelete,
  onLoad,
  onLoadMore,
  onEndSession,
  onUploadToCloud,
}: HistoryPanelProps) {
  const m = useMessages()
  const untitled = m.common.untitledSession
  const deletingKeysRef = useRef(new Set<string>())
  const [deletingKeys, setDeletingKeys] = useState<Set<string>>(() => new Set())
  const workingKeysRef = useRef(new Set<string>())
  const [workingKeys, setWorkingKeys] = useState<Set<string>>(() => new Set())

  const deleteSession = async (session: HistorySession) => {
    const key = `${session.location}:${session.id}`
    if (deletingKeysRef.current.has(key)) return
    if (!window.confirm(m.history.confirmDelete(session.title || untitled))) return
    deletingKeysRef.current.add(key)
    setDeletingKeys(new Set(deletingKeysRef.current))
    try {
      await onDelete(session)
    } finally {
      deletingKeysRef.current.delete(key)
      setDeletingKeys(new Set(deletingKeysRef.current))
    }
  }

  const runRowAction = async (
    session: HistorySession,
    action: (session: HistorySession) => Promise<void>,
  ) => {
    const key = `${session.location}:${session.id}`
    if (workingKeysRef.current.has(key)) return
    workingKeysRef.current.add(key)
    setWorkingKeys(new Set(workingKeysRef.current))
    try {
      await action(session)
    } finally {
      workingKeysRef.current.delete(key)
      setWorkingKeys(new Set(workingKeysRef.current))
    }
  }

  const endSession = async (session: HistorySession) => {
    if (!onEndSession) return
    if (!window.confirm(m.history.confirmEnd(session.title || untitled))) return
    await runRowAction(session, onEndSession)
  }

  const uploadSession = async (session: HistorySession) => {
    if (!onUploadToCloud) return
    await runRowAction(session, onUploadToCloud)
  }

  // Only blank the list on the first fetch. Opening a session keeps the list
  // visible so users can see which item is loading and cancel by picking another.
  if (loading && sessions.length === 0) {
    return (
      <div className="dt-empty">
        <span className="dt-spinner" />
        {m.history.loading}
      </div>
    )
  }

  if (sessions.length === 0) {
    return (
      <div className="dt-empty">
        <Icon name="history" size={28} />
        <strong>{m.history.emptyTitle}</strong>
        <span>{m.history.emptyBody}</span>
      </div>
    )
  }

  return (
    <div className="dt-history-list">
      {loading && (
        <div className="dt-history-list__banner" aria-live="polite">
          <span className="dt-spinner" />
          {m.history.refreshing}
        </div>
      )}
      {sessions.map((session) => {
        const sessionKey = `${session.location}:${session.id}`
        const isOpening = opening?.sessionId === session.id
        const isActive = session.id === activeSessionId
        const isDeleting = deletingKeys.has(sessionKey)
        const isWorking = workingKeys.has(sessionKey)
        const canEnd = Boolean(
          onEndSession && session.location === 'cloud' && session.status === 'active',
        )
        const canUpload = Boolean(onUploadToCloud && session.location === 'local')
        // Realtime cost only (transcription + translation); AI charges are a
        // separate concern and stay in the account panel's usage list.
        const cost = costs?.[session.id]
        const costUsd = cost ? cost.transcription_usd + cost.translation_usd : 0
        return (
          <article
            className={[
              'dt-history-item',
              isActive ? ' is-active' : '',
              isOpening ? ' is-opening' : '',
              isDeleting ? ' is-deleting' : '',
            ].join('')}
            key={sessionKey}
          >
            <button
              className="dt-history-item__main"
              aria-busy={isOpening || isDeleting || undefined}
              disabled={isDeleting}
              onClick={() => { void onLoad(session) }}
              type="button"
            >
              <span className="dt-history-item__icon">
                {isOpening
                  ? <span className="dt-spinner" aria-hidden />
                  : <Icon name={session.location === 'cloud' ? 'cloud' : 'archive'} size={18} />}
              </span>
              <span>
                <strong>{session.title || untitled}</strong>
                <small>
                  {isDeleting
                    ? m.history.deleting
                    : isOpening
                    ? opening.label
                    : `${formatDate(session.createdAt)} · ${formatDuration(session.durationSeconds)}${
                      costUsd > 0 ? ` · ${formatUsageUSD(costUsd)}` : ''
                    }`}
                </small>
              </span>
              <span className="dt-history-item__status">
                {session.location === 'cloud'
                  ? session.status === 'active' ? m.history.cloudActive : m.history.cloud
                  : m.history.local}
              </span>
            </button>
            {isOpening && (
              <div
                className="dt-history-item__progress"
                role="progressbar"
                aria-label={opening.label}
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={opening.percent ?? undefined}
                aria-valuetext={
                  opening.percent === null
                    ? opening.label
                    : `${opening.label} ${Math.round(opening.percent)}%`
                }
              >
                <span
                  className={
                    opening.percent === null
                      ? 'dt-history-item__progress-bar is-indeterminate'
                      : 'dt-history-item__progress-bar'
                  }
                  style={
                    opening.percent === null
                      ? undefined
                      : { width: `${Math.max(4, Math.min(100, opening.percent))}%` }
                  }
                />
              </div>
            )}
            <span className="dt-history-item__actions">
              {canEnd && (
                <button
                  aria-label={m.history.endAria(session.title || untitled)}
                  title={m.history.endTitle}
                  className="dt-icon-button"
                  disabled={isOpening || isDeleting || isWorking}
                  onClick={() => { void endSession(session) }}
                  type="button"
                >
                  {isWorking
                    ? <span className="dt-spinner" aria-hidden />
                    : <Icon name="stop" size={17} />}
                </button>
              )}
              {canUpload && (
                <button
                  aria-label={m.history.uploadAria(session.title || untitled)}
                  title={m.history.uploadTitle}
                  className="dt-icon-button"
                  disabled={isOpening || isDeleting || isWorking}
                  onClick={() => { void uploadSession(session) }}
                  type="button"
                >
                  {isWorking
                    ? <span className="dt-spinner" aria-hidden />
                    : <Icon name="cloud" size={17} />}
                </button>
              )}
              <button
                aria-label={isDeleting ? m.history.deletingAria(session.title || untitled) : m.history.deleteAria(session.title || untitled)}
                className="dt-icon-button dt-icon-button--danger"
                disabled={isOpening || isDeleting || isWorking}
                onClick={() => { void deleteSession(session) }}
                type="button"
              >
                {isDeleting
                  ? <span className="dt-spinner" aria-hidden />
                  : <Icon name="close" size={17} />}
              </button>
            </span>
          </article>
        )
      })}
      {hasMore && onLoadMore && (
        <button
          className="dt-history-list__more"
          disabled={loading}
          onClick={() => { void onLoadMore() }}
          type="button"
        >
          {loading ? m.history.loadingMore : m.history.loadMore}
        </button>
      )}
    </div>
  )
}
