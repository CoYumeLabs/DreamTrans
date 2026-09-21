import {
  useCallback
} from 'react'
import {
  generateSessionTitle
} from '../../api'
import {
  createStableTranslationId,
  type TranscriptSegment,
  type TranslationSegment
} from '../../core/transcription'
import { messages } from '../../i18n'
import {
  ApiRequestError,
  createSession as createCloudSession,
  deleteSession as deleteCloudSession,
  getSession as getCloudSession,
  getSessionTranscriptsPage,
  saveTranscriptsBatch,
  updateSession as updateCloudSession,
  type Transcript as CloudTranscript,
  type TranscriptInput
} from '../../pro/api/auth'
import { lexReset } from '../../utils/lexicon'
import type {
  HistorySession
} from '../components/HistoryPanel'
import {
  chatHistoryKey,
  legacyChatHistoryKey,
} from '../workspace/browserStorageKeys'
import {
  downloadCompleteAudio,
  downloadSessionText,
  requestCompleteAudioSave,
} from '../workspace/downloads'
import {
  mergeSessionRecords,
  type StoredSessionRecords,
} from '../workspace/mergeSessionRecords'
import type { WorkspaceHistoryIndex } from './useWorkspaceHistoryIndex'
import type { WorkspacePersistence } from './useWorkspacePersistence'
import type { WorkspaceRecording } from './useWorkspaceRecording'
import type { WorkspaceRuntime } from './useWorkspaceRuntime'
import { buildTitleExcerpt, canonicalizeLocalSession, defaultSessionTitle, isDefaultSessionTitle } from './workspaceModel'

type Dependencies = Pick<
  WorkspaceRuntime & WorkspacePersistence & WorkspaceHistoryIndex & WorkspaceRecording,
  "ownerGenerationRef"
  | "repositoryOwnerRef"
  | "historyLoadRequestRef"
  | "ownerScopeIsCurrent"
  | "statusRef"
  | "setError"
  | "stop"
  | "setHistoryOpening"
  | "settingsRef"
  | "userRef"
  | "repository"
  | "syncCloudMetadata"
  | "currentSessionRef"
  | "currentAudioMimeTypeRef"
  | "currentLocationRef"
  | "cloudSessionVerifiedRef"
  | "cloudSessionRef"
  | "cloudQueue"
  | "setSessionId"
  | "setSessionSourceLanguage"
  | "setTitle"
  | "applyLoadedRecords"
  | "setRecorderStatus"
  | "transcriptStore"
  | "feedModel"
  | "refreshHistory"
  | "ragEnabledRef"
  | "titleGenerationRef"
  | "setTitleGenerating"
  | "titleRef"
  | "autoTitleRef"
  | "title"
  | "captureRef"
  | "localWriteChainRef"
>

