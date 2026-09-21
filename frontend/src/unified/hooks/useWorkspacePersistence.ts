import {
  useCallback
} from 'react'
import { IndexedDbSessionRepository } from '../../core/session'
import {
  type TranscriptSegment,
  type TranslationSegment
} from '../../core/transcription'
import { messages } from '../../i18n'
import {
  ApiRequestError,
  createSession as createCloudSession,
  getSession as getCloudSession,
  updateSession as updateCloudSession,
  type TranscriptInput
} from '../../pro/api/auth'
import type { RecorderStatus } from '../components/RecorderBar'
import type { WorkspaceRuntime } from './useWorkspaceRuntime'
import { isTranscriptInput, updateCloudSessionWithRetry, withOperationTimeout } from './workspaceModel'

type Dependencies = Pick<
  WorkspaceRuntime,
  "sessionLockReleaseRef"
  | "sessionLockKeyRef"
  | "repository"
  | "userRef"
  | "cloudMetadataSyncRef"
  | "ownerGenerationRef"
  | "repositoryOwnerRef"
  | "statusRef"
  | "setRecorderStatusState"
  | "elapsedRunStartedRef"
  | "elapsedAccumulatedRef"
  | "setElapsedSeconds"
  | "repositoryForOwner"
  | "setLocalPending"
  | "localWriteChainRef"
  | "setError"
  | "currentSessionRef"
  | "setHistorySessions"
  | "cloudSessionRef"
  | "lastCloudDurationSyncRef"
  | "cloudSessionVerifiedRef"
  | "cloudQueue"
  | "settingsRef"
>

