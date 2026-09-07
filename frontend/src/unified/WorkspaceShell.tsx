import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  formatHours,
  formatUSD,
  formatUsageUSD,
  getSessionCostSummaries,
  type AccountBalance,
  type AccountSummary,
  type TrainingProgramInfo,
  type RagConfig,
  type SessionCostSummary,
} from '../api'
import type { User } from '../pro/api/auth'
import { intlLocale, useMessages, type Messages } from '../i18n'
import {
  TranscriptFeed,
  TranscriptFeedModeSwitch,
  type TranscriptChromeMode,
  type TranscriptFeedItem,
} from './feed'
import { resolveAiPrompt, type UnifiedSettings } from './hooks/useUnifiedSettings'
import type { SessionCostView, TransportDiagnostics } from './hooks/useUnifiedWorkspace'
import { BatchTranscribePanel } from './components/BatchTranscribePanel'
import { AccountPanel } from './components/AccountPanel'
import { BrandMark } from './components/BrandMark'
import { AssistantPanel } from './components/AssistantPanel'
import { GuideTour, type TourStep } from './components/GuideTour'
import {
  HistoryPanel,
  type HistoryOpenProgress,
  type HistorySession,
} from './components/HistoryPanel'
import { Icon } from './components/Icon'
import { InsightsPanel } from './components/InsightsPanel'
import { AnnouncementBanner } from './components/AnnouncementBanner'
import { OnboardingDialog } from './components/OnboardingDialog'
import { TrainingProgramDialog } from './components/TrainingProgramDialog'
import { RecorderBar, type RecorderStatus } from './components/RecorderBar'
import { SettingsPanel } from './components/SettingsPanel'
import { Sheet } from './components/Sheet'
import { useAnnouncements } from './hooks/useAnnouncements'
import { useOnboarding } from './hooks/useOnboarding'
import { adminNavigationState } from './workspace/adminNavigation'
import { isInsufficientBalanceMessage } from './workspace/billingErrors'
import { audioSourceLabel, languageLabel } from './workspace/languageOptions'

export interface WorkspaceStats {
  finalSegments: number
  translatedSegments: number
  speakers: number
  topWords: Array<{ word: string; count: number }>
}

export interface WorkspaceShellProps {
  account: AccountSummary | null
  allowUserApiKey: boolean
  balance: AccountBalance | null
  connectionLabel: string
  durationLabel: string
  error: string | null
  feedGeneration: number
  feedItems: readonly TranscriptFeedItem[]
  historyLoading: boolean
  historyOpening: HistoryOpenProgress | null
  historySessions: HistorySession[]
  legacyHistoryCount: number
  paymentsEnabled: boolean
  pendingWrites: number
  ragEnabled: boolean
  recorderStatus: RecorderStatus
  paymentRequired: boolean
  sessionCost: SessionCostView | null
  sessionId: string
  sessionSourceLanguage: string
  settings: UnifiedSettings
  stats: WorkspaceStats
  title: string
  titleGenerating: boolean
  transportDiagnostics: TransportDiagnostics | null
  transcriptContext: string
  trainingProgram: TrainingProgramInfo
  user: User | null
  onClearError: () => void
  onContinue: () => Promise<void>
  onDeleteHistory: (session: HistorySession) => Promise<void>
  onEndHistorySession: (session: HistorySession) => Promise<void>
  onUploadHistorySessionToCloud: (session: HistorySession) => Promise<void>
  onDownloadAudio: () => Promise<void>
  onDownloadText: (mode: 'original' | 'translation' | 'bilingual') => Promise<void>
  onLoadHistory: (session: HistorySession) => Promise<void>
  onMigrateLegacyHistory: () => Promise<void>
  onLogout: () => Promise<void>
  onPauseToggle: () => void
  onRefreshAccount: () => Promise<void>
  onRefreshHistory: () => Promise<void>
  onRequestLogin: () => void
  onSettingsChange: (patch: Partial<UnifiedSettings>) => void
  onStart: () => Promise<void>
  onStop: () => Promise<void>
  onTitleChange: (title: string) => Promise<void>
  onGenerateTitle: () => Promise<void>
  onTrainingOptInChange: (optIn: boolean) => Promise<boolean>
}

type PanelName = 'batch' | 'assistant' | 'history' | 'insights' | 'settings' | 'tools' | 'account'

const statusTone: Record<RecorderStatus, string> = {
  idle: 'neutral',
  starting: 'working',
  recording: 'live',
  paused: 'warning',
  stopping: 'working',
  reconnecting: 'warning',
  error: 'danger',
}

function currentDateLabel(): string {
  return new Intl.DateTimeFormat(intlLocale(), {
    month: 'long',
    day: 'numeric',
    weekday: 'short',
  }).format(Date.now())
}

/** `$12.50 · ≈ 16 小时`; hours follow the live balance using the account's hourly price. */
function balanceLabel(balance: AccountBalance | null, account: AccountSummary | null, localMode: string): string {
  if (!balance) return localMode
  const money = formatUSD(balance.available_usd)
  if (!account) return money
  const hours = account.realtime_hour_usd > 0
    ? balance.available_usd / account.realtime_hour_usd
    : account.estimated_realtime_hours
  return `${money} · ${formatHours(hours)}`
}