// Open, continue, delete and export history within the current account boundary.
export function useWorkspaceHistory(scope: Dependencies) {
  const {
    ownerGenerationRef,
    repositoryOwnerRef,
    historyLoadRequestRef,
    ownerScopeIsCurrent,
    statusRef,
    setError,
    stop,
    setHistoryOpening,
    settingsRef,
    userRef,
    repository,
    syncCloudMetadata,
    currentSessionRef,
    currentAudioMimeTypeRef,
    currentLocationRef,
    cloudSessionVerifiedRef,
    cloudSessionRef,
    cloudQueue,
    setSessionId,
    setSessionSourceLanguage,
    setTitle,
    applyLoadedRecords,
    setRecorderStatus,
    transcriptStore,
    feedModel,
    refreshHistory,
    ragEnabledRef,
    titleGenerationRef,
    setTitleGenerating,
    titleRef,
    autoTitleRef,
    title,
    captureRef,
    localWriteChainRef,
  } = scope


  const loadHistory = useCallback(async (session: HistorySession) => {
    const ownerGeneration = ownerGenerationRef.current
    const ownerId = repositoryOwnerRef.current
    const loadRequest = ++historyLoadRequestRef.current
    const loadIsCurrent = () => (
      loadRequest === historyLoadRequestRef.current
      && ownerScopeIsCurrent(ownerGeneration, ownerId)
    )
    const assertLoadCurrent = () => {
      if(!loadIsCurrent()) {
        throw new DOMException('Session load owner changed', 'AbortError')
      }
    }
    if(!loadIsCurrent()) return
    if(statusRef.current === 'starting' || statusRef.current === 'stopping') {
      setError(messages().workspace.runtime.sessionBusy)
      return
    }
    if(statusRef.current !== 'idle') {
      const shouldStop = window.confirm(messages().workspace.runtime.confirmLoad)
      if(!shouldStop) return
      await stop()
      assertLoadCurrent()
    }
    // Session open must not blank the history list — report progress on the row.
    const reportOpening = (label: string, percent: number | null = null) => {
      if(!loadIsCurrent()) return
      setHistoryOpening({ sessionId: session.id, label, percent })
    }
    reportOpening(messages().workspace.runtime.opening, null)
    setError(null)
    try {
      let records: StoredSessionRecords
      let loadedTitle = session.title
      let loadedDuration = session.durationSeconds
      let loadedSourceLanguage = settingsRef.current.sourceLanguage

      if(session.location === 'cloud' && userRef.current) {
        reportOpening(messages().workspace.runtime.readingCloud, null)
        const cloud = await getCloudSession(session.id, {
          includeTranscripts: false,
        })
        assertLoadCurrent()
        loadedSourceLanguage = cloud.source_language
        const cloudUpdatedAt = Date.parse(cloud.updated_at) || 0
        const localMetadata = await repository.getSessionMetadata(cloud.id)
        assertLoadCurrent()

        // Prefer local cache whenever we already have content. Full multi-page
        // cloud downloads only run when the cache is missing, pending, or known
        // to be behind the cloud revision.
        const localCacheUsable = Boolean(
          localMetadata
          && !localMetadata.cloudSessionPending
          && localMetadata.transcriptCount > 0,
        )
        const localCacheFresh = Boolean(
          localCacheUsable
          && localMetadata
          && (
            (
              localMetadata.cloudContentUpdatedAt !== undefined
              && localMetadata.cloudContentUpdatedAt >= cloudUpdatedAt
            )
            // Local writes are still at least as new as the cloud metadata
            // (typical after a just-finished recording on this device).
            || localMetadata.updatedAt >= cloudUpdatedAt
          ),
        )
        if(localCacheFresh && localMetadata) {
          reportOpening(messages().workspace.runtime.readingCache, null)
          records = await canonicalizeLocalSession(
            repository,
            cloud.id,
            localMetadata.targetLanguage ?? cloud.target_language,
          )
          assertLoadCurrent()
          const localWins = localMetadata.updatedAt > cloudUpdatedAt
          loadedTitle = localWins
            ? localMetadata.title || cloud.title
            : cloud.title
          loadedDuration = Math.max(
            cloud.duration_seconds,
            (localMetadata.durationMs ?? 0) / 1_000,
          )
          loadedSourceLanguage =
            localMetadata.sourceLanguage ?? cloud.source_language
          // Stamp the revision we validated so subsequent opens stay on the
          // fast path even if a title/status push advances cloud.updated_at.
          if(
            localMetadata.cloudContentUpdatedAt === undefined
            || localMetadata.cloudContentUpdatedAt < cloudUpdatedAt
          ) {
            await repository.updateSessionMetadata(cloud.id, {
              cloudContentUpdatedAt: Math.max(
                cloudUpdatedAt,
                localMetadata.cloudContentUpdatedAt ?? 0,
              ),
            }, { touch: false })
            assertLoadCurrent()
          }
          if(localWins) {
            const desiredStatus = localMetadata.status === 'active'
              ? 'active'
              : 'completed'
            if(
              (localMetadata.title && localMetadata.title !== cloud.title)
              || desiredStatus !== cloud.status
              || (localMetadata.durationMs ?? 0) > cloud.duration_seconds * 1_000
            ) {
              void syncCloudMetadata(cloud.id, {
                ...(localMetadata.title ? { title: localMetadata.title } : {}),
                status: desiredStatus,
                duration_seconds: Math.round(
                  Math.max(
                    localMetadata.durationMs ?? 0,
                    cloud.duration_seconds * 1_000,
                  ) / 1_000,
                ),
              }).catch(() => undefined)
            }
          }
        } else {
          reportOpening(
            localMetadata?.transcriptCount
              ? messages().workspace.runtime.syncingTranscript
              : messages().workspace.runtime.downloadingTranscript,
            2,
          )
          const localRecords = localMetadata
            ? await canonicalizeLocalSession(
              repository,
              cloud.id,
              cloud.target_language,
            )
            : { segments: [], translations: [] }
          assertLoadCurrent()
          // If we already have a usable local cache, paint it immediately so the
          // user can read while remaining cloud pages download.
          if(localCacheUsable && localMetadata && localRecords.segments.length > 0) {
            const localWinsEarly = localMetadata.updatedAt > cloudUpdatedAt
            const earlyTitle = localWinsEarly
              ? localMetadata.title || cloud.title
              : cloud.title
            const earlyDuration = Math.max(
              cloud.duration_seconds,
              (localMetadata.durationMs ?? 0) / 1_000,
            )
            currentSessionRef.current = cloud.id
            currentAudioMimeTypeRef.current = localMetadata.audioMimeType || 'audio/webm'
            currentLocationRef.current = 'cloud'
            cloudSessionVerifiedRef.current = true
            cloudSessionRef.current = null
            cloudQueue.setSession(null)
            setSessionId(cloud.id)
            setSessionSourceLanguage(
              localMetadata.sourceLanguage ?? cloud.source_language,
            )
            setTitle(earlyTitle)
            applyLoadedRecords(localRecords, earlyDuration, cloud.id)
            setRecorderStatus('idle')
            reportOpening(messages().workspace.runtime.openedSyncing, 5)
          }
          const segments: TranscriptSegment[] = []
          const translations: TranslationSegment[] = []
          const cloudByClientId = new Map<string, Pick<
            CloudTranscript,
            'text' | 'translation' | 'translation_group_id'
          > & { segment: TranscriptSegment }>()
          // Every covered atom carries the group id, while only the anchor
          // stores the paragraph text. Keep the group accumulator across page
          // boundaries so a long paragraph is reconstructed exactly once.
          const translationGroups = new Map<string, {
            anchorSegment?: TranscriptSegment
            firstIndex: number
            firstSegment: TranscriptSegment
            lastEndTime: number
            receivedAt: number
            speaker: string
            text?: string
          }>()
          let transcriptIndex = 0
          let cursor: { start_time: number; id: string } | null = null
          let pagesLoaded = 0
          // Progress without a total: asymptotic curve that approaches ~90% while
          // pages keep arriving, then jumps to 100% after merge/write.
          const pageProgress = (page: number) => Math.min(88, 8 + page * 12)

          for(;;) {
            let page: Awaited<ReturnType<typeof getSessionTranscriptsPage>> | null = null
            for(let attempt = 0;attempt < 3;attempt += 1) {
              try {
                page = await getSessionTranscriptsPage(cloud.id, {
                  limit: 500,
                  after: cursor,
                })
                break
              } catch(reason) {
                const retryable = !(reason instanceof ApiRequestError)
                  || reason.status === 408
                  || reason.status === 425
                  || reason.status === 429
                  || reason.status >= 500
                if(!retryable || attempt === 2) throw reason
                await new Promise<void>((resolve) => {
                  window.setTimeout(resolve, 400 * (2 ** attempt))
                })
                assertLoadCurrent()
              }
            }
            if(!page) throw new Error(messages().workspace.runtime.pageFailed)
            assertLoadCurrent()
            pagesLoaded += 1
            reportOpening(
              messages().workspace.runtime.downloadingPage(pagesLoaded),
              pageProgress(pagesLoaded),
            )

            const pageTranscripts = Array.isArray(page.transcripts)
              ? page.transcripts
              : []
            for(const transcript of pageTranscripts) {
              const index = transcriptIndex
              transcriptIndex += 1
              const transcriptText = transcript.text.trim()
              if(transcript.is_partial || !transcriptText) continue

              const clientSegmentId = transcript.client_segment_id || transcript.id
              let segment = cloudByClientId.get(clientSegmentId)?.segment
              if(!segment) {
                const startTime = transcript.start_time
                segment = Object.freeze({
                  id: clientSegmentId,
                  sequence: segments.length,
                  speaker: transcript.speaker.trim() || 'Speaker',
                  text: transcriptText,
                  status: 'final',
                  startTime,
                  endTime: Math.max(startTime, transcript.end_time ?? startTime),
                  receivedAt: Date.parse(transcript.created_at) || Date.now(),
                  source: 'cloud',
                })
                segments.push(segment)
              }
              cloudByClientId.set(clientSegmentId, {
                segment,
                text: transcript.text,
                ...(transcript.translation
                  ? { translation: transcript.translation }
                  : {}),
                ...(transcript.translation_group_id
                  ? { translation_group_id: transcript.translation_group_id }
                  : {}),
              })

              const text = transcript.translation?.trim()
              const persistedGroupId = transcript.translation_group_id?.trim()
              if(!persistedGroupId && !text) continue
              const groupId = persistedGroupId || `single:${clientSegmentId}`
              const receivedAt = Date.parse(transcript.updated_at) || Date.now()
              const existing = translationGroups.get(groupId)
              if(!existing) {
                translationGroups.set(groupId, {
                  ...(text ? { anchorSegment: segment, text } : {}),
                  firstIndex: index,
                  firstSegment: segment,
                  lastEndTime: segment.endTime,
                  receivedAt,
                  speaker: transcript.speaker,
                })
                continue
              }
              existing.lastEndTime = Math.max(existing.lastEndTime, segment.endTime)
              if(text && (!existing.text || receivedAt >= existing.receivedAt)) {
                existing.anchorSegment = segment
                existing.receivedAt = receivedAt
                existing.text = text
                existing.speaker = transcript.speaker
              }
            }

            if(!page.has_more) break
            const nextCursor = page.next_cursor
            const lastTranscript = pageTranscripts.at(-1)
            const advances = Boolean(
              nextCursor
              && (
                cursor === null
                || nextCursor.start_time > cursor.start_time
                || (
                  nextCursor.start_time === cursor.start_time
                  && nextCursor.id > cursor.id
                )
              ),
            )
            if(
              !nextCursor
              || !lastTranscript
              || !Number.isFinite(nextCursor.start_time)
              || nextCursor.start_time < 0
              || nextCursor.start_time !== lastTranscript.start_time
              || nextCursor.id !== lastTranscript.id
              || !advances
            ) {
              throw new Error(messages().workspace.runtime.cursorInvalid)
            }
            cursor = nextCursor
            // Let input, paint and cancellation handlers run between pages.
            await new Promise<void>((resolve) => window.setTimeout(resolve, 0))
            assertLoadCurrent()
          }

          for(const [groupId, group] of [...translationGroups.entries()]
            .sort((left, right) => left[1].firstIndex - right[1].firstIndex)) {
            if(!group.text) continue
            const anchor = group.anchorSegment ?? group.firstSegment
            const translationInput = {
              segmentId: anchor.id,
              speaker: group.speaker.trim() || 'Speaker',
              language: cloud.target_language.trim().toLowerCase(),
              text: group.text,
              startTime: group.firstSegment.startTime,
              endTime: Math.max(
                group.firstSegment.startTime,
                group.lastEndTime,
              ),
            }
            const translation: TranslationSegment = Object.freeze({
              ...translationInput,
              id: groupId.startsWith('single:')
                ? createStableTranslationId(translationInput)
                : groupId,
              sequence: translations.length,
              status: 'final',
              receivedAt: group.receivedAt,
              source: 'cloud',
            })
            translations.push(translation)
          }
          const mergedRecords = mergeSessionRecords(
            localRecords,
            { segments, translations },
          )
          records = mergedRecords
          const cloudDurationMs = cloud.duration_seconds * 1_000
          const localWins = Boolean(
            localMetadata && localMetadata.updatedAt > cloudUpdatedAt,
          )
          loadedTitle = localWins
            ? localMetadata?.title || cloud.title
            : cloud.title
          loadedDuration = Math.max(
            cloud.duration_seconds,
            (localMetadata?.durationMs ?? 0) / 1_000,
          )
          const mergedStatus = localWins
            ? localMetadata?.status ?? 'active'
            : cloud.status === 'active' || cloud.status === 'paused'
              ? 'active'
              : 'completed'
          await repository.ensureSession(cloud.id, {
            createdAt: Date.parse(cloud.created_at) || Date.now(),
            origin: 'cloud',
            sourceLanguage: cloud.source_language,
            targetLanguage: cloud.target_language,
            title: loadedTitle,
            status: mergedStatus,
            durationMs: Math.max(localMetadata?.durationMs ?? 0, cloudDurationMs),
          })
          assertLoadCurrent()
          if(!localMetadata || mergedRecords.addedSegments > 0) {
            reportOpening(messages().workspace.runtime.writingTranscript, 90)
            for(
              let offset = localRecords.segments.length;
              offset < mergedRecords.segments.length;
              offset += 500
            ) {
              await repository.writeTranscriptRecords(
                cloud.id,
                mergedRecords.segments
                  .slice(offset, offset + 500)
                  .map((segment) => ({
                    sequence: segment.sequence,
                    recordId: segment.id,
                    data: segment,
                  })),
              )
              assertLoadCurrent()
              await new Promise<void>((resolve) => window.setTimeout(resolve, 0))
            }
          }
          if(!localMetadata || mergedRecords.addedTranslations > 0) {
            for(
              let offset = localRecords.translations.length;
              offset < mergedRecords.translations.length;
              offset += 500
            ) {
              await repository.writeTranslationRecords(
                cloud.id,
                mergedRecords.translations
                  .slice(offset, offset + 500)
                  .map((translation) => ({
                    sequence: translation.sequence,
                    recordId: translation.id,
                    data: translation,
                  })),
              )
              assertLoadCurrent()
              await new Promise<void>((resolve) => window.setTimeout(resolve, 0))
            }
          }
          reportOpening(messages().workspace.runtime.writingCache, 92)
          await repository.updateSessionMetadata(cloud.id, {
            cloudSessionPending: false,
            cloudContentUpdatedAt: cloudUpdatedAt,
            sourceLanguage: cloud.source_language,
            targetLanguage: cloud.target_language,
            title: loadedTitle,
            durationMs: Math.max(localMetadata?.durationMs ?? 0, cloudDurationMs),
            status: mergedStatus,
          }, { touch: false })
          assertLoadCurrent()
          if(localWins && localMetadata) {
            const desiredStatus = localMetadata.status === 'active'
              ? 'active'
              : 'completed'
            if(
              (localMetadata.title && localMetadata.title !== cloud.title)
              || desiredStatus !== cloud.status
              || (localMetadata.durationMs ?? 0) > cloudDurationMs
            ) {
              void syncCloudMetadata(cloud.id, {
                ...(localMetadata.title ? { title: localMetadata.title } : {}),
                status: desiredStatus,
                duration_seconds: Math.round(
                  Math.max(localMetadata.durationMs ?? 0, cloudDurationMs) / 1_000,
                ),
              }).catch(() => undefined)
            }
          }

          // A page reload can lose the in-memory write-behind queue, but never
          // the local records. Requeue only local records missing or stale in the
          // cloud snapshot; the server upserts by client_segment_id.
          if(localRecords.segments.length > 0) {
            reportOpening(messages().workspace.runtime.checkingUnsynced, 96)
            const localTranslationBySegment = new Map<string, {
              groupId: string
              isAnchor: boolean
              translation: TranslationSegment
            }>()
            const localSegmentIndex = new Map(
              localRecords.segments.map((segment, index) => [segment.id, index]),
            )
            for(const translation of localRecords.translations) {
              if(!translation.segmentId) continue
              const anchorIndex = localSegmentIndex.get(translation.segmentId)
              if(anchorIndex === undefined) continue
              for(
                let index = anchorIndex;
                index < localRecords.segments.length;
                index += 1
              ) {
                const segment = localRecords.segments[index]
                if(!segment || segment.startTime > translation.endTime + 0.3) break
                if(
                  segment.speaker === translation.speaker
                  && segment.startTime >= translation.startTime - 0.3
                  && segment.endTime <= translation.endTime + 0.3
                ) {
                  localTranslationBySegment.set(segment.id, {
                    groupId: translation.id,
                    isAnchor: segment.id === translation.segmentId,
                    translation,
                  })
                }
              }
            }
            const reconciliation: TranscriptInput[] = []
            for(const segment of localRecords.segments) {
              const remote = cloudByClientId.get(segment.id)
              const translationMatch = localTranslationBySegment.get(segment.id)
              const translation = translationMatch?.translation
              if(
                remote
                && remote.text === segment.text
                && (
                  !translation
                  || (
                    remote.translation_group_id === translationMatch.groupId
                    && (
                      !translationMatch.isAnchor
                      || remote.translation === translation.text
                    )
                  )
                )
              ) {
                continue
              }
              reconciliation.push({
                client_segment_id: segment.id,
                ...(translationMatch
                  ? { translation_group_id: translationMatch.groupId }
                  : {}),
                speaker: segment.speaker,
                text: segment.text,
                ...(translation && translationMatch?.isAnchor
                  ? { translation: translation.text }
                  : {}),
                start_time: segment.startTime,
                end_time: segment.endTime,
                status: translation ? 'translated' : 'confirmed',
                is_partial: false,
              })
            }
            if(reconciliation.length > 0) {
              const inputById = new Map(
                reconciliation.map((input) => [input.client_segment_id, input]),
              )
              for(let offset = 0;offset < reconciliation.length;offset += 500) {
                const durableRecords = await repository.upsertCloudTranscriptOutboxBatch(
                  cloud.id,
                  reconciliation
                    .slice(offset, offset + 500)
                    .map((input) => ({
                      clientSegmentId: input.client_segment_id,
                      payload: input,
                    })),
                )
                assertLoadCurrent()
                cloudQueue.restore(durableRecords.flatMap((record) => {
                  const input = inputById.get(record.clientSegmentId)
                  return input
                    ? [{
                      ownerId: record.ownerId,
                      sessionId: record.sessionId,
                      input,
                      durableVersion: record.updatedAt,
                    }]
                    : []
                }))
              }
            }
          }
          reportOpening(messages().workspace.runtime.organizing, 99)
        } // end full cloud sync (non-fast-path)
      } else {
        reportOpening(messages().workspace.runtime.readingLocal, null)
        const metadata = await repository.getSessionMetadata(session.id)
        assertLoadCurrent()
        loadedSourceLanguage =
          metadata?.sourceLanguage ?? settingsRef.current.sourceLanguage
        records = await canonicalizeLocalSession(
          repository,
          session.id,
          metadata?.targetLanguage ?? settingsRef.current.targetLanguage,
        )
        assertLoadCurrent()
        loadedTitle = metadata?.title || loadedTitle
        loadedDuration = (metadata?.durationMs ?? loadedDuration * 1_000) / 1_000
      }

      reportOpening(messages().workspace.runtime.rendering, 100)
      const loadedMetadata = await repository.getSessionMetadata(session.id)
      assertLoadCurrent()
      currentSessionRef.current = session.id
      currentAudioMimeTypeRef.current = loadedMetadata?.audioMimeType || 'audio/webm'
      currentLocationRef.current = session.location
      cloudSessionVerifiedRef.current = session.location === 'cloud'
      cloudSessionRef.current = null
      cloudQueue.setSession(null)
      setSessionId(session.id)
      setSessionSourceLanguage(
        loadedMetadata?.sourceLanguage ?? loadedSourceLanguage,
      )
      setTitle(loadedTitle)
      applyLoadedRecords(records, loadedDuration, session.id)
      setRecorderStatus('idle')
    } catch(reason) {
      if(!loadIsCurrent()) return
      if(session.location === 'cloud') {
        try {
          const metadata = await repository.getSessionMetadata(session.id)
          assertLoadCurrent()
          if(metadata) {
            reportOpening(messages().workspace.runtime.cloudUnavailableCache, null)
            const cachedRecords = await canonicalizeLocalSession(
              repository,
              session.id,
              metadata.targetLanguage ?? settingsRef.current.targetLanguage,
            )
            assertLoadCurrent()
            const cachedDuration = (metadata.durationMs ?? 0) / 1_000
            currentSessionRef.current = session.id
            currentAudioMimeTypeRef.current = metadata.audioMimeType || 'audio/webm'
            currentLocationRef.current = 'cloud'
            cloudSessionVerifiedRef.current = false
            cloudSessionRef.current = null
            cloudQueue.setSession(null)
            setSessionId(session.id)
            setSessionSourceLanguage(
              metadata.sourceLanguage ?? settingsRef.current.sourceLanguage,
            )
            setTitle(metadata.title || session.title)
            applyLoadedRecords(cachedRecords, cachedDuration, session.id)
            setRecorderStatus('idle')
            setError(messages().workspace.runtime.openedCache(
              reason instanceof Error ? reason.message : String(reason),
            ))
            return
          }
        } catch {
          // Report the original cloud failure below when no usable cache exists.
        }
      }
      setError(messages().workspace.runtime.loadFailed(reason instanceof Error ? reason.message : String(reason)))
    } finally {
      if(loadIsCurrent()) setHistoryOpening(null)
    }
  }, [applyLoadedRecords, cloudQueue, cloudSessionRef, cloudSessionVerifiedRef, currentAudioMimeTypeRef, currentLocationRef, currentSessionRef, historyLoadRequestRef, ownerGenerationRef, ownerScopeIsCurrent, repository, repositoryOwnerRef, setError, setHistoryOpening, setRecorderStatus, setSessionId, setSessionSourceLanguage, setTitle, settingsRef, statusRef, stop, syncCloudMetadata, userRef])

  const deleteHistory = useCallback(async (session: HistorySession) => {
    const ownerGeneration = ownerGenerationRef.current
    const ownerId = repositoryOwnerRef.current
    const deleteIsCurrent = () => (
      ownerScopeIsCurrent(ownerGeneration, ownerId)
    )
    if(!deleteIsCurrent()) return
    if(session.id === currentSessionRef.current && statusRef.current !== 'idle') {
      setError(messages().workspace.runtime.cannotDeleteRecording)
      return
    }
    try {
      const deletingUser = userRef.current
      if(
        session.location === 'cloud'
        && deletingUser
        && deletingUser.id === ownerId
      ) {
        await deleteCloudSession(session.id)
        cloudQueue.discardSession(deletingUser.id, session.id)
      }
      if(!deleteIsCurrent()) return
      await repository.deleteSession(session.id)
      if(!deleteIsCurrent()) return
      try {
        localStorage.removeItem(chatHistoryKey(ownerId, session.id))
        localStorage.removeItem(legacyChatHistoryKey(session.id))
      } catch {
        // The session itself is deleted even if browser storage is unavailable.
      }
      if(session.id === currentSessionRef.current) {
        lexReset(session.id)
        currentSessionRef.current = ''
        currentAudioMimeTypeRef.current = 'audio/webm'
        setSessionId('')
        setSessionSourceLanguage(settingsRef.current.sourceLanguage)
        setTitle(defaultSessionTitle())
        transcriptStore.reset()
        feedModel.reset()
        applyLoadedRecords({ segments: [], translations: [] }, 0)
      }
      await refreshHistory()
    } catch(reason) {
      if(deleteIsCurrent()) {
        setError(messages().workspace.runtime.deleteFailed(reason instanceof Error ? reason.message : String(reason)))
      }
    }
  }, [applyLoadedRecords, cloudQueue, currentAudioMimeTypeRef, currentSessionRef, feedModel, ownerGenerationRef, ownerScopeIsCurrent, refreshHistory, repository, repositoryOwnerRef, setError, setSessionId, setSessionSourceLanguage, setTitle, settingsRef, statusRef, transcriptStore, userRef])

  const endHistorySession = useCallback(async (session: HistorySession) => {
    if(session.location !== 'cloud') return
    if(session.id === currentSessionRef.current && statusRef.current !== 'idle') {
      setError(messages().workspace.runtime.currentRecording)
      return
    }
    try {
      // Completing the cloud session also cuts any live transcription stream
      // still attached to it on another device.
      await updateCloudSession(session.id, { status: 'completed' })
      try {
        const metadata = await repository.getSessionMetadata(session.id)
        if(metadata && metadata.status === 'active') {
          await repository.completeSession(session.id)
        }
      } catch {
        // The cloud row is the source of truth; a missing local cache is fine.
      }
      await refreshHistory()
    } catch(reason) {
      setError(
        messages().workspace.runtime.endFailed(reason instanceof Error ? reason.message : String(reason)),
      )
    }
  }, [currentSessionRef, refreshHistory, repository, setError, statusRef])

  const uploadHistorySessionToCloud = useCallback(async (
    session: HistorySession,
  ) => {
    if(session.location !== 'local') return
    if(!userRef.current) {
      setError(messages().workspace.runtime.loginToUpload)
      return
    }
    if(session.id === currentSessionRef.current && statusRef.current !== 'idle') {
      setError(messages().workspace.runtime.stopToUpload)
      return
    }
    const ownerGeneration = ownerGenerationRef.current
    const ownerId = repositoryOwnerRef.current
    const uploadIsCurrent = () => ownerScopeIsCurrent(ownerGeneration, ownerId)
    if(!uploadIsCurrent()) return
    try {
      const metadata = await repository.getSessionMetadata(session.id)
      if(!metadata) {
        setError(messages().workspace.runtime.uploadMissing)
        return
      }
      if(metadata.origin === 'cloud') {
        await refreshHistory()
        return
      }
      // Create (or adopt) the cloud session under the same deterministic id,
      // so a retried upload continues instead of duplicating.
      await createCloudSession({
        client_session_id: session.id,
        title: metadata.title || session.title || defaultSessionTitle(),
        source_language:
          metadata.sourceLanguage || settingsRef.current.sourceLanguage,
        target_language:
          metadata.targetLanguage || settingsRef.current.targetLanguage,
      })
      // Fold local translations onto their transcripts, then upload in the
      // largest batches the server accepts (500 items / 1MB body). A long
      // session used to go up in dozens of tiny 50-item round trips, which
      // made the upload latency-bound; byte-budgeted batches keep the request
      // count minimal without risking the body-size cap.
      const translationBySegment = new Map<string, string>()
      for await(const record of repository.iterateTranslations(session.id)) {
        const translation = record.data
        if(translation.segmentId) {
          translationBySegment.set(translation.segmentId, translation.text)
        }
      }
      const maxBatchItems = 400
      const maxBatchBytes = 600_000
      const batch: TranscriptInput[] = []
      let batchBytes = 0
      const flushBatch = async () => {
        if(batch.length === 0) return
        const items = batch.splice(0, batch.length)
        batchBytes = 0
        await saveTranscriptsBatch(session.id, items)
      }
      for await(const record of repository.iterateTranscripts(session.id)) {
        const segment = record.data
        if(!segment.text.trim()) continue
        const translation = translationBySegment.get(segment.id)
        const input: TranscriptInput = {
          client_segment_id: segment.id,
          speaker: segment.speaker,
          text: segment.text,
          ...(translation ? { translation } : {}),
          start_time: segment.startTime,
          end_time: segment.endTime,
          status: translation ? 'translated' : 'confirmed',
          is_partial: false,
        }
        // CJK inflates to ~3 bytes/char in UTF-8; 120 covers the fixed fields.
        const approxBytes = (segment.text.length + (translation?.length ?? 0)) * 3 + 120
        if(batch.length > 0 && (batchBytes + approxBytes > maxBatchBytes || batch.length >= maxBatchItems)) {
          await flushBatch()
        }
        batch.push(input)
        batchBytes += approxBytes
      }
      await flushBatch()
      const durationSeconds = metadata.durationMs
        ? Math.round(metadata.durationMs / 1_000)
        : session.durationSeconds
      await updateCloudSession(session.id, {
        status: 'completed',
        ...(durationSeconds > 0 ? { duration_seconds: durationSeconds } : {}),
      })
      if(!uploadIsCurrent()) return
      await repository.markSessionCloud(session.id)
      await refreshHistory()
    } catch(reason) {
      if(!uploadIsCurrent()) return
      if(reason instanceof ApiRequestError && reason.status === 409) {
        setError(messages().workspace.runtime.uploadConflict)
        return
      }
      setError(
        messages().workspace.runtime.uploadFailed(reason instanceof Error ? reason.message : String(reason)),
      )
    }
  }, [currentSessionRef, ownerGenerationRef, ownerScopeIsCurrent, refreshHistory, repository, repositoryOwnerRef, setError, settingsRef, statusRef, userRef])

  const persistTitle = useCallback(async (id: string, normalized: string) => {
    if(currentSessionRef.current === id) setTitle(normalized)
    try {
      await repository.updateSessionMetadata(id, { title: normalized })
      if(currentLocationRef.current === 'cloud' && userRef.current) {
        await syncCloudMetadata(id, { title: normalized })
      }
      void refreshHistory()
    } catch(reason) {
      setError(messages().workspace.runtime.titleSaveFailed(reason instanceof Error ? reason.message : String(reason)))
    }
  }, [currentLocationRef, currentSessionRef, refreshHistory, repository, setError, setTitle, syncCloudMetadata, userRef])

  const updateTitle = useCallback(async (nextTitle: string) => {
    const id = currentSessionRef.current
    if(!id || !nextTitle.trim()) return
    await persistTitle(id, nextTitle.trim())
  }, [currentSessionRef, persistTitle])

  const runTitleGeneration = useCallback(async (
    id: string,
    mode: 'auto' | 'manual',
  ): Promise<void> => {
    if(!id) return
    if(!userRef.current || !ragEnabledRef.current) {
      if(mode === 'manual') setError(messages().workspace.runtime.titleNeedsAi)
      return
    }
    const excerpt = buildTitleExcerpt(feedModel.getSnapshot())
    if(!excerpt) {
      if(mode === 'manual') setError(messages().workspace.runtime.titleNeedsText)
      return
    }
    const generation = ++titleGenerationRef.current
    setTitleGenerating(true)
    try {
      const generated = await generateSessionTitle(id, excerpt)
      if(generation !== titleGenerationRef.current) return
      if(!generated) {
        if(mode === 'manual') setError(messages().workspace.runtime.titleEmpty)
        return
      }
      // A manual rename that landed while the request was in flight wins;
      // auto-naming must never clobber something the user typed.
      if(mode === 'auto' && currentSessionRef.current === id
        && !isDefaultSessionTitle(titleRef.current)) return
      await persistTitle(id, generated)
    } catch(reason) {
      if(generation !== titleGenerationRef.current) return
      if(mode === 'manual') {
        setError(messages().workspace.runtime.titleFailed(reason instanceof Error ? reason.message : String(reason)))
      }
    } finally {
      if(generation === titleGenerationRef.current) setTitleGenerating(false)
    }
  }, [currentSessionRef, feedModel, persistTitle, ragEnabledRef, setError, setTitleGenerating, titleGenerationRef, titleRef, userRef])

  const generateTitle = useCallback(
    () => runTitleGeneration(currentSessionRef.current, 'manual'),
    [currentSessionRef, runTitleGeneration],
  )
  autoTitleRef.current = (id: string) => {
    if(!isDefaultSessionTitle(titleRef.current)) return
    void runTitleGeneration(id, 'auto')
  }

  const downloadAudio = useCallback(async () => {
    const id = currentSessionRef.current
    if(!id) {
      setError(messages().workspace.runtime.noDownloadSession)
      return
    }
    try {
      // This must happen before the first await in the click callback.
      // Chromium otherwise drops transient user activation and rejects it.
      const saveRequest = requestCompleteAudioSave(
        title,
        captureRef.current?.mimeType || currentAudioMimeTypeRef.current,
      )
      await captureRef.current?.flushCompressedChunk()
      await localWriteChainRef.current
      const result = await downloadCompleteAudio(repository, id, title, saveRequest)
      if(result === 'empty') setError(messages().workspace.runtime.noAudio)
    } catch(reason) {
      setError(messages().workspace.runtime.audioDownloadFailed(reason instanceof Error ? reason.message : String(reason)))
    }
  }, [captureRef, currentAudioMimeTypeRef, currentSessionRef, localWriteChainRef, repository, setError, title])

  const downloadText = useCallback(async (
    mode: 'original' | 'translation' | 'bilingual',
  ) => {
    const id = currentSessionRef.current
    if(!id) {
      setError(messages().workspace.runtime.noDownloadSession)
      return
    }
    try {
      await localWriteChainRef.current
      await canonicalizeLocalSession(repository, id, settingsRef.current.targetLanguage)
      const downloaded = await downloadSessionText(repository, id, title, mode)
      if(!downloaded) setError(messages().workspace.runtime.noText)
    } catch(reason) {
      setError(messages().workspace.runtime.textDownloadFailed(reason instanceof Error ? reason.message : String(reason)))
    }
  }, [currentSessionRef, localWriteChainRef, repository, setError, settingsRef, title])
  return {
    loadHistory,
    deleteHistory,
    endHistorySession,
    uploadHistorySessionToCloud,
    persistTitle,
    updateTitle,
    runTitleGeneration,
    generateTitle,
    downloadAudio,
    downloadText,
  }
}
export type WorkspaceHistory = ReturnType<typeof useWorkspaceHistory>
