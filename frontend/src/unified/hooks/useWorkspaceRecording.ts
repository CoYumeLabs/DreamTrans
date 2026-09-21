import type { UnifiedSettings } from './useUnifiedSettings'
import {
  useCallback,
  useRef
} from 'react'
import {
  BrowserAudioCapture,
  probePreferredAudioSampleRate,
  type AudioCaptureError,
} from '../../core/audio/BrowserAudioCapture'
import { messages } from '../../i18n'
import {
  ApiRequestError,
  createSession as createCloudSession,
  deleteSession as deleteCloudSession,
  getSession as getCloudSession
} from '../../pro/api/auth'
import { lexReset } from '../../utils/lexicon'
import { ensureSpeechmaticsPreflight } from '../workspace/speechmaticsPreflight'
import type { WorkspaceHistoryIndex } from './useWorkspaceHistoryIndex'
import type { WorkspacePersistence } from './useWorkspacePersistence'
import type { WorkspaceRuntime } from './useWorkspaceRuntime'
import { defaultSessionTitle, shouldRetryCloudRequest, waitForRetry } from './workspaceModel'

type Dependencies = Pick<
  WorkspaceRuntime & WorkspacePersistence & WorkspaceHistoryIndex,
  "stopPromiseRef"
  | "statusRef"
  | "lifecycleEpochRef"
  | "setRecorderStatus"
  | "elapsedRunStartedRef"
  | "elapsedAccumulatedRef"
  | "updateElapsed"
  | "checkpointDuration"
  | "captureRef"
  | "sessionTranslationEngineRef"
  | "aiTranslator"
  | "client"
  | "sessionAuthRequiredRef"
  | "currentSessionRef"
  | "localWriteChainRef"
  | "repository"
  | "cloudSessionRef"
  | "cloudQueue"
  | "syncCloudMetadata"
  | "setError"
  | "releaseSessionLock"
  | "paymentRequiredRef"
  | "setPaymentRequired"
  | "refreshHistory"
  | "balanceCallbackRef"
  | "autoTitleRef"
  | "settingsRef"
  | "localAudioHealthyRef"
  | "enqueueLocal"
  | "startPromiseRef"
  | "userRef"
  | "repositoryOwnerRef"
  | "acquireSessionLock"
  | "currentLocationRef"
  | "cloudSessionVerifiedRef"
  | "setSessionId"
  | "setSessionSourceLanguage"
  | "setTitle"
  | "setElapsedSeconds"
  | "orphanTranslationsRef"
  | "wordCounterRef"
  | "setTopWords"
  | "transcriptStore"
  | "feedModel"
  | "ragQueue"
  | "restoreCloudOutbox"
  | "sessionTargetLanguageRef"
  | "currentAudioMimeTypeRef"
  | "sessionLockKeyRef"
  | "restoreCloudSessionOutbox"
  | "paymentHandlerRef"
>

