import {
  useEffect,
  useMemo,
  useRef
} from 'react'
import {
  getSessionCostSummaries,
  parseAccountBalance
} from '../../api'
import {
  type TranslationSegment
} from '../../core/transcription'
import { messages } from '../../i18n'
import { getAccessToken, type TranscriptInput } from '../../pro/api/auth'
import { lexIngest, lexReset } from '../../utils/lexicon'
import type { RecorderStatus } from '../components/RecorderBar'
import type { WorkspaceHistoryIndex } from './useWorkspaceHistoryIndex'
import type { WorkspacePersistence } from './useWorkspacePersistence'
import type { WorkspaceRecording } from './useWorkspaceRecording'
import type { WorkspaceRuntime } from './useWorkspaceRuntime'
import { DURATION_CHECKPOINT_INTERVAL_MS, buildTransportDiagnostics, defaultSessionTitle } from './workspaceModel'

type Dependencies = Pick<
  WorkspaceRuntime & WorkspacePersistence & WorkspaceHistoryIndex & WorkspaceRecording,
  "feedModel"
  | "settings"
  | "recorderStatus"
  | "currentSessionRef"
  | "paymentRequiredRef"
  | "settingsRef"
  | "sessionTranslationEngineRef"
  | "aiTranslator"
  | "sessionTargetLanguageRef"
  | "cloudSessionRef"
  | "setSessionSourceLanguage"
  | "ragEnabled"
  | "ragQueue"
  | "onlineRecoveryRef"
  | "restoreCloudOutbox"
  | "refreshHistory"
  | "setError"
  | "statusRef"
  | "stop"
  | "user"
  | "repositoryOwnerRef"
  | "stopPromiseRef"
  | "historyRequestRef"
  | "historyLoadRequestRef"
  | "currentLocationRef"
  | "cloudSessionVerifiedRef"
  | "currentAudioMimeTypeRef"
  | "cloudQueue"
  | "setSessionId"
  | "setTitle"
  | "applyLoadedRecords"
  | "setHistorySessions"
  | "setHistoryLoading"
  | "setHistoryOpening"
  | "setLegacyHistoryCount"
  | "updateElapsed"
  | "checkpointDuration"
  | "client"
  | "enqueueLocal"
  | "queueCloudInput"
  | "ragEnabledRef"
  | "wordCounterRef"
  | "setTopWords"
  | "orphanTranslationsRef"
  | "transcriptStore"
  | "aiTranslationHandlerRef"
  | "paymentHandlerRef"
  | "setRecorderStatus"
  | "balanceCallbackRef"
  | "addSessionLiveCost"
  | "stopRef"
  | "repository"
  | "sessionId"
  | "setSessionCostBase"
  | "setSessionLiveCostUsd"
  | "sessionCostBase"
  | "sessionLiveCostUsd"
  | "destroyTimerRef"
  | "captureRef"
  | "releaseSessionLock"
  | "setTransportDiagnostics"
>