/**
 * Interface tour shown after the first-run wizard. Each step lists desktop
 * and mobile targets; the tour skips whichever is not rendered.
 */
function workspaceTourSteps(m: Messages): TourStep[] {
  const s = m.tour.steps
  return [
    { id: 'record', selectors: ['[data-tour="record"]'], ...s.record },
    { id: 'mode-switch', selectors: ['.dt-feed-toolbar .dt-transcript-feed-mode-switch'], ...s.modeSwitch },
    { id: 'assistant', selectors: ['[data-tour="assistant"]'], ...s.assistant },
    { id: 'history', selectors: ['[data-tour="history"]', '[data-tour="history-mobile"]'], ...s.history },
    { id: 'settings', selectors: ['[data-tour="settings"]'], ...s.settings },
    { id: 'account', selectors: ['[data-tour="account"]', '[data-tour="account-mobile"]'], ...s.account },
  ]
}

type BillingReturn = 'success' | 'cancel' | null

/** Reads and strips `?billing=success|cancel` left by the Stripe redirect. */
function consumeBillingReturn(): BillingReturn {
  if (typeof window === 'undefined') return null
  const url = new URL(window.location.href)
  const value = url.searchParams.get('billing')
  if (value !== 'success' && value !== 'cancel') return null
  url.searchParams.delete('billing')
  window.history.replaceState(window.history.state, '', `${url.pathname}${url.search}${url.hash}`)
  return value
}

const BILLING_RETURN_REFRESH_DELAYS_MS = [0, 3_000, 6_000]

/** Reads and strips `?session=<id>` left by 学习空间 deep links. */
function trainingPromptKey(ownerId: string | null): string {
  return `yufolo.training-prompt-dismissed:${ownerId ?? 'guest'}`
}

function readTrainingPromptDismissed(ownerId: string | null): boolean {
  try {
    return window.localStorage.getItem(trainingPromptKey(ownerId)) === '1'
  } catch {
    return false
  }
}

function writeTrainingPromptDismissed(ownerId: string | null): void {
  try {
    window.localStorage.setItem(trainingPromptKey(ownerId), '1')
  } catch {
    // Storage may be unavailable; the prompt simply returns next visit.
  }
}

function consumeSessionDeepLink(): string | null {
  if (typeof window === 'undefined') return null
  const url = new URL(window.location.href)
  const value = url.searchParams.get('session')
  if (!value) return null
  url.searchParams.delete('session')
  window.history.replaceState(window.history.state, '', `${url.pathname}${url.search}${url.hash}`)
  return value
}

