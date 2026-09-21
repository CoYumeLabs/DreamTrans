import {
  useCallback
} from 'react'
import { migrateLegacySessionStorage } from '../../db'
import { messages } from '../../i18n'
import {
  listSessions as listCloudSessions
} from '../../pro/api/auth'
import { lexReplace } from '../../utils/lexicon'
import type {
  HistorySession
} from '../components/HistoryPanel'
import {
  type StoredSessionRecords
} from '../workspace/mergeSessionRecords'
import type { WorkspacePersistence } from './useWorkspacePersistence'
import type { WorkspaceRuntime } from './useWorkspaceRuntime'

type Dependencies = Pick<
  WorkspaceRuntime & WorkspacePersistence,
  "ownerGenerationRef"
  | "repositoryOwnerRef"
  | "ownerScopeIsCurrent"
  | "historyRequestRef"
  | "setHistoryLoading"
  | "repository"
  | "setLegacyHistoryCount"
  | "setHistorySessions"
  | "userRef"
  | "syncCloudMetadata"
  | "setError"
  | "statusRef"
  | "transcriptStore"
  | "feedModel"
  | "wordCounterRef"
  | "setTopWords"
  | "orphanTranslationsRef"
  | "elapsedAccumulatedRef"
  | "elapsedRunStartedRef"
  | "setElapsedSeconds"
>