// Connect transport events and lifecycle effects to persistence and UI snapshots.
export function useWorkspaceSync(scope: Dependencies) {
  const {
    feedModel,
    settings,
    recorderStatus,
    currentSessionRef,
    paymentRequiredRef,
    settingsRef,
    sessionTranslationEngineRef,
    aiTranslator,
    sessionTargetLanguageRef,
    cloudSessionRef,
    setSessionSourceLanguage,
    ragEnabled,
    ragQueue,
    onlineRecoveryRef,
    restoreCloudOutbox,
    refreshHistory,
    setError,
    statusRef,
    stop,
    user,
    repositoryOwnerRef,
    stopPromiseRef,
    historyRequestRef,
    historyLoadRequestRef,
    currentLocationRef,
    cloudSessionVerifiedRef,
    currentAudioMimeTypeRef,
    cloudQueue,
    setSessionId,
    setTitle,
    applyLoadedRecords,
    setHistorySessions,
    setHistoryLoading,
    setHistoryOpening,
    setLegacyHistoryCount,
    updateElapsed,
    checkpointDuration,
    client,
    enqueueLocal,
    queueCloudInput,
    ragEnabledRef,
    wordCounterRef,
    setTopWords,
    orphanTranslationsRef,
    transcriptStore,
    aiTranslationHandlerRef,
    paymentHandlerRef,
    setRecorderStatus,
    balanceCallbackRef,
    addSessionLiveCost,
    stopRef,
    repository,
    sessionId,
    setSessionCostBase,
    setSessionLiveCostUsd,
    sessionCostBase,
    sessionLiveCostUsd,
    destroyTimerRef,
    captureRef,
    releaseSessionLock,
    setTransportDiagnostics,
  } = scope


  useEffect(() => {
    feedModel.configure({
      sourceLanguage: settings.sourceLanguage,
      targetLanguage: settings.targetLanguage,
      translationEnabled: settings.translationEnabled,
    })
  }, [
    feedModel,
    settings.sourceLanguage,
    settings.targetLanguage,
    settings.translationEnabled,
  ])

  // Learning mode can be toggled mid-session. Start/stop the AI translator to
  // match the live assist mode so switching back to 同传 actually resumes
  // context-aware translation (and backfills untranslated finals).
  useEffect(() => {
    const live = recorderStatus === 'recording'
      || recorderStatus === 'reconnecting'
      || recorderStatus === 'paused'
      || recorderStatus === 'error'
    if(!live || !currentSessionRef.current || paymentRequiredRef.current) return

    const activeSettings = settingsRef.current
    const wantAi = activeSettings.assistMode !== 'learn'
      && activeSettings.translationEnabled
      && activeSettings.translationEngine === 'ai'

    if(!wantAi) {
      // Keep the session engine flag honest while learning so new segments are
      // not enqueued; leave any in-flight AI work alone (no abrupt cancel).
      if(sessionTranslationEngineRef.current === 'ai') {
        sessionTranslationEngineRef.current = ''
      }
      return
    }

    const alreadyLive = sessionTranslationEngineRef.current === 'ai'
      && aiTranslator.isSessionActive()
    if(!alreadyLive) {
      sessionTranslationEngineRef.current = 'ai'
      sessionTargetLanguageRef.current = activeSettings.targetLanguage
      aiTranslator.startSession({
        ...(cloudSessionRef.current
          ? { sessionId: cloudSessionRef.current }
          : {}),
        translatePrompt: activeSettings.translatePrompt.trim(),
        sourceLanguage: activeSettings.sourceLanguage,
        targetLanguage: activeSettings.targetLanguage,
      })
    } else {
      sessionTranslationEngineRef.current = 'ai'
    }

    // Backfill cards that never received a final translation while learning.
    const snapshot = feedModel.getSnapshot()
    for(const item of snapshot.items) {
      const original = item.original?.text?.trim()
      if(!original) continue
      const hasFinalTranslation = Boolean(item.translation?.text?.trim())
      if(hasFinalTranslation) continue
      const segmentId = item.segmentIds?.[0] ?? item.id
      aiTranslator.addSegment(
        {
          id: segmentId,
          speaker: item.speaker,
          text: original,
          startTime: item.startTime ?? 0,
          endTime: item.endTime ?? item.startTime ?? 0,
        },
        item.id,
      )
    }
  }, [aiTranslator, feedModel, recorderStatus, settings.assistMode, settings.translationEnabled, settings.translationEngine, settings.translatePrompt, settings.sourceLanguage, settings.targetLanguage, currentSessionRef, paymentRequiredRef, settingsRef, sessionTranslationEngineRef, sessionTargetLanguageRef, cloudSessionRef])

  useEffect(() => {
    if(!currentSessionRef.current) {
      setSessionSourceLanguage(settings.sourceLanguage)
    }
  }, [currentSessionRef, setSessionSourceLanguage, settings.sourceLanguage])

  useEffect(() => {
    if(!ragEnabled || !settings.automaticAiIngest) ragQueue.clear()
  }, [ragEnabled, ragQueue, settings.automaticAiIngest])

  useEffect(() => {
    const handleOnline = () => {
      if(onlineRecoveryRef.current) return
      const operation = restoreCloudOutbox()
        .then(refreshHistory)
        .catch((reason: unknown) => {
          setError(
            messages().workspace.runtime.resyncFailed(
              reason instanceof Error ? reason.message : String(reason),
            ),
          )
        })
        .finally(() => {
          if(onlineRecoveryRef.current === operation) {
            onlineRecoveryRef.current = null
          }
        })
      onlineRecoveryRef.current = operation
    }
    window.addEventListener('online', handleOnline)
    return () => window.removeEventListener('online', handleOnline)
  }, [onlineRecoveryRef, refreshHistory, restoreCloudOutbox, setError])

  useEffect(() => {
    const handlePageHide = () => {
      if(statusRef.current !== 'idle' && statusRef.current !== 'stopping') {
        // Start local finalization while the page is still alive. Browsers do
        // not guarantee enough time for cloud I/O here, so durable IndexedDB
        // writes remain the recovery boundary and sync resumes next launch.
        void stop()
      }
    }
    window.addEventListener('pagehide', handlePageHide)
    return () => window.removeEventListener('pagehide', handlePageHide)
  }, [statusRef, stop])

  useEffect(() => {
    let cancelled = false
    const transitionOwner = async () => {
      const nextOwnerId = user?.id ?? null
      const ownerChanged = repositoryOwnerRef.current !== nextOwnerId
      if(
        ownerChanged
        && statusRef.current !== 'idle'
        && statusRef.current !== 'stopping'
      ) {
        setError(messages().workspace.runtime.authChanged)
        await stop()
      } else if(ownerChanged && statusRef.current === 'stopping') {
        await stopPromiseRef.current
      }
      if(cancelled) return

      if(ownerChanged) {
        historyRequestRef.current += 1
        historyLoadRequestRef.current += 1
        if(currentSessionRef.current) lexReset(currentSessionRef.current)
        repositoryOwnerRef.current = nextOwnerId
        currentSessionRef.current = ''
        currentLocationRef.current = 'local'
        cloudSessionVerifiedRef.current = false
        currentAudioMimeTypeRef.current = 'audio/webm'
        cloudSessionRef.current = null
        cloudQueue.setSession(null)
        setSessionId('')
        setSessionSourceLanguage(settingsRef.current.sourceLanguage)
        setTitle(defaultSessionTitle())
        applyLoadedRecords({ segments: [], translations: [] }, 0)
        setHistorySessions([])
        setHistoryLoading(false)
        setHistoryOpening(null)
        setLegacyHistoryCount(0)
        ragQueue.clear()
        setError(null)
      }

      cloudQueue.setOwner(nextOwnerId)
      // History list should not wait on outbox recovery (can walk every cloud
      // session). Restore outbox in parallel so the sidebar appears quickly.
      const restorePromise = restoreCloudOutbox()
      if(!cancelled) await refreshHistory()
      await restorePromise.catch((reason: unknown) => {
        if(!cancelled) {
          setError(
            messages().workspace.runtime.cloudResyncFailed(
              reason instanceof Error ? reason.message : String(reason),
            ),
          )
        }
      })
    }
    void transitionOwner().catch((reason: unknown) => {
      if(!cancelled) {
        setError(
          messages().workspace.runtime.accountSwitchFailed(reason instanceof Error ? reason.message : String(reason)),
        )
      }
    })
    return () => {
      cancelled = true
    }
  }, [applyLoadedRecords, cloudQueue, cloudSessionRef, cloudSessionVerifiedRef, currentAudioMimeTypeRef, currentLocationRef, currentSessionRef, historyLoadRequestRef, historyRequestRef, ragQueue, refreshHistory, repositoryOwnerRef, restoreCloudOutbox, setError, setHistoryLoading, setHistoryOpening, setHistorySessions, setLegacyHistoryCount, setSessionId, setSessionSourceLanguage, setTitle, settingsRef, statusRef, stop, stopPromiseRef, user?.id])

  useEffect(() => {
    if(
      recorderStatus !== 'recording'
      && recorderStatus !== 'reconnecting'
      && recorderStatus !== 'error'
    ) {
      return
    }
    updateElapsed()
    const timer = window.setInterval(updateElapsed, 1_000)
    return () => window.clearInterval(timer)
  }, [recorderStatus, updateElapsed])

  useEffect(() => {
    if(
      recorderStatus !== 'recording'
      && recorderStatus !== 'reconnecting'
      && recorderStatus !== 'error'
    ) {
      return
    }
    // Local every 15s; cloud every 60s. The history merge in refreshHistory
    // also pushes a larger local duration to the cloud, so the cloud write
    // here only shortens how long the two can disagree.
    let ticks = 0
    const timer = window.setInterval(() => {
      ticks += 1
      checkpointDuration({ cloud: ticks % 4 === 0 })
    }, DURATION_CHECKPOINT_INTERVAL_MS)
    return () => window.clearInterval(timer)
  }, [checkpointDuration, recorderStatus])

  useEffect(() => {
    const unsubscribers = [
      client.on('transcript', (segment) => {
        const activeSessionId = currentSessionRef.current
        if(!activeSessionId) return
        feedModel.appendSegment(segment)
        const liveSettings = settingsRef.current
        const wantAiNow = liveSettings.assistMode !== 'learn'
          && liveSettings.translationEnabled
          && liveSettings.translationEngine === 'ai'
        // Prefer live settings over the session snapshot so mid-session
        // assistMode toggles take effect without restarting the recorder.
        if(wantAiNow && !paymentRequiredRef.current) {
          if(
            sessionTranslationEngineRef.current !== 'ai'
            || !aiTranslator.isSessionActive()
          ) {
            sessionTranslationEngineRef.current = 'ai'
            aiTranslator.startSession({
              ...(cloudSessionRef.current
                ? { sessionId: cloudSessionRef.current }
                : {}),
              translatePrompt: liveSettings.translatePrompt.trim(),
              sourceLanguage: liveSettings.sourceLanguage,
              targetLanguage: liveSettings.targetLanguage,
            })
          }
          aiTranslator.addSegment(
            {
              id: segment.id,
              speaker: segment.speaker,
              text: segment.text,
              startTime: segment.startTime,
              endTime: segment.endTime,
            },
            feedModel.cardIdOf(segment.id) ?? segment.id,
          )
        }
        void enqueueLocal((scopedRepository) => scopedRepository.appendTranscript(
          activeSessionId,
          segment,
          { sequence: segment.sequence, recordId: segment.id },
        ))
        if(cloudSessionRef.current === activeSessionId) {
          const input: TranscriptInput = {
            client_segment_id: segment.id,
            speaker: segment.speaker,
            text: segment.text,
            start_time: segment.startTime,
            end_time: segment.endTime,
            status: 'confirmed',
            is_partial: false,
          }
          queueCloudInput(activeSessionId, input)
        }
        if(ragEnabledRef.current && settingsRef.current.automaticAiIngest) {
          const cardId = feedModel.cardIdOf(segment.id) ?? segment.id
          const card = feedModel.getSnapshot().items.find((item) => item.id === cardId)
          ragQueue.queue({
            id: cardId,
            sessionId: activeSessionId,
            speaker: card?.speaker ?? segment.speaker,
            text: card?.original?.text ?? segment.text,
            startTime: card?.startTime ?? segment.startTime,
            endTime: card?.endTime ?? segment.endTime,
          })
        }
        wordCounterRef.current.add(segment.text)
        setTopWords(wordCounterRef.current.getTop())
        lexIngest(activeSessionId, segment.text)

        // Final translations can arrive just before their matching final
        // transcript. Relink only the small bounded orphan set and update the
        // existing persisted record in place.
        for(const [translationId, orphan] of orphanTranslationsRef.current) {
          const matchingSegmentId = transcriptStore.findSegmentId(
            orphan.speaker,
            orphan.startTime,
            orphan.endTime,
          )
          if(matchingSegmentId !== segment.id) continue
          orphanTranslationsRef.current.delete(translationId)
          const linked = transcriptStore.relinkTranslation(translationId, segment.id)
          if(!linked) continue
          feedModel.appendTranslation(linked)
          void enqueueLocal((scopedRepository) => scopedRepository.upsertTranslation(
            activeSessionId,
            linked.id,
            linked,
          ))
          if(cloudSessionRef.current === activeSessionId) {
            queueCloudInput(activeSessionId, {
              client_segment_id: segment.id,
              speaker: segment.speaker,
              text: segment.text,
              translation: linked.text,
              start_time: segment.startTime,
              end_time: segment.endTime,
              status: 'translated',
              is_partial: false,
            })
          }
        }
      }),
      client.on('partial', (partial) => {
        if(partial) feedModel.setPartial(partial)
        else feedModel.clearPartial()
      }),
      client.on('translation', (translation) => {
        const activeSessionId = currentSessionRef.current
        if(!activeSessionId) return
        feedModel.appendTranslation(translation)
        if(!translation.segmentId) {
          if(orphanTranslationsRef.current.size >= 256) {
            const oldestId = orphanTranslationsRef.current.keys().next().value
            if(typeof oldestId === 'string') {
              orphanTranslationsRef.current.delete(oldestId)
            }
          }
          orphanTranslationsRef.current.set(translation.id, translation)
        }
        void enqueueLocal((scopedRepository) => scopedRepository.appendTranslation(
          activeSessionId,
          translation,
          { sequence: translation.sequence, recordId: translation.id },
        ))
        if(
          cloudSessionRef.current === activeSessionId
          && translation.segmentId
        ) {
          const segment = transcriptStore.getSegment(translation.segmentId)
          if(segment) {
            queueCloudInput(activeSessionId, {
              client_segment_id: segment.id,
              speaker: segment.speaker,
              text: segment.text,
              translation: translation.text,
              start_time: segment.startTime,
              end_time: segment.endTime,
              status: 'translated',
              is_partial: false,
            })
          }
        }
      }),
      client.on('translationPartial', (partial) => {
        if(partial) feedModel.setTranslationPartial(partial)
        else feedModel.clearTranslationPartial()
      }),
    ]

    // Context-aware AI paragraph translations reuse the same persistence and
    // sync path as provider translations; the record anchors on the chunk's
    // first segment and spans the chunk's full time range.
    aiTranslationHandlerRef.current = (chunk, result) => {
      const activeSessionId = currentSessionRef.current
      if(!activeSessionId || sessionTranslationEngineRef.current !== 'ai') return
      const firstSegmentId = chunk.segmentIds[0]
      if(!firstSegmentId) return
      let translation: TranslationSegment
      try {
        translation = transcriptStore.appendTranslation({
          segmentId: firstSegmentId,
          speaker: chunk.speaker,
          language: sessionTargetLanguageRef.current,
          text: result.text,
          startTime: chunk.startTime,
          endTime: chunk.endTime,
        }).record
      } catch {
        // The segment may have been cleared by a session switch mid-flight.
        return
      }
      feedModel.appendTranslation(translation)
      void enqueueLocal((scopedRepository) => scopedRepository.appendTranslation(
        activeSessionId,
        translation,
        { sequence: translation.sequence, recordId: translation.id },
      ))
      if(cloudSessionRef.current === activeSessionId) {
        for(const segmentId of chunk.segmentIds) {
          const segment = transcriptStore.getSegment(segmentId)
          if(!segment) continue
          const isAnchor = segment.id === firstSegmentId
          queueCloudInput(activeSessionId, {
            client_segment_id: segment.id,
            translation_group_id: translation.id,
            speaker: segment.speaker,
            text: segment.text,
            // The group ID marks every covered atom, but the paragraph text is
            // stored once on its anchor. Repeating it on every provider
            // fragment multiplies storage/quota/export payloads and causes old
            // clients to render the same translation many times.
            ...(isAnchor ? { translation: translation.text } : {}),
            start_time: segment.startTime,
            end_time: segment.endTime,
            status: 'translated',
            is_partial: false,
          })
        }
      }
    }

    const remaining = [
      client.on('paymentRequired', () => paymentHandlerRef.current()),
      client.on('state', (snapshot) => {
        if(snapshot.status === 'reconnecting' && (
          statusRef.current === 'recording'
          || statusRef.current === 'reconnecting'
          || statusRef.current === 'error'
        )) {
          setRecorderStatus('reconnecting')
        } else if(
          snapshot.status === 'error'
          && (
            statusRef.current === 'recording'
            || statusRef.current === 'reconnecting'
            || statusRef.current === 'error'
          )
        ) {
          setRecorderStatus('error')
        } else if(
          snapshot.status === 'running'
          && !paymentRequiredRef.current
          && (
            statusRef.current === 'reconnecting'
            || statusRef.current === 'error'
          )
        ) {
          setRecorderStatus('recording')
        }
      }),
      client.on('error', (event) => {
        if(statusRef.current !== 'idle' && statusRef.current !== 'stopping') {
          setError(event.message)
        }
      }),
      client.on('balance', (event) => {
        balanceCallbackRef.current?.(parseAccountBalance(event.balance))
        addSessionLiveCost(event.costUsd)
      }),
      client.on('audioDropped', (event) => {
        const kilobytes = Math.ceil(event.bytes / 1_024)
        setError(messages().workspace.runtime.congestionDropped(kilobytes))
      }),
      client.on('terminated', (event) => {
        setError(
          event.reason.startsWith('Edge:') ? event.reason.slice(5).trim() : event.reason.includes('administrator')
            ? messages().workspace.runtime.remoteAdmin
            : messages().workspace.runtime.remoteOther,
        )
        if(statusRef.current !== 'idle' && statusRef.current !== 'stopping') {
          void stopRef.current().catch(() => { })
        }
      }),
    ]
    return () => {
      aiTranslationHandlerRef.current = null
      for(const unsubscribe of [...unsubscribers, ...remaining]) unsubscribe()
    }
  }, [addSessionLiveCost, aiTranslator, client, cloudQueue, enqueueLocal, feedModel, ragQueue, repository, queueCloudInput, setRecorderStatus, transcriptStore, aiTranslationHandlerRef, currentSessionRef, settingsRef, paymentRequiredRef, cloudSessionRef, ragEnabledRef, wordCounterRef, setTopWords, sessionTranslationEngineRef, orphanTranslationsRef, sessionTargetLanguageRef, paymentHandlerRef, statusRef, setError, balanceCallbackRef, stopRef])

  // Seed the session's cost from the ledger whenever the active session
  // changes, and re-read it when a recording ends: transcription reservations
  // are 5-second-quantized and only refund their unused tail at settlement, so
  // the client-side running sum lands slightly high until this refresh.
  const previousCostSessionRef = useRef('')
  const previousRecorderStatusRef = useRef<RecorderStatus>('idle')
  useEffect(() => {
    const sessionChanged = previousCostSessionRef.current !== sessionId
    previousCostSessionRef.current = sessionId
    const previousStatus = previousRecorderStatusRef.current
    previousRecorderStatusRef.current = recorderStatus
    const recordingEnded = recorderStatus === 'idle' && previousStatus !== 'idle'
    const recordingBegan = recorderStatus === 'recording' && previousStatus !== 'recording'
    if(sessionChanged) {
      setSessionCostBase(null)
      setSessionLiveCostUsd(0)
    } else if(!recordingEnded && !recordingBegan) {
      return
    }
    if(!sessionId || !getAccessToken()) return
    let active = true
    void getSessionCostSummaries([sessionId])
      .then((summaries) => {
        if(!active) return
        setSessionCostBase(summaries[0] ?? null)
        setSessionLiveCostUsd(0)
      })
      .catch(() => {
        // The workspace stays usable without a cost figure.
      })
    return () => { active = false }
  }, [sessionId, recorderStatus, setSessionCostBase, setSessionLiveCostUsd])

  const sessionCost = useMemo(() => {
    const settledRealtimeUsd = (sessionCostBase?.transcription_usd ?? 0)
      + (sessionCostBase?.translation_usd ?? 0)
    const realtimeUsd = settledRealtimeUsd + sessionLiveCostUsd
    const aiUsd = sessionCostBase?.ai_usd ?? 0
    if(realtimeUsd <= 0 && aiUsd <= 0) return null
    return {
      realtimeUsd,
      aiUsd,
      // Mid-recording figures include unrefunded reservation tails.
      approximate: recorderStatus !== 'idle',
    }
  }, [sessionCostBase, sessionLiveCostUsd, recorderStatus])

  useEffect(() => {
    if(destroyTimerRef.current !== null) {
      window.clearTimeout(destroyTimerRef.current)
      destroyTimerRef.current = null
    }
    return () => {
      destroyTimerRef.current = window.setTimeout(() => {
        destroyTimerRef.current = null
        void captureRef.current?.stop()
        captureRef.current = null
        releaseSessionLock()
        client.destroy()
        aiTranslator.destroy()
        cloudQueue.destroy()
        ragQueue.destroy()
      }, 0)
    }
  }, [aiTranslator, captureRef, client, cloudQueue, destroyTimerRef, ragQueue, releaseSessionLock])

  useEffect(() => {
    const live = recorderStatus === 'recording'
      || recorderStatus === 'paused'
      || recorderStatus === 'reconnecting'
      || recorderStatus === 'error'
    if(!live || !settings.debugTransport) {
      setTransportDiagnostics(null)
      return
    }
    const tick = () => {
      try {
        setTransportDiagnostics(buildTransportDiagnostics(
          client.getDiagnostics(),
          aiTranslator.getDiagnostics(),
        ))
      } catch {
        // Diagnostics must never interfere with capture.
      }
    }
    tick()
    const timer = window.setInterval(tick, 500)
    return () => window.clearInterval(timer)
  }, [aiTranslator, client, recorderStatus, setTransportDiagnostics, settings.debugTransport])
  return {
    previousCostSessionRef,
    previousRecorderStatusRef,
    sessionCost,
  }
}
export type WorkspaceSync = ReturnType<typeof useWorkspaceSync>
