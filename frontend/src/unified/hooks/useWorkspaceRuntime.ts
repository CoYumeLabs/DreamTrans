import {
  useCallback,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore
} from 'react'
import {
  parseAccountBalance,
  type SessionCostSummary
} from '../../api'
import {
  BrowserAudioCapture
} from '../../core/audio/BrowserAudioCapture'
import { IndexedDbSessionRepository } from '../../core/session'
import {
  SpeechmaticsPaymentRequiredError,
  SpeechmaticsProxyClient,
  TranscriptStore,
  resolveSpeechmaticsProxyUrl,
  type TranscriptSegment,
  type TranslationSegment
} from '../../core/transcription'
import { EdgeAudioBuffer, RegionalEdgeSocket, type EdgeAuthorization } from '../../core/transcription/RegionalEdge'
import type { SpeechmaticsSocket } from '../../core/transcription/SpeechmaticsProxyClient'
import { messages } from '../../i18n'
import { ensureValidAccessToken, getAccessToken, updateSession as updateCloudSession } from '../../pro/api/auth'
import { websocketAuthProtocols } from '../../utils/websocketAuth'
import type {
  HistoryOpenProgress,
  HistorySession,
} from '../components/HistoryPanel'
import type { RecorderStatus } from '../components/RecorderBar'
import {
  AiTranslateClient,
  type AiTranslateChunk,
  type AiTranslationResult,
} from '../workspace/AiTranslateClient'
import { isInsufficientBalanceError } from '../workspace/billingErrors'
import { CloudTranscriptQueue } from '../workspace/CloudTranscriptQueue'
import { authorizeEdge } from '../workspace/edgeSelection'
import { RagIngestQueue } from '../workspace/RagIngestQueue'
import { ensureSpeechmaticsPreflight } from '../workspace/speechmaticsPreflight'
import {
  TranscriptFeedModel
} from '../workspace/TranscriptFeedModel'
import { ANONYMOUS_TOKEN_SENTINEL, WordCounter, backendURL, defaultSessionTitle, resolveTranslateProxyUrl, type TransportDiagnostics, type UnifiedWorkspaceOptions } from './workspaceModel'

