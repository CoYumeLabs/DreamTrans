import { messages } from '../../i18n'
import { useWorkspaceHistory } from './useWorkspaceHistory'
import { useWorkspaceHistoryIndex } from './useWorkspaceHistoryIndex'
import { useWorkspacePersistence } from './useWorkspacePersistence'
import { useWorkspaceRecording } from './useWorkspaceRecording'
import { useWorkspaceRuntime } from './useWorkspaceRuntime'
import { useWorkspaceSync } from './useWorkspaceSync'
import { defaultSessionTitle, formatDuration, type UnifiedWorkspaceOptions, type UnifiedWorkspaceState } from './workspaceModel'

export type { SessionCostView, TransportDiagRow, TransportDiagnostics, UnifiedWorkspaceOptions, UnifiedWorkspaceState } from './workspaceModel'

export function useUnifiedWorkspace(options: UnifiedWorkspaceOptions): UnifiedWorkspaceState {
  const runtime = useWorkspaceRuntime(options)
  const persistence = useWorkspacePersistence({ ...runtime })
  const historyindex = useWorkspaceHistoryIndex({ ...runtime, ...persistence })
  const recording = useWorkspaceRecording({ ...runtime, ...persistence, ...historyindex })
  const history = useWorkspaceHistory({ ...runtime, ...persistence, ...historyindex, ...recording })
  const sync = useWorkspaceSync({ ...runtime, ...persistence, ...historyindex, ...recording, ...history })
  const { paymentRequired, recorderStatus, localAudioHealthyRef, clientSnapshot, user, repositoryOwnerRef, elapsedSeconds, error, feedSnapshot, historyLoading, historyOpening, historySessions, legacyHistoryCount, localPending, cloudPending, sessionId, settings, sessionSourceLanguage, transcriptSnapshot, topWords, title, transportDiagnostics, transcriptContext, setError, titleGenerating } = runtime
  const { migrateLegacyHistory, refreshHistory } = historyindex
  const { pauseToggle, continueSession, start, stop } = recording
  const { deleteHistory, endHistorySession, uploadHistorySessionToCloud, downloadAudio, downloadText, loadHistory, updateTitle, generateTitle } = history
  const { sessionCost } = sync


  const connectionLabel = paymentRequired
    ? messages().workspace.runtime.paymentPaused
    : recorderStatus === 'error'
      ? localAudioHealthyRef.current
        ? messages().workspace.runtime.disconnectedRecording
        : messages().workspace.runtime.connectionFailed
      : clientSnapshot.status === 'reconnecting'
        ? messages().workspace.runtime.reconnecting(clientSnapshot.reconnectAttempt, clientSnapshot.maxReconnectAttempts)
        : clientSnapshot.connected
          ? messages().workspace.runtime.connected
          : user
            ? messages().workspace.runtime.cloudReady
            : messages().workspace.runtime.localReady
  const ownerTransitioning = repositoryOwnerRef.current !== (user?.id ?? null)

  return {
    paymentRequired,
    connectionLabel,
    durationLabel: formatDuration(ownerTransitioning ? 0 : elapsedSeconds),
    error,
    feedGeneration: feedSnapshot.generation,
    feedItems: ownerTransitioning ? [] : feedSnapshot.items,
    historyLoading: ownerTransitioning ? true : historyLoading,
    historyOpening: ownerTransitioning ? null : historyOpening,
    historySessions: ownerTransitioning ? [] : historySessions,
    legacyHistoryCount: ownerTransitioning ? 0 : legacyHistoryCount,
    pendingWrites: localPending + cloudPending,
    recorderStatus,
    sessionCost: ownerTransitioning ? null : sessionCost,
    sessionId: ownerTransitioning ? '' : sessionId,
    sessionSourceLanguage: ownerTransitioning
      ? settings.sourceLanguage
      : sessionSourceLanguage,
    stats: {
      finalSegments: ownerTransitioning ? 0 : transcriptSnapshot.stats.segmentCount,
      // Chunked AI translations cover several segments per record; the feed
      // model counts covered segments so the progress ratio stays truthful.
      translatedSegments: ownerTransitioning ? 0 : feedSnapshot.translatedSegmentCount,
      speakers: ownerTransitioning ? 0 : transcriptSnapshot.stats.speakerCount,
      topWords: ownerTransitioning ? [] : topWords,
    },
    title: ownerTransitioning ? defaultSessionTitle() : title,
    transportDiagnostics: ownerTransitioning ? null : transportDiagnostics,
    transcriptContext: ownerTransitioning ? '' : transcriptContext,
    clearError: () => setError(null),
    deleteHistory,
    endHistorySession,
    uploadHistorySessionToCloud,
    downloadAudio,
    downloadText,
    loadHistory,
    migrateLegacyHistory,
    pauseToggle,
    refreshHistory,
    continueSession,
    start,
    stop,
    updateTitle,
    titleGenerating: ownerTransitioning ? false : titleGenerating,
    generateTitle,
  }
}