// Merge local/cloud history without blocking on pending transcript uploads.
export function useWorkspaceHistoryIndex(scope: Dependencies) {
  const {
    ownerGenerationRef,
    repositoryOwnerRef,
    ownerScopeIsCurrent,
    historyRequestRef,
    setHistoryLoading,
    repository,
    setLegacyHistoryCount,
    setHistorySessions,
    userRef,
    syncCloudMetadata,
    setError,
    statusRef,
    transcriptStore,
    feedModel,
    wordCounterRef,
    setTopWords,
    orphanTranslationsRef,
    elapsedAccumulatedRef,
    elapsedRunStartedRef,
    setElapsedSeconds,
  } = scope


  const refreshHistory = useCallback(async () => {
    const ownerGeneration = ownerGenerationRef.current
    const ownerId = repositoryOwnerRef.current
    if(!ownerScopeIsCurrent(ownerGeneration, ownerId)) return
    const request = ++historyRequestRef.current
    const isCurrent = () => (
      request === historyRequestRef.current
      && ownerScopeIsCurrent(ownerGeneration, ownerId)
    )
    // HistoryPanel keeps an existing list visible while loading; only blanks
    // the panel when there is nothing to show yet.
    setHistoryLoading(true)
    try {
      const [localPage, legacyCount] = await Promise.all([
        repository.listSessions({ limit: 60 }),
        repository.countLegacySessions(),
      ])
      if(!isCurrent()) return
      const merged = new Map<string, HistorySession>()
      const localById = new Map(
        localPage.items.map((metadata) => [metadata.id, metadata]),
      )
      for(const metadata of localPage.items) {
        merged.set(metadata.id, {
          id: metadata.id,
          title: metadata.title || messages().common.untitledSession,
          createdAt: metadata.createdAt,
          durationSeconds: Math.round((metadata.durationMs ?? 0) / 1_000),
          status: metadata.status,
          location: metadata.origin,
        })
      }

      // Paint local cache immediately so the sidebar is usable while the cloud
      // list is still in flight.
      setLegacyHistoryCount(legacyCount)
      setHistorySessions(
        [...merged.values()]
          .sort((left, right) => right.createdAt - left.createdAt)
          .slice(0, 60),
      )
      if(!userRef.current) setHistoryLoading(false)

      if(userRef.current) {
        try {
          const cloud = await listCloudSessions(1, 60)
          if(!isCurrent()) return
          for(const session of cloud.sessions) {
            const local = localById.get(session.id)
            const cloudUpdatedAt = Date.parse(session.updated_at) || 0
            const localWins = Boolean(local && local.updatedAt > cloudUpdatedAt)
            const status = localWins
              ? local?.status ?? 'active'
              : session.status === 'active' || session.status === 'paused'
                ? 'active'
                : 'completed'
            merged.set(session.id, {
              id: session.id,
              title: localWins
                ? local?.title || session.title || messages().common.untitledSession
                : session.title || messages().common.untitledSession,
              createdAt: Date.parse(session.created_at) || Date.now(),
              durationSeconds: Math.max(
                session.duration_seconds || 0,
                (local?.durationMs ?? 0) / 1_000,
              ),
              status,
              location: 'cloud',
            })
            if(localWins && local) {
              const desiredStatus = local.status === 'active'
                ? 'active'
                : 'completed'
              const desiredDuration = Math.round((local.durationMs ?? 0) / 1_000)
              if(
                (local.title && local.title !== session.title)
                || desiredStatus !== session.status
                || desiredDuration > (session.duration_seconds || 0)
              ) {
                void syncCloudMetadata(session.id, {
                  ...(local.title ? { title: local.title } : {}),
                  status: desiredStatus,
                  duration_seconds: Math.max(
                    desiredDuration,
                    session.duration_seconds || 0,
                  ),
                }).catch(() => undefined)
              }
            }
          }
        } catch(reason) {
          if(isCurrent()) {
            setError(messages().workspace.runtime.cloudHistoryFailed(reason instanceof Error ? reason.message : String(reason)))
          }
        }
      }
      if(isCurrent()) {
        setHistorySessions(
          [...merged.values()]
            .sort((left, right) => right.createdAt - left.createdAt)
            .slice(0, 60),
        )
      }
    } catch(reason) {
      if(isCurrent()) {
        setError(messages().workspace.runtime.historyFailed(reason instanceof Error ? reason.message : String(reason)))
      }
    } finally {
      if(isCurrent()) setHistoryLoading(false)
    }
  }, [historyRequestRef, ownerGenerationRef, ownerScopeIsCurrent, repository, repositoryOwnerRef, setError, setHistoryLoading, setHistorySessions, setLegacyHistoryCount, syncCloudMetadata, userRef])

  const migrateLegacyHistory = useCallback(async () => {
    if(statusRef.current !== 'idle') {
      setError(messages().workspace.runtime.stopBeforeMigrate)
      return
    }
    setHistoryLoading(true)
    setError(null)
    try {
      await migrateLegacySessionStorage()
      setLegacyHistoryCount(0)
      await refreshHistory()
    } catch(reason) {
      setError(
        messages().workspace.runtime.migrationFailed(reason instanceof Error ? reason.message : String(reason)),
      )
    } finally {
      setHistoryLoading(false)
    }
  }, [refreshHistory, setError, setHistoryLoading, setLegacyHistoryCount, statusRef])

  const applyLoadedRecords = useCallback((
    records: StoredSessionRecords,
    loadedDurationSeconds: number,
    loadedSessionId = '',
  ) => {
    transcriptStore.reset()
    transcriptStore.batch(() => {
      for(const segment of records.segments) transcriptStore.appendTranscript(segment)
      for(const translation of records.translations) {
        try {
          transcriptStore.appendTranslation(translation)
        } catch {
          transcriptStore.appendTranslation({ ...translation, segmentId: null })
        }
      }
    })
    feedModel.hydrate(records.segments, records.translations)
    wordCounterRef.current.reset()
    for(const segment of records.segments) wordCounterRef.current.add(segment.text)
    setTopWords(wordCounterRef.current.getTop())
    orphanTranslationsRef.current.clear()
    if(loadedSessionId) {
      lexReplace(
        loadedSessionId,
        records.segments.map((segment) => segment.text),
      )
    }
    elapsedAccumulatedRef.current = loadedDurationSeconds * 1_000
    elapsedRunStartedRef.current = null
    setElapsedSeconds(Math.floor(loadedDurationSeconds))
  }, [elapsedAccumulatedRef, elapsedRunStartedRef, feedModel, orphanTranslationsRef, setElapsedSeconds, setTopWords, transcriptStore, wordCounterRef])
  return {
    refreshHistory,
    migrateLegacyHistory,
    applyLoadedRecords,
  }
}
export type WorkspaceHistoryIndex = ReturnType<typeof useWorkspaceHistoryIndex>