// Own stable session state and transport instances across controller renders.
export function useWorkspaceRuntime({ ragEnabled, settings, user, onBalanceUpdated }: UnifiedWorkspaceOptions) {

  const [paymentRequired, setPaymentRequired] = useState(false)
  const paymentRequiredRef = useRef(false)
  const paymentHandlerRef = useRef<() => void>(() => { })
  const repositoryOwnerRef = useRef<string | null>(user?.id ?? null)
  const [repository] = useState(
    () => new IndexedDbSessionRepository<TranscriptSegment, TranslationSegment>({
      ownerId: () => repositoryOwnerRef.current,
    }),
  )
  const scopedRepositoriesRef = useRef(new Map<
    string,
    IndexedDbSessionRepository<TranscriptSegment, TranslationSegment>
  >())
  const repositoryForOwner = useCallback((ownerId: string | null) => {
    const key = ownerId === null ? 'anonymous' : `account:${ownerId}`
    let scoped = scopedRepositoriesRef.current.get(key)
    if(!scoped) {
      scoped = new IndexedDbSessionRepository<TranscriptSegment, TranslationSegment>({
        ownerId,
      })
      scopedRepositoriesRef.current.set(key, scoped)
    }
    return scoped
  }, [])
  const [transcriptStore] = useState(() => new TranscriptStore())
  const [feedModel] = useState(() => new TranscriptFeedModel({
    sourceLanguage: settings.sourceLanguage,
    targetLanguage: settings.targetLanguage,
    translationEnabled: settings.translationEnabled,
  }))
  const sessionAuthRequiredRef = useRef(false)
  const edgeAuthorizationRef = useRef<EdgeAuthorization | null>(null)
  const [edgeBuffer] = useState(() => new EdgeAudioBuffer())
  const [client] = useState(() => new SpeechmaticsProxyClient({
    beforeReconnect: async () => {
      try {
        await ensureSpeechmaticsPreflight()
      } catch(reason) {
        if(isInsufficientBalanceError(reason)) {
          throw new SpeechmaticsPaymentRequiredError(messages().workspace.runtime.insufficientBalance)
        }
        throw reason
      }
    },
    // The session id lets the backend tie this live stream to the session so
    // it can be ended remotely from another device or the admin console.
    url: () => edgeAuthorizationRef.current ? edgeAuthorizationRef.current.endpoint.replace(/^https:/, 'wss:') + '/ws/edge' : resolveSpeechmaticsProxyUrl(backendURL, currentSessionRef.current),
    socketFactory: (url, protocols) => edgeAuthorizationRef.current ? new RegionalEdgeSocket(edgeAuthorizationRef.current, edgeBuffer) : new WebSocket(url, [...protocols]) as unknown as SpeechmaticsSocket,
    tokenProvider: async (sampleRate) => {
      const token = getAccessToken()
      const sessionId = currentSessionRef.current
      if(edgeAuthorizationRef.current?.grant.session_id !== sessionId) edgeAuthorizationRef.current = null
      if(token && sessionId) {
        // Retain the transport through failed retries and changes to routing.
        // Only successful authorization replaces the current grant.
        const continuingEdge = edgeAuthorizationRef.current?.grant.session_id === sessionId
        edgeAuthorizationRef.current = await authorizeEdge(sessionId, sampleRate, continuingEdge)
        if(edgeAuthorizationRef.current) return edgeAuthorizationRef.current.token
      }
      if(sessionAuthRequiredRef.current && !token) {
        throw new Error(messages().workspace.runtime.authExpired)
      }
      return token
        ? ensureValidAccessToken(90)
        : ANONYMOUS_TOKEN_SENTINEL
    },
    protocolFactory: (token) => (
      token === ANONYMOUS_TOKEN_SENTINEL ? [] : websocketAuthProtocols(token)
    ),
    store: transcriptStore,
    resetStoreOnStart: false,
    partialUpdateIntervalMs: 50,
    // Keep trying through a short mobile hand-off or Wi-Fi outage. The
    // byte-bounded queue below caps memory while preserving roughly 30 seconds
    // of speech instead of dropping audio after the previous five seconds.
    reconnect: { maxAttempts: 8 },
    audio: {
      sampleRate: 48_000,
      frameDurationMs: 40,
      maxQueuedAudioSeconds: 30,
    },
  }))
  const aiTranslationHandlerRef = useRef<
    ((chunk: AiTranslateChunk, result: AiTranslationResult) => void) | null
  >(null)
  // Running realtime cost of the current session. The server row sums are the
  // base; per-charge deltas pushed over both live sockets accumulate on top,
  // and every server refresh resets the delta so nothing counts twice.
  const [sessionCostBase, setSessionCostBase] = useState<SessionCostSummary | null>(null)
  const [sessionLiveCostUsd, setSessionLiveCostUsd] = useState(0)
  const addSessionLiveCost = useCallback((costUsd: number) => {
    if(!Number.isFinite(costUsd) || costUsd <= 0) return
    setSessionLiveCostUsd((current) => current + costUsd)
  }, [])
  const [aiTranslator] = useState(() => new AiTranslateClient({
    url: () => resolveTranslateProxyUrl(backendURL),
    tokenProvider: async () => {
      const token = getAccessToken()
      return token ? ensureValidAccessToken(90) : ANONYMOUS_TOKEN_SENTINEL
    },
    protocolFactory: (token) => (
      token === ANONYMOUS_TOKEN_SENTINEL ? [] : websocketAuthProtocols(token)
    ),
    onTranslation: (chunk, result) => {
      aiTranslationHandlerRef.current?.(chunk, result)
    },
    onChunkError: (chunk, message) => {
      feedModel.markTranslationError(chunk.segmentIds, message)
    },
    onError: (message) => setError(message),
    onPaymentRequired: () => paymentHandlerRef.current(),
    onRecovered: () => setError(null),
    onBalance: (event) => {
      balanceCallbackRef.current?.(parseAccountBalance(event.balance))
      addSessionLiveCost(event.costUsd)
    },
  }))
  const [localPending, setLocalPending] = useState(0)
  const [cloudPending, setCloudPending] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [recorderStatus, setRecorderStatusState] = useState<RecorderStatus>('idle')
  const [sessionId, setSessionId] = useState('')
  const [sessionSourceLanguage, setSessionSourceLanguage] = useState(
    settings.sourceLanguage,
  )
  const [title, setTitle] = useState(() => defaultSessionTitle())
  const titleRef = useRef(title)
  titleRef.current = title
  const [titleGenerating, setTitleGenerating] = useState(false)
  const titleGenerationRef = useRef(0)
  const autoTitleRef = useRef<((sessionId: string) => void) | null>(null)
  const [elapsedSeconds, setElapsedSeconds] = useState(0)
  const [historySessions, setHistorySessions] = useState<HistorySession[]>([])
  const [historyLoading, setHistoryLoading] = useState(false)
  const [historyOpening, setHistoryOpening] = useState<HistoryOpenProgress | null>(null)
  const historySessionsRef = useRef<HistorySession[]>([])
  historySessionsRef.current = historySessions
  const [legacyHistoryCount, setLegacyHistoryCount] = useState(0)
  const [topWords, setTopWords] = useState<Array<{ word: string; count: number }>>([])
  const [transportDiagnostics, setTransportDiagnostics] = useState<TransportDiagnostics | null>(
    null,
  )
  const feedSnapshot = useSyncExternalStore(feedModel.subscribe, feedModel.getSnapshot)
  const transcriptContext = useMemo(
    () => feedSnapshot.items
      .filter((item) => item.original?.status === 'final' && item.original.text?.trim())
      .map((item) => {
        const start = item.startTime === undefined ? '' : `[${item.startTime.toFixed(1)}s] `
        return `${start}${item.speaker}: ${item.original?.text?.trim() ?? ''}`
      })
      .join('\n'),
    [feedSnapshot],
  )
  const transcriptSnapshot = useSyncExternalStore(
    transcriptStore.subscribe,
    transcriptStore.getSnapshot,
  )
  const clientSnapshot = useSyncExternalStore(client.subscribe, client.getSnapshot)

  const settingsRef = useRef(settings)
  const ragEnabledRef = useRef(ragEnabled)
  const userRef = useRef(user)
  const balanceCallbackRef = useRef(onBalanceUpdated)
  const statusRef = useRef<RecorderStatus>('idle')
  const currentSessionRef = useRef('')
  const currentAudioMimeTypeRef = useRef('audio/webm')
  const currentLocationRef = useRef<'cloud' | 'local'>('local')
  const cloudSessionRef = useRef<string | null>(null)
  const cloudSessionVerifiedRef = useRef(false)
  /** Translation engine locked in for the active session ('' when none). */
  const sessionTranslationEngineRef = useRef<'' | 'ai' | 'speechmatics'>('')
  const sessionTargetLanguageRef = useRef(settings.targetLanguage)
  const captureRef = useRef<BrowserAudioCapture | null>(null)
  const localAudioHealthyRef = useRef(false)
  const localWriteChainRef = useRef<Promise<void>>(Promise.resolve())
  const elapsedAccumulatedRef = useRef(0)
  const elapsedRunStartedRef = useRef<number | null>(null)
  const orphanTranslationsRef = useRef(new Map<string, TranslationSegment>())
  const wordCounterRef = useRef(new WordCounter())
  const historyRequestRef = useRef(0)
  const historyLoadRequestRef = useRef(0)
  const destroyTimerRef = useRef<number | null>(null)
  const lifecycleEpochRef = useRef(0)
  const startPromiseRef = useRef<Promise<void> | null>(null)
  const stopPromiseRef = useRef<Promise<void> | null>(null)
  const ownerGenerationRef = useRef(0)
  const sessionLockKeyRef = useRef('')
  const sessionLockReleaseRef = useRef<(() => void) | null>(null)
  const cloudMetadataSyncRef = useRef(new Map<string, {
    appliedRevision: number
    desired: Parameters<typeof updateCloudSession>[1]
    operation: Promise<void> | null
    ownerId: string
    revision: number
  }>())
  const onlineRecoveryRef = useRef<Promise<void> | null>(null)
  const lastCloudDurationSyncRef = useRef({ id: '', seconds: 0 })
  const renderedOwnerIdRef = useRef<string | null>(user?.id ?? null)
  const renderedOwnerId = user?.id ?? null
  if(renderedOwnerIdRef.current !== renderedOwnerId) {
    renderedOwnerIdRef.current = renderedOwnerId
    ownerGenerationRef.current += 1
  }

  const [cloudQueue] = useState(() => new CloudTranscriptQueue({
    maxPending: 100_000,
    onPendingChange: setCloudPending,
    onError: (reason) => setError(messages().workspace.runtime.cloudSyncFailed(reason.message)),
    onBatchSaved: async (batch) => {
      await repository.acknowledgeCloudTranscriptOutbox(
        batch.entries.flatMap((entry) => (
          entry.durableVersion === undefined
            ? []
            : [{
              ownerId: batch.ownerId,
              sessionId: batch.sessionId,
              clientSegmentId: entry.clientSegmentId,
              updatedAt: entry.durableVersion,
            }]
        )),
      )
    },
  }))
  const [ragQueue] = useState(() => new RagIngestQueue())

  settingsRef.current = settings
  ragEnabledRef.current = ragEnabled
  userRef.current = user
  balanceCallbackRef.current = onBalanceUpdated
  return {
    paymentRequired,
    setPaymentRequired,
    paymentRequiredRef,
    paymentHandlerRef,
    repositoryOwnerRef,
    repository,
    scopedRepositoriesRef,
    repositoryForOwner,
    transcriptStore,
    feedModel,
    sessionAuthRequiredRef,
    edgeAuthorizationRef,
    edgeBuffer,
    client,
    aiTranslationHandlerRef,
    sessionCostBase,
    setSessionCostBase,
    sessionLiveCostUsd,
    setSessionLiveCostUsd,
    addSessionLiveCost,
    aiTranslator,
    localPending,
    setLocalPending,
    cloudPending,
    setCloudPending,
    error,
    setError,
    recorderStatus,
    setRecorderStatusState,
    sessionId,
    setSessionId,
    sessionSourceLanguage,
    setSessionSourceLanguage,
    title,
    setTitle,
    titleRef,
    titleGenerating,
    setTitleGenerating,
    titleGenerationRef,
    autoTitleRef,
    elapsedSeconds,
    setElapsedSeconds,
    historySessions,
    setHistorySessions,
    historyLoading,
    setHistoryLoading,
    historyOpening,
    setHistoryOpening,
    historySessionsRef,
    legacyHistoryCount,
    setLegacyHistoryCount,
    topWords,
    setTopWords,
    transportDiagnostics,
    setTransportDiagnostics,
    feedSnapshot,
    transcriptContext,
    transcriptSnapshot,
    clientSnapshot,
    settingsRef,
    ragEnabledRef,
    userRef,
    balanceCallbackRef,
    statusRef,
    currentSessionRef,
    currentAudioMimeTypeRef,
    currentLocationRef,
    cloudSessionRef,
    cloudSessionVerifiedRef,
    sessionTranslationEngineRef,
    sessionTargetLanguageRef,
    captureRef,
    localAudioHealthyRef,
    localWriteChainRef,
    elapsedAccumulatedRef,
    elapsedRunStartedRef,
    orphanTranslationsRef,
    wordCounterRef,
    historyRequestRef,
    historyLoadRequestRef,
    destroyTimerRef,
    lifecycleEpochRef,
    startPromiseRef,
    stopPromiseRef,
    ownerGenerationRef,
    sessionLockKeyRef,
    sessionLockReleaseRef,
    cloudMetadataSyncRef,
    onlineRecoveryRef,
    lastCloudDurationSyncRef,
    renderedOwnerIdRef,
    renderedOwnerId,
    cloudQueue,
    ragQueue,
    ragEnabled,
    settings,
    user,
    onBalanceUpdated,
  }
}
export type WorkspaceRuntime = ReturnType<typeof useWorkspaceRuntime>