export function WorkspaceShell(props: WorkspaceShellProps) {
  const {
    account,
    allowUserApiKey,
    balance,
    connectionLabel,
    durationLabel,
    error,
    feedGeneration,
    feedItems,
    historyLoading,
    historyOpening,
    historySessions,
    legacyHistoryCount,
    transportDiagnostics,
    paymentsEnabled,
    pendingWrites,
    ragEnabled,
    recorderStatus,
    sessionCost,
    sessionId,
    sessionSourceLanguage,
    settings,
    stats,
    title,
    titleGenerating,
    transcriptContext,
    trainingProgram,
    user,
    onClearError,
    onContinue,
    onDeleteHistory,
    onEndHistorySession,
    onUploadHistorySessionToCloud,
    onDownloadAudio,
    onDownloadText,
    onLoadHistory,
    onMigrateLegacyHistory,
    onLogout,
    onPauseToggle,
    onRefreshAccount,
    onRefreshHistory,
    onRequestLogin,
    onSettingsChange,
    onStart,
    onStop,
    onTitleChange,
    onGenerateTitle,
  } = props
  const [panel, setPanel] = useState<PanelName | null>(null)
  const [assistantDraft, setAssistantDraft] = useState('')
  // A /pro?session=<id> deep link (e.g. from the 学习空间) waiting for history.
  const [pendingSession, setPendingSession] = useState(consumeSessionDeepLink)
  const [notice, setNotice] = useState<string | null>(null)
  const m = useMessages()
  const w = m.workspace
  const { paymentRequired } = props
  const status = { label: w.status[recorderStatus], tone: statusTone[recorderStatus] }
  const balanceError = isInsufficientBalanceMessage(error)
  const memberActive = balance?.member_active ?? account?.member_active ?? false

  const generateTitleHint = !ragEnabled
    ? w.hints.aiUnavailable
    : !user
      ? w.hints.titleLoginFirst
      : !sessionId || !transcriptContext
        ? w.hints.titleNeedsContent
        : titleGenerating
          ? w.hints.titleGenerating
          : w.hints.titleGenerate
  const canGenerateTitle = Boolean(
    ragEnabled && user && sessionId && transcriptContext && !titleGenerating
      && recorderStatus !== 'starting' && recorderStatus !== 'stopping',
  )
  const learningMode = settings.assistMode === 'learn'
  const effectiveViewMode = learningMode
    ? 'original'
    : settings.translationEnabled
      ? settings.viewMode
      : 'original'
  const chromeMode: TranscriptChromeMode = learningMode
    ? 'learn'
    : effectiveViewMode

  const setChromeMode = (mode: TranscriptChromeMode) => {
    if (mode === 'learn') {
      onSettingsChange({ assistMode: 'learn' })
      return
    }
    onSettingsChange({
      assistMode: 'interpret',
      viewMode: mode,
      // Leaving learning should not strand the user with translation off.
      ...(mode !== 'original' ? { translationEnabled: true } : {}),
    })
  }
  const active = recorderStatus === 'recording'
    || recorderStatus === 'paused'
    || recorderStatus === 'reconnecting'
    || recorderStatus === 'error'
  const studyEnabled = ragEnabled && Boolean(user)
  const studyNavigationDisabled = !studyEnabled || recorderStatus !== 'idle'
  const studyNavigationTitle = !ragEnabled
    ? w.hints.aiUnavailable
    : !user
      ? w.hints.studyLoginFirst
      : recorderStatus !== 'idle'
        ? w.hints.studyStopFirst
        : w.hints.studyDescription

  // Deep-linked session opens once the history list can resolve it.
  useEffect(() => {
    if (!pendingSession) return
    const match = historySessions.find(({ id }) => id === pendingSession)
    if (match) {
      setPendingSession(null)
      void onLoadHistory(match)
      return
    }
    if (!historyLoading && historySessions.length > 0) {
      setPendingSession(null)
      setNotice(w.notices.sessionNotFound)
    }
  }, [pendingSession, historySessions, historyLoading, onLoadHistory, w.notices.sessionNotFound])

  // Realtime costs for the history list. Keyed off the joined id string so a
  // refresh that returns the same sessions does not refetch.
  const [historyCosts, setHistoryCosts] = useState<Record<string, SessionCostSummary>>({})
  const recorderIdle = recorderStatus === 'idle'
  const historyCostIds = historySessions
    .filter((session) => session.location === 'cloud')
    .slice(0, 100)
    .map((session) => session.id)
    .join(',')
  useEffect(() => {
    if (!user || !historyCostIds) {
      setHistoryCosts({})
      return
    }
    let effectActive = true
    // recorderIdle retriggers this after a recording ends, once its
    // reservation tail has settled.
    void getSessionCostSummaries(historyCostIds.split(','))
      .then((summaries) => {
        if (!effectActive) return
        const next: Record<string, SessionCostSummary> = {}
        for (const summary of summaries) next[summary.session_id] = summary
        setHistoryCosts(next)
      })
      .catch(() => {
        // The list renders fine without cost figures.
      })
    return () => { effectActive = false }
  }, [user, historyCostIds, recorderIdle])
  const transitionBusy = recorderStatus === 'starting' || recorderStatus === 'stopping'
  const adminNavigation = adminNavigationState(user?.role, recorderStatus)
  const adminNavigationDisabled = adminNavigation === 'disabled'
  const adminNavigationTitle = adminNavigationDisabled
    ? w.hints.adminStopFirst
    : w.hints.adminOpen
  const today = useMemo(currentDateLabel, [m])
  // Stripe sends the browser back to /pro?billing=success|cancel. The webhook
  // that credits the wallet may lag the redirect, so re-read the account a
  // few times instead of trusting the first response.
  const refreshAccountRef = useRef(onRefreshAccount)
  refreshAccountRef.current = onRefreshAccount
  useEffect(() => {
    if (!user) return
    const outcome = consumeBillingReturn()
    if (!outcome) return
    if (outcome === 'cancel') {
      setNotice(w.notices.paymentCancelled)
      return
    }
    setNotice(w.notices.paymentDone)
    const timers = BILLING_RETURN_REFRESH_DELAYS_MS.map((delay) => globalThis.setTimeout(() => {
      void refreshAccountRef.current()
    }, delay))
    return () => {
      for (const timer of timers) globalThis.clearTimeout(timer)
    }
  }, [user, w.notices.paymentCancelled, w.notices.paymentDone])
  // An unedited default prompt follows the interface language (m tracks it).
  const aiPrompt = resolveAiPrompt(settings.aiPrompt).trim()
  const aiConfig = useMemo<RagConfig>(() => ({
    ...(allowUserApiKey && settings.aiApiKey.trim()
      ? {
          api_key: settings.aiApiKey.trim(),
          ...(settings.aiApiBase.trim() ? { api_base: settings.aiApiBase.trim() } : {}),
          ...(settings.aiModel.trim() ? { model: settings.aiModel.trim() } : {}),
        }
      : {}),
    ...(aiPrompt ? { prompt: aiPrompt } : {}),
  }), [
    aiPrompt,
    allowUserApiKey,
    settings.aiApiBase,
    settings.aiApiKey,
    settings.aiModel,
  ])

  const closePanel = useCallback(() => setPanel(null), [])
  const { announcements, dismiss: dismissAnnouncement } = useAnnouncements(user?.id ?? null)
  const onboarding = useOnboarding({
    ownerId: user?.id ?? null,
    historyLoading,
    historyCount: historySessions.length,
    recorderStatus,
  })
  const tourSteps = useMemo(() => {
    const steps = workspaceTourSteps(m)
    return ragEnabled ? steps : steps.filter((step) => step.id !== 'assistant')
  }, [ragEnabled, m])
  // Accounts that finished onboarding before the programme existed get asked
  // once; "later" is remembered per account in this browser, and Settings
  // keeps the switch.
  const [trainingPromptDismissed, setTrainingPromptDismissed] = useState(false)
  useEffect(() => {
    setTrainingPromptDismissed(readTrainingPromptDismissed(user?.id ?? null))
  }, [user?.id])
  const dismissTrainingPrompt = useCallback(() => {
    writeTrainingPromptDismissed(user?.id ?? null)
    setTrainingPromptDismissed(true)
  }, [user?.id])
  const trainingPromptOpen = Boolean(user)
    && trainingProgram.available
    && user?.training_opt_in === null
    && onboarding.phase === 'done'
    && recorderStatus === 'idle'
    && panel === null
    && !trainingPromptDismissed

  const replayOnboarding = useCallback(() => {
    closePanel()
    onboarding.openWizard()
  }, [closePanel, onboarding])
  const explainTerm = useCallback((term: string) => {
    setAssistantDraft(w.explainPrompt(term))
    setPanel('assistant')
  }, [w])

  // Select any text inside the live transcript to get a one-tap AI
  // explanation (parity with the old classic UI's selection lookup).
  const stageRef = useRef<HTMLElement | null>(null)
  const [selectionLookup, setSelectionLookup] = useState<
    { text: string; x: number; y: number } | null
  >(null)
  useEffect(() => {
    if (!ragEnabled) return
    const handleSelection = () => {
      const stage = stageRef.current
      const selection = window.getSelection()
      if (!stage || !selection || selection.isCollapsed) {
        setSelectionLookup(null)
        return
      }
      const text = selection.toString().trim().replace(/\s+/g, ' ')
      if (!text || text.length > 120) {
        setSelectionLookup(null)
        return
      }
      const anchor = selection.anchorNode
      if (!anchor || !stage.contains(anchor)) {
        setSelectionLookup(null)
        return
      }
      const rect = selection.getRangeAt(0).getBoundingClientRect()
      const stageRect = stage.getBoundingClientRect()
      setSelectionLookup({
        text,
        x: Math.min(
          Math.max(rect.left + rect.width / 2 - stageRect.left, 40),
          stageRect.width - 40,
        ),
        y: Math.max(rect.top - stageRect.top, 8),
      })
    }
    document.addEventListener('selectionchange', handleSelection)
    return () => document.removeEventListener('selectionchange', handleSelection)
  }, [ragEnabled])
  const loadHistory = async (session: HistorySession) => {
    await onLoadHistory(session)
    closePanel()
  }

  return (
    <div
      className={`dt-app${settings.reducedEffects ? ' dt-app--reduced-effects' : ''}`}
      data-recorder-status={recorderStatus}
    >
      <aside className="dt-sidebar" aria-label={w.nav.aria}>
        <div className="dt-brand">
          <BrandMark className="dt-brand__mark" />
          <span>
            <strong>Yufolo</strong>
            <small>{user ? w.cloudWorkspace : w.localWorkspace}</small>
          </span>
        </div>

        <nav className="dt-nav">
          <button className="is-active" type="button">
            <Icon name="mic" size={18} />
            <span>{w.nav.live}</span>
            {active && <i className="dt-nav__live" aria-label={w.nav.recordingAria} />}
          </button>
          <button onClick={() => setPanel('batch')} type="button">
            <Icon name="paperclip" size={18} />
            <span>{m.batch.title}</span><small>Pro</small>
          </button>
          <button
            disabled={studyNavigationDisabled}
            onClick={() => { window.location.assign('/pro/study') }}
            title={studyNavigationTitle}
            type="button"
          >
            <Icon name="map" size={18} />
            <span>{w.nav.study}</span>
          </button>
          {adminNavigation !== 'hidden' && (
            <button
              disabled={adminNavigationDisabled}
              onClick={() => { window.location.assign('/pro/admin') }}
              title={adminNavigationTitle}
              type="button"
            >
              <Icon name="shield" size={18} />
              <span>{w.nav.admin}</span>
            </button>
          )}
        </nav>

        <div className="dt-sidebar__history-heading" data-tour="history">
          <span>{w.nav.recentSessions}</span>
          <button
            aria-label={w.nav.refreshHistory}
            className="dt-icon-button"
            onClick={() => { void onRefreshHistory() }}
            type="button"
          >
            <Icon name="history" size={16} />
          </button>
        </div>
        <div className="dt-sidebar__history">
          <LegacyHistoryNotice
            busy={historyLoading}
            count={legacyHistoryCount}
            disabled={recorderStatus !== 'idle'}
            onMigrate={onMigrateLegacyHistory}
          />
          <HistoryPanel
            activeSessionId={sessionId}
            costs={historyCosts}
            loading={historyLoading}
            opening={historyOpening}
            sessions={historySessions}
            onDelete={onDeleteHistory}
            onLoad={onLoadHistory}
            onEndSession={onEndHistorySession}
            {...(user ? { onUploadToCloud: onUploadHistorySessionToCloud } : {})}
          />
        </div>

        <div className="dt-sidebar__footer">
          <button
            className="dt-account-chip"
            data-tour="account"
            onClick={() => setPanel('account')}
            type="button"
          >
            <span className="dt-account-chip__avatar">
              {user?.name?.trim().slice(0, 1).toUpperCase() || m.common.guestInitial}
            </span>
            <span>
              <strong>
                {user?.name || m.common.guest}
                {memberActive && <em className="dt-pro-badge">Pro</em>}
              </strong>
              <small>{balanceLabel(balance, account, m.format.localMode)}</small>
            </span>
            <Icon name="more" size={17} />
          </button>
        </div>
      </aside>

      <section className="dt-workspace">
        <header className="dt-topbar">
          <button
            aria-label={w.nav.openHistory}
            className="dt-icon-button dt-mobile-only"
            data-tour="history-mobile"
            onClick={() => setPanel('history')}
            type="button"
          >
            <Icon name="menu" />
          </button>

          <div className="dt-session-heading">
            <div className="dt-session-heading__meta">
              <span>{today}</span>
              <span aria-hidden="true">/</span>
              <span className={`dt-status dt-status--${status.tone}`}>
                <i />
                {status.label}
              </span>
              {sessionCost && sessionCost.realtimeUsd > 0 && (
                <>
                  <span aria-hidden="true">/</span>
                  <span
                    className="dt-session-cost"
                    title={w.hints.sessionCost(
                      sessionCost.approximate,
                      sessionCost.aiUsd > 0 ? w.hints.sessionCostAi(formatUsageUSD(sessionCost.aiUsd)) : '',
                    )}
                  >
                    {sessionCost.approximate ? '≈ ' : ''}
                    {formatUsageUSD(sessionCost.realtimeUsd)}
                  </span>
                </>
              )}
            </div>
            <div className="dt-session-title-row">
              <input
                aria-label={w.hints.sessionTitle}
                className="dt-session-title"
                defaultValue={title}
                key={`${sessionId || 'empty'}:${title}`}
                onBlur={(event) => {
                  const nextTitle = event.currentTarget.value.trim()
                  if (nextTitle && nextTitle !== title) void onTitleChange(nextTitle)
                }}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') event.currentTarget.blur()
                }}
              />
              <button
                aria-label={titleGenerating ? w.hints.titleGenerating : w.hints.titleGenerate}
                className={`dt-icon-button dt-session-title__ai${titleGenerating ? ' is-busy' : ''}`}
                disabled={!canGenerateTitle}
                onClick={() => void onGenerateTitle()}
                title={generateTitleHint}
                type="button"
              >
                <Icon name="sparkles" size={16} />
              </button>
            </div>
          </div>

          <div className="dt-topbar__actions">
            <span className="dt-connection" title={w.hints.connection}>
              <Icon name="cloud" size={16} />
              {connectionLabel}
            </span>
            <button
              aria-label={w.hints.downloads}
              className="dt-icon-button"
              onClick={() => setPanel('tools')}
              type="button"
            >
              <Icon name="download" />
            </button>
            <button
              aria-label={m.common.settings}
              className="dt-icon-button"
              data-tour="settings"
              onClick={() => setPanel('settings')}
              type="button"
            >
              <Icon name="settings" />
            </button>
            <button
              aria-label={m.common.account}
              className="dt-topbar__avatar"
              data-tour="account-mobile"
              onClick={() => setPanel('account')}
              type="button"
            >
              {user?.name?.trim().slice(0, 1).toUpperCase() || m.common.guestInitial}
            </button>
          </div>
        </header>

        {(paymentRequired || error) && (
          <div className={`dt-alert${paymentRequired ? ' dt-alert--payment' : ''}`} role="alert">
            <span>
              <strong>{paymentRequired ? w.runtime.paymentPaused : w.notices.problem}</strong>
              {paymentRequired ? w.runtime.paymentPausedDetail : error}
              {paymentRequired && error && <span>{error}</span>}
            </span>
            {(paymentRequired || balanceError) && (
              <button
                className="dt-alert__action"
                onClick={() => setPanel('account')}
                type="button"
              >
                {w.notices.topUp}
              </button>
            )}
            {paymentRequired ? (
              <button
                className="dt-alert__action"
                disabled={recorderStatus !== 'paused'}
                onClick={onPauseToggle}
                type="button"
              >
                {w.runtime.paymentResume}
              </button>
            ) : (
              <button aria-label={w.notices.closeError} onClick={onClearError} type="button">
                <Icon name="close" size={17} />
              </button>
            )}
          </div>
        )}

        {notice && !error && !paymentRequired && (
          <div className="dt-alert dt-alert--info" role="status">
            <span>{notice}</span>
            <button
              className="dt-alert__action"
              onClick={() => { setNotice(null); setPanel('account') }}
              type="button"
            >
              {w.notices.viewAccount}
            </button>
            <button aria-label={w.notices.closeNotice} onClick={() => setNotice(null)} type="button">
              <Icon name="close" size={17} />
            </button>
          </div>
        )}

        {(announcements.length > 0 || transportDiagnostics) && (
        <div className="dt-workspace__notices">
        <AnnouncementBanner announcements={announcements} onDismiss={dismissAnnouncement} />
        {transportDiagnostics && (
          <div
            className={`dt-transport-diag dt-transport-diag--${transportDiagnostics.tone}`}
            role="status"
            title={transportDiagnostics.detail}
          >
            <div className="dt-transport-diag__head">
              <span className="dt-transport-diag__label">{w.notices.transportDebug}</span>
              <span className="dt-transport-diag__summary">{transportDiagnostics.summary}</span>
            </div>
            <dl className="dt-transport-diag__rows">
              {transportDiagnostics.rows.map((row) => (
                <div className="dt-transport-diag__row" key={row.label}>
                  <dt>{row.label}</dt>
                  <dd>
                    <strong>{row.value}</strong>
                    <small>{row.note}</small>
                  </dd>
                </div>
              ))}
            </dl>
            <span className="dt-transport-diag__hint">{transportDiagnostics.detail}</span>
          </div>
        )}
        </div>
        )}

        <main className="dt-stage" ref={stageRef}>
          <div className="dt-feed-toolbar">
            <div>
              <p className="dt-eyebrow">{w.feed.eyebrow}</p>
              <span className="dt-feed-toolbar__count">
                {stats.finalSegments > 0
                  ? w.feed.segments(stats.finalSegments)
                  : w.feed.waiting}
              </span>
            </div>
            <TranscriptFeedModeSwitch
              translationDisabled={!settings.translationEnabled && !learningMode}
              labels={{
                ...m.feed.modes,
                ...(learningMode ? { learn: w.feed.learnMode(settings.learningLevel) } : {}),
              }}
              ariaLabel={m.feed.modeSwitchAria}
              learnTitle={m.feed.learnTitle}
              onChange={setChromeMode}
              value={chromeMode}
            />
          </div>

          <TranscriptFeed
            emptyState={(
              <div className="dt-feed-empty">
                <span className="dt-feed-empty__icon"><Icon name="mic" size={28} /></span>
                <strong>{active ? w.feed.listening : w.feed.readyTitle}</strong>
                <p>
                  {active
                    ? (learningMode ? w.feed.listeningLearn : w.feed.listeningBody)
                    : w.feed.readyBody}
                </p>
                {!active && (
                  <>
                    <button
                      className="dt-button dt-button--primary"
                      onClick={() => { void onStart() }}
                      type="button"
                    >
                      {w.feed.start}
                    </button>
                    <dl className="dt-feed-empty__setup" aria-label={w.feed.setupAria}>
                      <div>
                        <dt>{w.feed.audio}</dt>
                        <dd>{audioSourceLabel(settings.audioSource)}</dd>
                      </div>
                      <div>
                        <dt>{w.feed.language}</dt>
                        <dd>
                          {languageLabel(settings.sourceLanguage)}
                          {settings.translationEnabled
                            ? ` → ${languageLabel(settings.targetLanguage)}`
                            : ` · ${w.feed.originalOnly}`}
                        </dd>
                      </div>
                    </dl>
                    <div className="dt-feed-empty__links">
                      <button
                        className="dt-button dt-button--text"
                        onClick={() => setPanel('settings')}
                        type="button"
                      >
                        {w.feed.changeSettings}
                      </button>
                      <button
                        className="dt-button dt-button--text"
                        onClick={onboarding.openWizard}
                        type="button"
                      >
                        {w.feed.onboarding}
                      </button>
                    </div>
                  </>
                )}
              </div>
            )}
            initialFollow={settings.autoScroll}
            items={feedItems}
            labels={m.feed}
            layoutRevision={feedGeneration}
            learningDomains={settings.learningDomains}
            learningLevel={settings.learningLevel}
            learningMode={learningMode}
            mode={effectiveViewMode}
          />

          {selectionLookup && (
            <div
              className="dt-selection-lookup"
              style={{ left: selectionLookup.x, top: selectionLookup.y }}
            >
              <span className="dt-selection-lookup__text">{selectionLookup.text}</span>
              <button
                onClick={() => {
                  explainTerm(selectionLookup.text)
                  setSelectionLookup(null)
                  window.getSelection()?.removeAllRanges()
                }}
                type="button"
              >
                {w.explain}
              </button>
            </div>
          )}
        </main>

        <RecorderBar
          assistantEnabled={ragEnabled}
          canContinue={Boolean(sessionId)}
          durationLabel={durationLabel}
          onAssistant={() => setPanel('assistant')}
          onContinue={() => { void onContinue() }}
          onMore={() => setPanel('tools')}
          onPauseToggle={onPauseToggle}
          onStart={() => { void onStart() }}
          onStop={() => { void onStop() }}
          status={recorderStatus}
        />
      </section>

      <BatchTranscribePanel
        key={user?.id ?? 'guest'}
        ownerId={user?.id ?? null}
        allowed={!!user && account?.effective_plan?.features?.batch === true}
        open={panel === 'batch'}
        sourceLanguage={settings.sourceLanguage}
        onClose={closePanel}
        onAccount={() => setPanel('account')}
        onHistory={() => { void onRefreshHistory(); setPanel('history') }}
        onSaved={async () => { await Promise.all([onRefreshHistory(), onRefreshAccount()]) }}
      />
      <Sheet
        description={w.sheets.history.description}
        eyebrow={w.sheets.history.eyebrow}
        onClose={closePanel}
        open={panel === 'history'}
        title={w.sheets.history.title}
        wide
      >
        <div className="dt-mobile-panel-nav" aria-label={w.nav.mobileTools}>
          <button
            disabled={!ragEnabled}
            onClick={() => setPanel('assistant')}
            title={ragEnabled ? undefined : w.hints.aiUnavailable}
            type="button"
          >
            <Icon name="sparkles" size={18} />
            <span>{w.nav.assistant}</span>
          </button>
          <button onClick={() => setPanel('insights')} type="button">
            <Icon name="wave" size={18} />
            <span>{w.nav.insights}</span>
          </button>
          <button onClick={() => setPanel('batch')} type="button">
            <Icon name="paperclip" size={18} />
            <span>{m.batch.title}</span><small>Pro</small>
          </button>
          <button
            disabled={studyNavigationDisabled}
            onClick={() => { window.location.assign('/pro/study') }}
            title={studyNavigationTitle}
            type="button"
          >
            <Icon name="map" size={18} />
            <span>{w.nav.study}</span>
          </button>
          <button onClick={() => setPanel('account')} type="button">
            <Icon name="user" size={18} />
            <span>{user ? m.common.account : m.common.login}</span>
          </button>
          {adminNavigation !== 'hidden' && (
            <button
              disabled={adminNavigationDisabled}
              onClick={() => { window.location.assign('/pro/admin') }}
              title={adminNavigationTitle}
              type="button"
            >
              <Icon name="shield" size={18} />
              <span>{w.nav.admin}</span>
            </button>
          )}
        </div>
        <LegacyHistoryNotice
          busy={historyLoading}
          count={legacyHistoryCount}
          disabled={recorderStatus !== 'idle'}
          onMigrate={onMigrateLegacyHistory}
        />
        <HistoryPanel
          activeSessionId={sessionId}
          costs={historyCosts}
          loading={historyLoading}
          opening={historyOpening}
          sessions={historySessions}
          onDelete={onDeleteHistory}
          onLoad={loadHistory}
          onEndSession={onEndHistorySession}
          {...(user ? { onUploadToCloud: onUploadHistorySessionToCloud } : {})}
        />
      </Sheet>

      <Sheet
        description={w.sheets.assistant.description}
        eyebrow={w.sheets.assistant.eyebrow}
        onClose={closePanel}
        open={panel === 'assistant'}
        title={w.sheets.assistant.title}
        wide
      >
        {ragEnabled ? (
          <AssistantPanel
            key={`${user?.id ?? 'anonymous'}:${sessionId}`}
            config={aiConfig}
            onTopUp={() => setPanel('account')}
            ownerId={user?.id ?? null}
            sessionId={sessionId}
            sourceLanguage={sessionSourceLanguage}
            suggestedQuestion={assistantDraft}
            transcriptContext={transcriptContext}
          />
        ) : (
          <div className="dt-empty dt-empty--compact">
            <Icon name="sparkles" size={24} />
            <strong>{w.sheets.assistantOff}</strong>
            <span>{w.sheets.assistantOffBody}</span>
          </div>
        )}
      </Sheet>

      <Sheet
        description={w.sheets.insights.description}
        eyebrow={w.sheets.insights.eyebrow}
        onClose={closePanel}
        open={panel === 'insights'}
        title={w.sheets.insights.title}
        wide
      >
        <InsightsPanel
          assistantEnabled={ragEnabled}
          canViewApiMetrics={user?.role === 'super_admin'}
          durationLabel={durationLabel}
          finalSegments={stats.finalSegments}
          pendingWrites={pendingWrites}
          sessionId={sessionId}
          speakers={stats.speakers}
          topWords={stats.topWords}
          translatedSegments={stats.translatedSegments}
          onExplainTerm={explainTerm}
        />
      </Sheet>

      <Sheet
        description={w.sheets.settings.description}
        eyebrow={w.sheets.settings.eyebrow}
        onClose={closePanel}
        open={panel === 'settings'}
        title={w.sheets.settings.title}
        wide
      >
        <SettingsPanel
          allowUserApiKey={allowUserApiKey}
          authenticated={Boolean(user)}
          onChange={onSettingsChange}
          onReplayOnboarding={replayOnboarding}
          onTrainingOptInChange={props.onTrainingOptInChange}
          ragEnabled={ragEnabled}
          recorderStatus={recorderStatus}
          settings={settings}
          trainingOptIn={user?.training_opt_in ?? null}
          trainingProgram={trainingProgram}
        />
      </Sheet>

      <Sheet
        description={w.sheets.tools.description}
        eyebrow={w.sheets.tools.eyebrow}
        onClose={closePanel}
        open={panel === 'tools'}
        title={w.sheets.tools.title}
      >
        <div className="dt-export-list">
          <ExportButton
            description={w.exports.insights.description}
            icon="wave"
            label={w.exports.insights.label}
            onClick={() => setPanel('insights')}
          />
          <ExportButton
            description={w.exports.audio.description}
            icon="download"
            label={w.exports.audio.label}
            onClick={onDownloadAudio}
          />
          <ExportButton
            description={w.exports.bilingual.description}
            icon="message"
            label={w.exports.bilingual.label}
            onClick={() => onDownloadText('bilingual')}
          />
          <ExportButton
            description={w.exports.original.description}
            icon="archive"
            label={w.exports.original.label}
            onClick={() => onDownloadText('original')}
          />
          <ExportButton
            description={w.exports.translation.description}
            icon="language"
            label={w.exports.translation.label}
            onClick={() => onDownloadText('translation')}
          />
        </div>
      </Sheet>

      <Sheet
        description={user ? w.sheets.account.description : undefined}
        eyebrow={user ? w.sheets.account.eyebrow : w.sheets.account.localEyebrow}
        onClose={closePanel}
        open={panel === 'account'}
        title={user?.name || w.sheets.account.guestTitle}
        wide={Boolean(user)}
      >
        <div className="dt-account-panel">
          <div className="dt-account-panel__identity">
            <span>{user?.name?.trim().slice(0, 1).toUpperCase() || m.common.guestInitial}</span>
            <div>
              <strong>
                {user?.email || w.sheets.account.localOnly}
                {memberActive && <em className="dt-pro-badge">Pro</em>}
              </strong>
              <small>{balanceLabel(balance, account, m.format.localMode)}</small>
            </div>
          </div>
          {user ? (
            <>
              <AccountPanel
                account={account}
                balance={balance}
                open={panel === 'account'}
                paymentsEnabled={paymentsEnabled}
                sessionId={sessionId}
                onRefreshAccount={onRefreshAccount}
              />
              {adminNavigation === 'enabled' && (
                <a className="dt-button dt-button--primary dt-button--wide" href="/pro/admin">
                  {w.account.openAdmin}
                </a>
              )}
              {adminNavigation === 'disabled' && (
                <button
                  className="dt-button dt-button--primary dt-button--wide"
                  disabled
                  title={adminNavigationTitle}
                  type="button"
                >
                  {w.account.openAdminAfter}
                </button>
              )}
              <button
                className="dt-button dt-button--secondary dt-button--wide"
                disabled={transitionBusy}
                onClick={() => { void onLogout().then(closePanel) }}
                type="button"
              >
                {transitionBusy ? w.account.busy : w.account.logout}
              </button>
            </>
          ) : (
            <button
              className="dt-button dt-button--primary dt-button--wide"
              disabled={transitionBusy}
              onClick={() => {
                closePanel()
                onRequestLogin()
              }}
              type="button"
            >
              {w.account.loginSync}
            </button>
          )}
        </div>
      </Sheet>

      {onboarding.phase === 'wizard' && recorderStatus === 'idle' && (
        <OnboardingDialog
          account={account}
          balance={balance}
          paymentsEnabled={paymentsEnabled}
          settings={settings}
          signedIn={Boolean(user)}
          trainingOptIn={user?.training_opt_in ?? null}
          trainingProgram={trainingProgram}
          onFinish={onboarding.finishWizard}
          onOpenAccount={() => {
            onboarding.finishWizard('close')
            setPanel('account')
          }}
          onSettingsChange={onSettingsChange}
          onTrainingOptInChange={props.onTrainingOptInChange}
        />
      )}
      {trainingPromptOpen && (
        <TrainingProgramDialog
          onAnswer={props.onTrainingOptInChange}
          onDismiss={dismissTrainingPrompt}
          program={trainingProgram}
        />
      )}
      {onboarding.phase === 'tour' && (
        <GuideTour onFinish={onboarding.finishTour} steps={tourSteps} />
      )}
    </div>
  )
}