// Control recording transitions, cancellation, payment suspension and transport drain.
export function useWorkspaceRecording(scope: Dependencies) {
  const {
    stopPromiseRef,
    statusRef,
    lifecycleEpochRef,
    setRecorderStatus,
    elapsedRunStartedRef,
    elapsedAccumulatedRef,
    updateElapsed,
    checkpointDuration,
    captureRef,
    sessionTranslationEngineRef,
    aiTranslator,
    client,
    sessionAuthRequiredRef,
    currentSessionRef,
    localWriteChainRef,
    repository,
    cloudSessionRef,
    cloudQueue,
    syncCloudMetadata,
    setError,
    releaseSessionLock,
    paymentRequiredRef,
    setPaymentRequired,
    refreshHistory,
    balanceCallbackRef,
    autoTitleRef,
    settingsRef,
    localAudioHealthyRef,
    enqueueLocal,
    startPromiseRef,
    userRef,
    repositoryOwnerRef,
    acquireSessionLock,
    currentLocationRef,
    cloudSessionVerifiedRef,
    setSessionId,
    setSessionSourceLanguage,
    setTitle,
    setElapsedSeconds,
    orphanTranslationsRef,
    wordCounterRef,
    setTopWords,
    transcriptStore,
    feedModel,
    ragQueue,
    restoreCloudOutbox,
    sessionTargetLanguageRef,
    currentAudioMimeTypeRef,
    sessionLockKeyRef,
    restoreCloudSessionOutbox,
    paymentHandlerRef,
  } = scope


  const stop = useCallback((): Promise<void> => {
    if(stopPromiseRef.current) return stopPromiseRef.current
    if(statusRef.current === 'idle') return Promise.resolve()

    lifecycleEpochRef.current += 1
    setRecorderStatus('stopping')
    if(elapsedRunStartedRef.current !== null) {
      elapsedAccumulatedRef.current += performance.now() - elapsedRunStartedRef.current
      elapsedRunStartedRef.current = null
    }
    updateElapsed()
    // Queue the duration write before the first await: on pagehide the page
    // may not survive the capture/socket/translation drain below, and this
    // small IndexedDB transaction is what keeps history truthful.
    checkpointDuration()

    const operation = (async () => {
      const stopFailures: Error[] = []
      let aiDrainCompleted = true
      const rememberFailure = (reason: unknown) => {
        if(stopFailures.length === 0) {
          stopFailures.push(
            reason instanceof Error ? reason : new Error(String(reason)),
          )
        }
      }

      // Stop capture first so its final PCM reaches the still-open
      // transcription socket. Then wait for Speechmatics EndOfStream finals,
      // feed those finals into the AI chunker, and only then drain translation.
      // This order keeps the last sentence instead of closing its consumer
      // before the provider has emitted it.
      const initialCapture = captureRef.current
      await initialCapture?.stop().catch(rememberFailure)
      const capture = captureRef.current
      captureRef.current = null
      if(capture && capture !== initialCapture) {
        await capture.stop().catch(rememberFailure)
      }
      // Let already-finalized speech translate while Speechmatics emits its
      // EndOfStream finals. The translator remains active until client.stop()
      // resolves, so those last finals are still accepted before the final
      // drain is sealed.
      if(sessionTranslationEngineRef.current === 'ai') aiTranslator.flush()
      await client.stop().catch(rememberFailure)
      try {
        aiDrainCompleted = await aiTranslator.stopSession()
      } catch(reason) {
        rememberFailure(reason)
      }
      sessionTranslationEngineRef.current = ''
      sessionAuthRequiredRef.current = false

      const activeSessionId = currentSessionRef.current
      if(activeSessionId) {
        await localWriteChainRef.current
        const durationMs = Math.round(elapsedAccumulatedRef.current)
        try {
          await repository.completeSession(activeSessionId, { durationMs })
        } catch(reason) {
          rememberFailure(reason)
        }

        if(cloudSessionRef.current === activeSessionId) {
          try {
            // Local completion is the durability boundary. Cloud transcript
            // batches continue in the bounded background queue so stopping a
            // long session cannot wait on every network request.
            void cloudQueue.flush().catch(() => undefined)
            void syncCloudMetadata(activeSessionId, {
              status: 'completed',
              duration_seconds: Math.round(durationMs / 1_000),
            }).catch((reason: unknown) => {
              setError(messages().workspace.runtime.localFinishedCloudRetry(
                reason instanceof Error ? reason.message : String(reason),
              ))
            })
          } catch(reason) {
            rememberFailure(reason)
          }
        }
      }

      cloudQueue.setSession(null)
      cloudSessionRef.current = null
      releaseSessionLock()
      setRecorderStatus('idle')
      paymentRequiredRef.current = false
      setPaymentRequired(false)
      const stopFailure = stopFailures[0]
      if(stopFailure) {
        setError(messages().workspace.runtime.finishStepFailed(stopFailure.message))
      } else if(!aiDrainCompleted) {
        setError(messages().workspace.runtime.translationTimeout)
      }
      void refreshHistory()
      balanceCallbackRef.current?.(null)
      // Name the session once its transcript is final. Fire-and-forget: the
      // stop flow is already complete and a naming failure is not an error
      // the user needs to act on.
      if(activeSessionId && !stopFailures.length) autoTitleRef.current?.(activeSessionId)
    })()

    const tracked = operation
      .catch((reason: unknown) => {
        sessionAuthRequiredRef.current = false
        releaseSessionLock()
        setRecorderStatus('idle')
        setError(messages().workspace.runtime.finishFailed(
          reason instanceof Error ? reason.message : String(reason),
        ))
      })
      .finally(() => {
        if(stopPromiseRef.current === tracked) stopPromiseRef.current = null
      })
    stopPromiseRef.current = tracked
    return tracked
  }, [aiTranslator, autoTitleRef, balanceCallbackRef, captureRef, checkpointDuration, client, cloudQueue, cloudSessionRef, currentSessionRef, elapsedAccumulatedRef, elapsedRunStartedRef, lifecycleEpochRef, localWriteChainRef, paymentRequiredRef, refreshHistory, releaseSessionLock, repository, sessionAuthRequiredRef, sessionTranslationEngineRef, setError, setPaymentRequired, setRecorderStatus, statusRef, stopPromiseRef, syncCloudMetadata, updateElapsed])
  const stopRef = useRef(stop)
  stopRef.current = stop

  const handleCaptureError = useCallback((captureError: AudioCaptureError) => {
    if(captureError.code === 'microphone-ended') {
      const source = settingsRef.current.audioSource
      const sourceNames = messages().workspace.runtime.sourceNames
      const sourceLabel = source === 'system'
        ? sourceNames.system
        : source === 'mixed'
          ? sourceNames.audio
          : sourceNames.microphone
      setError(messages().workspace.runtime.sourceDisconnected(sourceLabel, captureError.message))
      void stop()
      return
    }
    if(
      captureError.code === 'audio-encoder-failed'
      || captureError.code === 'audio-storage-backpressure'
      || captureError.code === 'audio-storage-write-failed'
    ) {
      localAudioHealthyRef.current = false
      const activeSessionId = currentSessionRef.current
      if(activeSessionId) {
        void enqueueLocal(
          (scopedRepository) => scopedRepository.markLocalAudioIncomplete(
            activeSessionId,
          ),
        )
      }
      setError(messages().workspace.runtime.localAudioStopped(captureError.message))
      return
    }
    setError(captureError.message)
  }, [currentSessionRef, enqueueLocal, localAudioHealthyRef, setError, settingsRef, stop])

  // Starting and continuing use the same transport, PCM format and cleanup.
  // Capture ownership is registered before its first await so stop/cancel can drain it.
  const startTransport = useCallback(async ({
    activeSettings, activeSessionId, sourceLanguage, targetLanguage, cloud,
    audioSequenceOffset = 0, timelineOffset, previousAudioMimeType,
    isCurrent, assertCurrent, onCapture,
  }: {
    activeSettings: UnifiedSettings
    activeSessionId: string
    sourceLanguage: string
    targetLanguage: string
    cloud: boolean
    audioSequenceOffset?: number
    timelineOffset?: number
    previousAudioMimeType?: string
    isCurrent: () => boolean
    assertCurrent: () => void
    onCapture: (capture: BrowserAudioCapture) => void
  }) => {
    const learningLive = activeSettings.assistMode === 'learn'
    const useAiTranslation = !learningLive
      && activeSettings.translationEnabled
      && activeSettings.translationEngine === 'ai'
    const useSpeechmaticsTranslation = !learningLive
      && activeSettings.translationEnabled
      && !useAiTranslation
    sessionTranslationEngineRef.current = useAiTranslation
      ? 'ai'
      : useSpeechmaticsTranslation
        ? 'speechmatics'
        : ''
    sessionTargetLanguageRef.current = targetLanguage
    // Match Speechmatics clock to the AudioContext the browser will run,
    // otherwise resampling drift shows up as growing / random transcript lag.
    const captureSampleRate = await probePreferredAudioSampleRate(48_000)
    assertCurrent()
    await client.start({
      ...(timelineOffset === undefined ? {} : { timeline_offset_seconds: timelineOffset }),
      language: sourceLanguage,
      enable_partials: true,
      diarization: 'speaker',
      operating_point: 'enhanced',
      max_delay: 2.0,
      audio_format: {
        type: 'raw',
        encoding: 'pcm_f32le',
        sample_rate: captureSampleRate,
        channels: 1,
      },
      ...(useSpeechmaticsTranslation
        ? {
          translation_config: {
            target_languages: [targetLanguage],
            enable_partials: true,
          },
        }
        : {}),
    })
    assertCurrent()
    if(useAiTranslation) {
      aiTranslator.startSession({
        ...(cloud ? { sessionId: activeSessionId } : {}),
        translatePrompt: activeSettings.translatePrompt.trim(),
        sourceLanguage: sourceLanguage,
        targetLanguage: targetLanguage,
      })
    }

    const capture = new BrowserAudioCapture({
      audioSource: activeSettings.audioSource,
      sampleRate: captureSampleRate,
      onError: handleCaptureError,
      onPCM: (audio) => {
        try {
          client.sendAudio(audio)
        } catch(reason) {
          if(isCurrent()) {
            setError(
              messages().workspace.runtime.audioSendFailed(reason instanceof Error ? reason.message : String(reason)),
            )
          }
        }
      },
      ...(activeSettings.keepLocalAudio
        ? {
          onChunk: async (chunk: { sequence: number; recordedAt: number; blob: Blob }) => {
            await enqueueLocal((scopedRepository) => (
              scopedRepository.appendAudioChunk(
                activeSessionId,
                chunk.blob,
                {
                  sequence: audioSequenceOffset + chunk.sequence,
                  capturedAt: chunk.recordedAt,
                  durationMs: 2_000,
                  mimeType: chunk.blob.type,
                },
              )
            ), true)
          },
        }
        : {}),
    })
    onCapture(capture)
    localAudioHealthyRef.current = activeSettings.keepLocalAudio
    captureRef.current = capture
    currentAudioMimeTypeRef.current = previousAudioMimeType && !activeSettings.keepLocalAudio
      ? previousAudioMimeType
      : capture.mimeType
    await capture.start()
    assertCurrent()
    if(activeSettings.keepLocalAudio) {
      await repository.updateSessionMetadata(activeSessionId, {
        audioMimeType: capture.mimeType,
      })
      assertCurrent()
    }
  }, [aiTranslator, client, enqueueLocal, handleCaptureError, repository,
    captureRef, currentAudioMimeTypeRef, localAudioHealthyRef,
    sessionTargetLanguageRef, sessionTranslationEngineRef, setError])

  const cleanupFailedTransport = useCallback(async (
    capture: BrowserAudioCapture | null, cancelled: boolean,
  ) => {
    if(captureRef.current === capture) captureRef.current = null
    await capture?.stop().catch(() => undefined)
    if(!cancelled) await client.stop().catch(() => undefined)
    if(sessionTranslationEngineRef.current === 'ai') {
      await aiTranslator.stopSession().catch(() => false)
    }
    sessionTranslationEngineRef.current = ''
  }, [aiTranslator, client, captureRef, sessionTranslationEngineRef])

  const start = useCallback((): Promise<void> => {
    if(statusRef.current !== 'idle') {
      return startPromiseRef.current ?? Promise.resolve()
    }

    const epoch = ++lifecycleEpochRef.current
    const isCurrent = () => lifecycleEpochRef.current === epoch
    const assertCurrent = () => {
      if(!isCurrent()) {
        throw new DOMException('Session start was cancelled', 'AbortError')
      }
    }

    setError(null)
    setRecorderStatus('starting')
    const activeSettings = settingsRef.current
    const startingUser = userRef.current
    const startingOwnerId = repositoryOwnerRef.current
    sessionAuthRequiredRef.current = startingUser !== null
    const sessionTitle = defaultSessionTitle()
    const createdAt = Date.now()
    let nextSessionId: string = crypto.randomUUID()
    let cloudCreated = false
    let cloudCreationUncertain = false
    let startingCapture: BrowserAudioCapture | null = null

    const operation = (async () => {
      try {
        await ensureSpeechmaticsPreflight()
        assertCurrent()
        if(startingUser) {
          try {
            let cloudSession: Awaited<ReturnType<typeof createCloudSession>> | null = null
            let lastCreateFailure: unknown
            for(const delayMs of [0, 400, 1_200]) {
              if(delayMs > 0) await waitForRetry(delayMs)
              assertCurrent()
              try {
                cloudSession = await createCloudSession({
                  client_session_id: nextSessionId,
                  title: sessionTitle,
                  source_language: activeSettings.sourceLanguage,
                  target_language: activeSettings.targetLanguage,
                })
                break
              } catch(reason) {
                lastCreateFailure = reason
                cloudCreationUncertain = (
                  cloudCreationUncertain || shouldRetryCloudRequest(reason)
                )
                if(!shouldRetryCloudRequest(reason)) throw reason
              }
            }
            if(!cloudSession) throw lastCreateFailure ?? new Error(messages().workspace.runtime.cloudCreateFailed)
            nextSessionId = cloudSession.id
            cloudCreated = true
            cloudCreationUncertain = false
          } catch(reason) {
            assertCurrent()
            setError(
              cloudCreationUncertain
                ? messages().workspace.runtime.cloudCreateUncertain(
                  reason instanceof Error ? reason.message : String(reason),
                )
                : messages().workspace.runtime.cloudCreateLocal(
                  reason instanceof Error ? reason.message : String(reason),
                ),
            )
          }
        }
        assertCurrent()
        if(!await acquireSessionLock(nextSessionId, startingOwnerId)) {
          throw new Error(messages().workspace.runtime.sessionLocked)
        }
        assertCurrent()

        currentSessionRef.current = nextSessionId
        const cloudIntended = cloudCreated || cloudCreationUncertain
        currentLocationRef.current = cloudIntended ? 'cloud' : 'local'
        cloudSessionRef.current = cloudIntended ? nextSessionId : null
        cloudSessionVerifiedRef.current = cloudCreated
        cloudQueue.setOwner(startingUser?.id ?? null)
        cloudQueue.setSession(cloudCreated ? nextSessionId : null)
        setSessionId(nextSessionId)
        setSessionSourceLanguage(activeSettings.sourceLanguage)
        setTitle(sessionTitle)
        elapsedAccumulatedRef.current = 0
        elapsedRunStartedRef.current = null
        setElapsedSeconds(0)
        orphanTranslationsRef.current.clear()
        wordCounterRef.current.reset()
        setTopWords([])
        lexReset(nextSessionId)
        transcriptStore.reset()
        feedModel.reset({
          sourceLanguage: activeSettings.sourceLanguage,
          targetLanguage: activeSettings.targetLanguage,
          translationEnabled: activeSettings.translationEnabled,
        })
        ragQueue.clear()

        await repository.ensureSession(nextSessionId, {
          createdAt,
          origin: cloudIntended ? 'cloud' : 'local',
          cloudSessionPending: cloudCreationUncertain,
          sourceLanguage: activeSettings.sourceLanguage,
          targetLanguage: activeSettings.targetLanguage,
          title: sessionTitle,
          status: 'active',
        })
        assertCurrent()
        if(cloudCreationUncertain && startingUser) {
          // navigator.onLine often remains true during a proxy/server outage,
          // so an `online` event may never arrive. Retry deterministic cloud
          // verification in the background while recording continues locally.
          void (async () => {
            for(const delayMs of [5_000, 15_000, 30_000, 60_000]) {
              await waitForRetry(delayMs)
              if(
                !isCurrent()
                || userRef.current?.id !== startingUser.id
                || cloudSessionVerifiedRef.current
              ) {
                return
              }
              await restoreCloudOutbox().catch(() => undefined)
            }
          })()
        }
        // Never silently change the translation provider. If the user chose
        // AI, keep that engine and surface an AI capability/connectivity error
        // while preserving the original transcript.
        // Learning mode is original-first with local glosses; never stack AI
        // translation latency on the live path.
        await startTransport({
          activeSettings, activeSessionId: nextSessionId,
          sourceLanguage: activeSettings.sourceLanguage,
          targetLanguage: activeSettings.targetLanguage, cloud: cloudCreated,
          isCurrent, assertCurrent, onCapture: (capture) => { startingCapture = capture },
        })
        elapsedRunStartedRef.current = performance.now()
        setRecorderStatus('recording')
        void refreshHistory()
      } catch(reason) {
        const cancelled = !isCurrent()
        const failure = reason instanceof Error ? reason : new Error(String(reason))
        await cleanupFailedTransport(startingCapture, cancelled)
        await localWriteChainRef.current
        if(cloudSessionRef.current === nextSessionId) {
          cloudSessionRef.current = null
          cloudSessionVerifiedRef.current = false
          cloudQueue.setSession(null)
        }
        let preserveCloudRecovery = false
        if(
          (cloudCreated || cloudCreationUncertain)
          && startingUser
          && userRef.current?.id === startingUser.id
        ) {
          try {
            await deleteCloudSession(nextSessionId)
          } catch {
            // A create or delete response can both be lost on the same bad
            // network. Keep the local cloud intent so a later retry can
            // reconcile the deterministic session ID instead of orphaning a
            // quota-consuming server session.
            preserveCloudRecovery = true
          }
        } else if(cloudCreated || cloudCreationUncertain) {
          preserveCloudRecovery = true
        }
        if(repositoryOwnerRef.current === startingOwnerId) {
          if(preserveCloudRecovery) {
            await repository.completeSession(nextSessionId).catch(() => undefined)
          } else {
            await repository.deleteSession(nextSessionId).catch(() => undefined)
          }
        }
        if(currentSessionRef.current === nextSessionId) {
          currentSessionRef.current = ''
          setSessionId('')
          setSessionSourceLanguage(settingsRef.current.sourceLanguage)
        }
        if(sessionLockKeyRef.current.endsWith(`:${nextSessionId}`)) {
          releaseSessionLock()
        }
        sessionAuthRequiredRef.current = false
        if(!cancelled) {
          setRecorderStatus('idle')
          setError(messages().workspace.runtime.startFailed(failure.message))
        }
      }
    })()

    const tracked = operation.finally(() => {
      if(startPromiseRef.current === tracked) startPromiseRef.current = null
    })
    startPromiseRef.current = tracked
    return tracked
  }, [statusRef, lifecycleEpochRef, setError, setRecorderStatus, settingsRef, userRef, repositoryOwnerRef, sessionAuthRequiredRef, startPromiseRef, acquireSessionLock, currentSessionRef, currentLocationRef, cloudSessionRef, cloudSessionVerifiedRef, cloudQueue, setSessionId, setSessionSourceLanguage, setTitle, elapsedAccumulatedRef, elapsedRunStartedRef, setElapsedSeconds, orphanTranslationsRef, wordCounterRef, setTopWords, transcriptStore, feedModel, ragQueue, repository, startTransport, refreshHistory, restoreCloudOutbox, cleanupFailedTransport, localWriteChainRef, sessionLockKeyRef, releaseSessionLock])

  const continueSession = useCallback((): Promise<void> => {
    if(statusRef.current !== 'idle') {
      return startPromiseRef.current ?? Promise.resolve()
    }
    const continuingSessionId = currentSessionRef.current
    if(!continuingSessionId) return start()

    const epoch = ++lifecycleEpochRef.current
    const isCurrent = () => lifecycleEpochRef.current === epoch
    const assertCurrent = () => {
      if(!isCurrent()) {
        throw new DOMException('Session continue was cancelled', 'AbortError')
      }
    }
    const activeSettings = settingsRef.current
    const startingUser = userRef.current
    const startingOwnerId = repositoryOwnerRef.current
    const continuingCloud = currentLocationRef.current === 'cloud' && startingUser !== null
    const transcriptTimelineEnd = transcriptStore.getSnapshot().stats.durationSeconds
    let startingCapture: BrowserAudioCapture | null = null
    let previousStatus: 'active' | 'completed' = 'completed'

    sessionAuthRequiredRef.current = startingUser !== null
    setError(null)
    setRecorderStatus('starting')

    const operation = (async () => {
      try {
        const metadata = await repository.getSessionMetadata(continuingSessionId)
        if(!metadata) throw new Error(messages().workspace.runtime.localMissing)
        const sessionSourceLanguage =
          metadata.sourceLanguage ?? activeSettings.sourceLanguage
        const sessionTargetLanguage =
          metadata.targetLanguage ?? activeSettings.targetLanguage
        if(!await acquireSessionLock(continuingSessionId, startingOwnerId)) {
          throw new Error(messages().workspace.runtime.sessionLocked)
        }
        assertCurrent()
        if(
          activeSettings.keepLocalAudio
          && metadata.audioChunkCount > 0
          && metadata.audioMimeType
          && !metadata.audioMimeType.includes('mpeg')
          && !metadata.audioMimeType.includes('mp3')
        ) {
          throw new Error(
            messages().workspace.runtime.legacyAudio,
          )
        }
        previousStatus = metadata.status
        if(continuingCloud && !cloudSessionVerifiedRef.current) {
          try {
            await getCloudSession(continuingSessionId, {
              includeTranscripts: false,
            })
            cloudSessionVerifiedRef.current = true
          } catch(reason) {
            if(reason instanceof ApiRequestError && reason.status === 404) {
              const recreated = await createCloudSession({
                client_session_id: continuingSessionId,
                title: metadata.title,
                source_language: sessionSourceLanguage,
                target_language: sessionTargetLanguage,
              })
              if(recreated.id !== continuingSessionId) {
                throw new Error(
                  messages().workspace.runtime.inconsistentSession,
                  { cause: reason },
                )
              }
              cloudSessionVerifiedRef.current = true
            } else {
              throw new Error(messages().workspace.runtime.cloudStateFailed(
                reason instanceof Error ? reason.message : String(reason),
              ), { cause: reason })
            }
          }
          await repository.updateSessionMetadata(continuingSessionId, {
            cloudSessionPending: false,
            sourceLanguage: sessionSourceLanguage,
            targetLanguage: sessionTargetLanguage,
          }, { touch: false })
          assertCurrent()
          await restoreCloudSessionOutbox(
            continuingSessionId,
            startingOwnerId,
          )
          assertCurrent()
        }
        const audioSequenceOffset = metadata.nextAudioSequence
        const timelineOffset = Math.max(
          transcriptTimelineEnd,
          (metadata.durationMs ?? 0) / 1_000,
        )
        await ensureSpeechmaticsPreflight()
        assertCurrent()
        await repository.updateSessionMetadata(continuingSessionId, {
          status: 'active',
          sourceLanguage: sessionSourceLanguage,
          targetLanguage: sessionTargetLanguage,
        })
        assertCurrent()
        setSessionSourceLanguage(sessionSourceLanguage)

        cloudQueue.setOwner(startingUser?.id ?? null)
        cloudSessionRef.current = continuingCloud ? continuingSessionId : null
        cloudQueue.setSession(continuingCloud ? continuingSessionId : null)

        await startTransport({
          activeSettings, activeSessionId: continuingSessionId,
          sourceLanguage: sessionSourceLanguage, targetLanguage: sessionTargetLanguage,
          cloud: continuingCloud, audioSequenceOffset, timelineOffset,
          previousAudioMimeType: metadata.audioMimeType || currentAudioMimeTypeRef.current,
          isCurrent, assertCurrent, onCapture: (capture) => { startingCapture = capture },
        })
        if(continuingCloud) {
          void syncCloudMetadata(continuingSessionId, {
            status: 'active',
          }).catch(() => undefined)
        }
        elapsedAccumulatedRef.current = Math.max(
          elapsedAccumulatedRef.current,
          (metadata.durationMs ?? 0),
        )
        elapsedRunStartedRef.current = performance.now()
        setRecorderStatus('recording')
        void refreshHistory()
      } catch(reason) {
        const cancelled = !isCurrent()
        const failure = reason instanceof Error ? reason : new Error(String(reason))
        await cleanupFailedTransport(startingCapture, cancelled)
        if(cloudSessionRef.current === continuingSessionId) {
          cloudSessionRef.current = null
          cloudSessionVerifiedRef.current = false
          cloudQueue.setSession(null)
        }
        if(repositoryOwnerRef.current === startingOwnerId) {
          await repository.updateSessionMetadata(continuingSessionId, {
            status: previousStatus,
          }).catch(() => undefined)
        }
        sessionAuthRequiredRef.current = false
        if(sessionLockKeyRef.current.endsWith(`:${continuingSessionId}`)) {
          releaseSessionLock()
        }
        if(!cancelled) {
          setRecorderStatus('idle')
          setError(messages().workspace.runtime.continueFailed(failure.message))
        }
      }
    })()

    const tracked = operation.finally(() => {
      if(startPromiseRef.current === tracked) startPromiseRef.current = null
    })
    startPromiseRef.current = tracked
    return tracked
  }, [statusRef, currentSessionRef, start, lifecycleEpochRef, settingsRef, userRef, repositoryOwnerRef, currentLocationRef, transcriptStore, sessionAuthRequiredRef, setError, setRecorderStatus, startPromiseRef, repository, acquireSessionLock, cloudSessionVerifiedRef, setSessionSourceLanguage, cloudQueue, cloudSessionRef, startTransport, currentAudioMimeTypeRef, elapsedAccumulatedRef, elapsedRunStartedRef, refreshHistory, restoreCloudSessionOutbox, syncCloudMetadata, cleanupFailedTransport, sessionLockKeyRef, releaseSessionLock])

  const pauseToggle = useCallback(() => {
    if(statusRef.current === 'recording' || statusRef.current === 'reconnecting') {
      const previousStatus = statusRef.current
      if(elapsedRunStartedRef.current !== null) {
        elapsedAccumulatedRef.current += performance.now() - elapsedRunStartedRef.current
        elapsedRunStartedRef.current = null
      }
      updateElapsed()
      checkpointDuration({ cloud: true })
      captureRef.current?.setPaused(true)
      try {
        client.pause()
        setRecorderStatus('paused')
      } catch(reason) {
        // Keep capture, timer and UI state atomic with the transcription
        // client. A failed remote pause must not leave the microphone silently
        // paused while the interface still claims to be recording.
        captureRef.current?.setPaused(false)
        elapsedRunStartedRef.current = performance.now()
        setRecorderStatus(previousStatus)
        setError(messages().workspace.runtime.pauseFailed(reason instanceof Error ? reason.message : String(reason)))
      }
      return
    }
    if(statusRef.current !== 'paused') return
    const resumeEpoch = lifecycleEpochRef.current
    const resumeSessionId = currentSessionRef.current
    setRecorderStatus('reconnecting')
    void client.resume()
      .then(() => {
        if(
          lifecycleEpochRef.current !== resumeEpoch
          || currentSessionRef.current !== resumeSessionId
          || statusRef.current !== 'reconnecting'
        ) {
          return
        }
        captureRef.current?.setPaused(false)
        elapsedRunStartedRef.current = performance.now()
        paymentRequiredRef.current = false
        setPaymentRequired(false)
        setError(null)
        setRecorderStatus('recording')
      })
      .catch((reason: unknown) => {
        if(
          lifecycleEpochRef.current !== resumeEpoch
          || currentSessionRef.current !== resumeSessionId
          || statusRef.current !== 'reconnecting'
        ) {
          return
        }
        setRecorderStatus('paused')
        setError(messages().workspace.runtime.resumeFailed(reason instanceof Error ? reason.message : String(reason)))
      })
  }, [captureRef, checkpointDuration, client, currentSessionRef, elapsedAccumulatedRef, elapsedRunStartedRef, lifecycleEpochRef, paymentRequiredRef, setError, setPaymentRequired, setRecorderStatus, statusRef, updateElapsed])

  paymentHandlerRef.current = () => {
    if(
      paymentRequiredRef.current
      || statusRef.current === 'idle'
      || statusRef.current === 'starting'
      || statusRef.current === 'stopping'
    ) return
    paymentRequiredRef.current = true
    setPaymentRequired(true)
    captureRef.current?.setPaused(true)
    if(elapsedRunStartedRef.current !== null) {
      elapsedAccumulatedRef.current += performance.now() - elapsedRunStartedRef.current
      elapsedRunStartedRef.current = null
    }
    updateElapsed()
    checkpointDuration({ cloud: true })
    client.suspendForPayment()
    aiTranslator.suspendForPayment()
    setRecorderStatus('paused')
    setError(null)
  }
  return {
    stop,
    stopRef,
    handleCaptureError,
    start,
    continueSession,
    pauseToggle,
  }
}
export type WorkspaceRecording = ReturnType<typeof useWorkspaceRecording>