// Serialize local writes and retain durable cloud intents across owner changes.
export function useWorkspacePersistence(scope: Dependencies) {
  const {
    sessionLockReleaseRef,
    sessionLockKeyRef,
    repository,
    userRef,
    cloudMetadataSyncRef,
    ownerGenerationRef,
    repositoryOwnerRef,
    statusRef,
    setRecorderStatusState,
    elapsedRunStartedRef,
    elapsedAccumulatedRef,
    setElapsedSeconds,
    repositoryForOwner,
    setLocalPending,
    localWriteChainRef,
    setError,
    currentSessionRef,
    setHistorySessions,
    cloudSessionRef,
    lastCloudDurationSyncRef,
    cloudSessionVerifiedRef,
    cloudQueue,
    settingsRef,
  } = scope


  const releaseSessionLock = useCallback(() => {
    sessionLockReleaseRef.current?.()
    sessionLockReleaseRef.current = null
    sessionLockKeyRef.current = ''
  }, [sessionLockKeyRef, sessionLockReleaseRef])

  const acquireSessionLock = useCallback(async (
    activeSessionId: string,
    ownerId: string | null,
  ): Promise<boolean> => {
    const lockManager = navigator.locks
    if(!lockManager) return true
    const lockKey = `dreamtrans:${ownerId ?? 'anonymous'}:${activeSessionId}`
    if(
      sessionLockKeyRef.current === lockKey
      && sessionLockReleaseRef.current
    ) {
      return true
    }
    releaseSessionLock()
    let releaseGate!: () => void
    const gate = new Promise<void>((resolve) => {
      releaseGate = resolve
    })
    const granted = new Promise<boolean>((resolve, reject) => {
      void lockManager.request(
        lockKey,
        { mode: 'exclusive', ifAvailable: true },
        async (lock) => {
          if(!lock) {
            resolve(false)
            return
          }
          sessionLockKeyRef.current = lockKey
          sessionLockReleaseRef.current = releaseGate
          resolve(true)
          await gate
        },
      ).catch(reject)
    })
    return granted
  }, [releaseSessionLock, sessionLockKeyRef, sessionLockReleaseRef])

  const syncCloudMetadata = useCallback((
    activeSessionId: string,
    data: Parameters<typeof updateCloudSession>[1],
  ): Promise<void> => {
    const ownerId = repository.currentOwnerId()
    if(!ownerId || userRef.current?.id !== ownerId) {
      return Promise.reject(new Error(messages().workspace.runtime.ownerChanged))
    }
    const queueKey = `${ownerId}\u0000${activeSessionId}`
    let state = cloudMetadataSyncRef.current.get(queueKey)
    if(!state) {
      state = {
        appliedRevision: 0,
        desired: {},
        operation: null,
        ownerId,
        revision: 0,
      }
      cloudMetadataSyncRef.current.set(queueKey, state)
    }
    state.desired = { ...state.desired, ...data }
    state.revision += 1
    if(state.operation) return state.operation

    const target = state
    const operation = (async () => {
      while(target.appliedRevision < target.revision) {
        if(userRef.current?.id !== target.ownerId) {
          throw new Error(messages().workspace.runtime.ownerChanged)
        }
        const revision = target.revision
        const desired = { ...target.desired }
        await updateCloudSessionWithRetry(
          activeSessionId,
          desired,
          3,
          () => (
            repository.currentOwnerId() === target.ownerId
            && userRef.current?.id === target.ownerId
          ),
        )
        target.appliedRevision = revision
      }
    })()
      .finally(() => {
        if(target.operation === operation) target.operation = null
        if(
          target.appliedRevision >= target.revision
          && cloudMetadataSyncRef.current.get(queueKey) === target
        ) {
          cloudMetadataSyncRef.current.delete(queueKey)
        }
      })
    target.operation = operation
    return operation
  }, [cloudMetadataSyncRef, repository, userRef])

  const ownerScopeIsCurrent = useCallback((
    generation: number,
    ownerId: string | null,
  ) => (
    ownerGenerationRef.current === generation
    && repositoryOwnerRef.current === ownerId
    && (userRef.current?.id ?? null) === ownerId
  ), [ownerGenerationRef, repositoryOwnerRef, userRef])

  const setRecorderStatus = useCallback((status: RecorderStatus) => {
    statusRef.current = status
    setRecorderStatusState(status)
  }, [setRecorderStatusState, statusRef])

  const updateElapsed = useCallback(() => {
    const runningSince = elapsedRunStartedRef.current
    const elapsed = elapsedAccumulatedRef.current + (
      runningSince === null ? 0 : performance.now() - runningSince
    )
    setElapsedSeconds(Math.floor(elapsed / 1_000))
  }, [elapsedAccumulatedRef, elapsedRunStartedRef, setElapsedSeconds])

  const enqueueLocal = useCallback((
    operation: (
      scopedRepository: IndexedDbSessionRepository<
        TranscriptSegment,
        TranslationSegment
      >,
    ) => Promise<unknown>,
    propagateError = false,
  ): Promise<void> => {
    // Capture the owner before this write waits behind earlier IndexedDB work.
    // Even if a 15s UI timeout lets logout continue, the underlying operation
    // can only ever finish inside the account that produced it.
    const scopedRepository = repositoryForOwner(repository.currentOwnerId())
    setLocalPending((count) => count + 1)
    const operationResult = localWriteChainRef.current.then(async () => {
      await withOperationTimeout(
        Promise.resolve().then(() => operation(scopedRepository)),
        15_000,
        messages().workspace.runtime.localTimeout,
      )
    })
    localWriteChainRef.current = operationResult
      .catch((reason: unknown) => {
        setError(messages().workspace.runtime.localSaveFailed(reason instanceof Error ? reason.message : String(reason)))
      })
      .finally(() => setLocalPending((count) => Math.max(0, count - 1)))
    return propagateError ? operationResult : localWriteChainRef.current
  }, [localWriteChainRef, repository, repositoryForOwner, setError, setLocalPending])

  /**
   * Persist the running duration mid-session. Until now durationMs was only
   * written by the final completeSession() in stop(), so a closed tab, crash
   * or killed mobile page left history showing the previous stop's value (or
   * 0:00) and Continue restarted its timer from that stale base. Checkpoints
   * ride the serial local write chain, and stop() awaits that chain before
   * its final write, so a late checkpoint can never overwrite the real total.
   */
  const checkpointDuration = useCallback((options: { cloud?: boolean } = {}) => {
    const id = currentSessionRef.current
    if(!id) return
    const runningSince = elapsedRunStartedRef.current
    const durationMs = Math.round(elapsedAccumulatedRef.current + (
      runningSince === null ? 0 : performance.now() - runningSince
    ))
    if(durationMs <= 0) return
    // touch:false — a checkpoint is not new content and must not reorder
    // history or advance the revision the cloud merge compares against.
    void enqueueLocal((scopedRepository) => (
      scopedRepository.updateSessionMetadata(id, { durationMs }, { touch: false })
    ))
    const durationSeconds = Math.round(durationMs / 1_000)
    setHistorySessions((sessions) => {
      const index = sessions.findIndex((session) => session.id === id)
      if(index < 0 || sessions[index].durationSeconds >= durationSeconds) return sessions
      const next = sessions.slice()
      next[index] = { ...sessions[index], durationSeconds }
      return next
    })
    if(!options.cloud || cloudSessionRef.current !== id || !userRef.current) return
    const last = lastCloudDurationSyncRef.current
    if(last.id === id && last.seconds >= durationSeconds) return
    lastCloudDurationSyncRef.current = { id, seconds: durationSeconds }
    void syncCloudMetadata(id, { duration_seconds: durationSeconds }).catch(() => undefined)
  }, [cloudSessionRef, currentSessionRef, elapsedAccumulatedRef, elapsedRunStartedRef, enqueueLocal, lastCloudDurationSyncRef, setHistorySessions, syncCloudMetadata, userRef])

  const queueCloudInput = useCallback((
    cloudSessionId: string,
    input: TranscriptInput,
  ) => {
    // During logout/account transition the live session is stopped before the
    // repository owner changes. Persist its final records under that captured
    // owner even though the React user may already be null; CloudTranscriptQueue
    // independently refuses to send them with another account's token.
    const ownerId = repository.currentOwnerId()
    if(!ownerId) return
    void enqueueLocal(async (scopedRepository) => {
      const outbox = await scopedRepository.upsertCloudTranscriptOutbox(
        cloudSessionId,
        input.client_segment_id,
        input,
      )
      if(
        currentSessionRef.current === cloudSessionId
        && !cloudSessionVerifiedRef.current
      ) {
        return
      }
      cloudQueue.restore([{
        ownerId: outbox.ownerId,
        sessionId: outbox.sessionId,
        input,
        durableVersion: outbox.updatedAt,
      }])
    })
  }, [cloudQueue, cloudSessionVerifiedRef, currentSessionRef, enqueueLocal, repository])

  const restoreCloudSessionOutbox = useCallback(async (
    cloudSessionId: string,
    expectedOwnerId = repository.currentOwnerId(),
  ) => {
    if(!expectedOwnerId || userRef.current?.id !== expectedOwnerId) return
    const scopedRepository = repositoryForOwner(expectedOwnerId)
    let after:
      | { createdAt: number; clientSegmentId: string }
      | undefined
    do {
      if(userRef.current?.id !== expectedOwnerId) return
      const page = await scopedRepository.getCloudTranscriptOutboxPage<TranscriptInput>(
        cloudSessionId,
        {
          limit: 500,
          ...(after ? { after } : {}),
        },
      )
      cloudQueue.restore(page.items.flatMap((record) => (
        isTranscriptInput(record.payload)
          ? [{
            ownerId: record.ownerId,
            sessionId: record.sessionId,
            input: record.payload,
            durableVersion: record.updatedAt,
          }]
          : []
      )))
      if(!page.hasMore || !page.nextCursor) return
      after = page.nextCursor
    } while(after)
  }, [cloudQueue, repository, repositoryForOwner, userRef])

  const restoreCloudOutbox = useCallback(async () => {
    const ownerId = repository.currentOwnerId()
    if(!ownerId) return
    for await(const metadata of repository.iterateSessions(100)) {
      if(metadata.origin !== 'cloud') continue
      if(metadata.cloudSessionPending) {
        const recoveryUser = userRef.current
        if(!recoveryUser || recoveryUser.id !== ownerId) continue
        try {
          try {
            await getCloudSession(metadata.id, { includeTranscripts: false })
          } catch(reason) {
            if(!(reason instanceof ApiRequestError) || reason.status !== 404) {
              throw reason
            }
            const recreated = await createCloudSession({
              client_session_id: metadata.id,
              title: metadata.title,
              source_language:
                metadata.sourceLanguage ?? settingsRef.current.sourceLanguage,
              target_language:
                metadata.targetLanguage ?? settingsRef.current.targetLanguage,
            })
            if(recreated.id !== metadata.id) {
              throw new Error(
                messages().workspace.runtime.inconsistentSession,
                { cause: reason },
              )
            }
          }
          if(
            repository.currentOwnerId() !== ownerId
            || userRef.current?.id !== ownerId
          ) {
            return
          }
          await repository.updateSessionMetadata(metadata.id, {
            cloudSessionPending: false,
          }, { touch: false })
          if(currentSessionRef.current === metadata.id) {
            cloudSessionVerifiedRef.current = true
            if(cloudSessionRef.current === metadata.id) {
              cloudQueue.setSession(metadata.id)
            }
          }
          void syncCloudMetadata(metadata.id, {
            ...(metadata.title ? { title: metadata.title } : {}),
            status: metadata.status === 'active' ? 'active' : 'completed',
            duration_seconds: Math.round((metadata.durationMs ?? 0) / 1_000),
          }).catch(() => undefined)
        } catch {
          // Keep the durable pending marker and outbox untouched. A later
          // online event or application restart will retry the same UUID.
          continue
        }
      }
      await restoreCloudSessionOutbox(metadata.id, ownerId)
    }
  }, [cloudQueue, cloudSessionRef, cloudSessionVerifiedRef, currentSessionRef, repository, restoreCloudSessionOutbox, settingsRef, syncCloudMetadata, userRef])
  return {
    releaseSessionLock,
    acquireSessionLock,
    syncCloudMetadata,
    ownerScopeIsCurrent,
    setRecorderStatus,
    updateElapsed,
    enqueueLocal,
    checkpointDuration,
    queueCloudInput,
    restoreCloudSessionOutbox,
    restoreCloudOutbox,
  }
}
export type WorkspacePersistence = ReturnType<typeof useWorkspacePersistence>