interface LegacyHistoryNoticeProps {
  busy: boolean
  count: number
  disabled: boolean
  onMigrate: () => Promise<void>
}

function LegacyHistoryNotice({
  busy,
  count,
  disabled,
  onMigrate,
}: LegacyHistoryNoticeProps) {
  const { legacy } = useMessages().workspace
  if (count === 0) return null
  return (
    <div className="dt-legacy-history" role="status">
      <span>{legacy.notice(count)}</span>
      <button
        className="dt-button dt-button--secondary"
        disabled={busy || disabled}
        onClick={() => { void onMigrate() }}
        type="button"
      >
        {disabled ? legacy.afterRecording : busy ? legacy.migrating : legacy.migrate}
      </button>
    </div>
  )
}

interface ExportButtonProps {
  description: string
  icon: 'archive' | 'download' | 'language' | 'message' | 'wave'
  label: string
  onClick: () => Promise<void> | void
}

function ExportButton({ description, icon, label, onClick }: ExportButtonProps) {
  const [busy, setBusy] = useState(false)
  return (
    <button
      className="dt-export-item"
      disabled={busy}
      onClick={() => {
        setBusy(true)
        void Promise.resolve(onClick()).finally(() => setBusy(false))
      }}
      type="button"
    >
      <span className="dt-export-item__icon">
        {busy ? <span className="dt-spinner" /> : <Icon name={icon} size={20} />}
      </span>
      <span>
        <strong>{label}</strong>
        <small>{description}</small>
      </span>
      <Icon name="arrow-down" size={17} />
    </button>
  )
}
