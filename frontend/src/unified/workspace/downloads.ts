import type { IndexedDbSessionRepository } from '../../core/session'
import type {
  TranscriptSegment,
  TranslationSegment,
} from '../../core/transcription/types'
import { messages } from '../../i18n'
import { TranscriptFeedModel } from './TranscriptFeedModel'

interface WritableFileTarget {
  write(data: Blob): Promise<void>
  close(): Promise<void>
  abort?(): Promise<void>
}

interface SaveFileHandle {
  createWritable(): Promise<WritableFileTarget>
}

interface SaveFilePickerResult {
  handle?: SaveFileHandle
  reason?: unknown
}

export interface CompleteAudioSaveRequest {
  result: Promise<SaveFilePickerResult>
}

export type AudioDownloadResult = 'saved' | 'empty' | 'cancelled'

interface SaveFilePickerWindow extends Window {
  showSaveFilePicker?: (options: {
    suggestedName: string
    types: Array<{
      description: string
      accept: Record<string, string[]>
    }>
  }) => Promise<SaveFileHandle>
}

export function safeFilename(value: string): string {
  return (
    value
      .trim()
      .replace(/[<>:"/\\|?*]/g, '-')
      .replace(/\p{Cc}/gu, '')
      .replace(/\s+/g, ' ')
      .slice(0, 90)
    || messages().workspace.runtime.downloads.fallbackTitle
  )
}

function audioExtension(mimeType: string | undefined): string {
  if (mimeType?.includes('mp4')) return 'm4a'
  if (mimeType?.includes('ogg')) return 'ogg'
  if (mimeType?.includes('mpeg')) return 'mp3'
  return 'webm'
}

export function triggerBlobDownload(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.style.display = 'none'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1_000)
}

function isUserCancellation(reason: unknown): boolean {
  return reason instanceof DOMException && reason.name === 'AbortError'
}

/**
 * Calls the file picker synchronously from the click handler so Chromium keeps
 * transient user activation. The settled wrapper is attached immediately to
 * avoid an unhandled rejection while the recorder and IndexedDB writes flush.
 */
export function requestCompleteAudioSave(
  title: string,
  mimeType = 'audio/webm',
): CompleteAudioSaveRequest | null {
  const pickerWindow = window as SaveFilePickerWindow
  const picker = pickerWindow.showSaveFilePicker
  if (!picker) return null

  const pickerMimeType = mimeType.split(';', 1)[0] || 'audio/webm'
  const filename = `${safeFilename(title)}.${audioExtension(mimeType)}`
  const result = picker.call(pickerWindow, {
    suggestedName: filename,
    types: [{
      description: messages().workspace.runtime.downloads.localRecording,
      accept: { [pickerMimeType]: [`.${audioExtension(mimeType)}`] },
    }],
  }).then(
    (handle): SaveFilePickerResult => ({ handle }),
    (reason): SaveFilePickerResult => ({ reason }),
  )
  return { result }
}

/**
 * Downloads the locally persisted recording without rebuilding it during autosave.
 * Chromium can stream IndexedDB chunks straight into the chosen file; other
 * browsers assemble one Blob only for this explicit user action.
 */
export async function downloadCompleteAudio(
  repository: IndexedDbSessionRepository<TranscriptSegment, TranslationSegment>,
  sessionId: string,
  title: string,
  saveRequest: CompleteAudioSaveRequest | null = null,
): Promise<AudioDownloadResult> {
  const metadata = await repository.getSessionMetadata(sessionId)
  if (!metadata || metadata.audioChunkCount === 0) return 'empty'
  const mimeType = metadata.audioMimeType || 'audio/webm'
  const filename = `${safeFilename(title)}${
    metadata.localAudioIncomplete ? messages().workspace.runtime.downloads.incompleteSuffix : ''
  }.${audioExtension(mimeType)}`
  const warnIfIncomplete = () => {
    if (!metadata.localAudioIncomplete) return
    window.alert(
      messages().workspace.runtime.downloads.incompleteWarning,
    )
  }

  if (saveRequest) {
    const pickerResult = await saveRequest.result
    if (pickerResult.reason && isUserCancellation(pickerResult.reason)) {
      return 'cancelled'
    }
    // If invoking the picker itself failed (unsupported MIME declaration,
    // permissions, etc.), retain a working explicit-download fallback.
    if (pickerResult.handle) {
      warnIfIncomplete()
      let writable: WritableFileTarget | null = null
      try {
        writable = await pickerResult.handle.createWritable()
        for await (const chunk of repository.iterateAudioChunks(sessionId, 25)) {
          await writable.write(chunk.blob)
        }
        await writable.close()
        return 'saved'
      } catch (reason) {
        await writable?.abort?.().catch(() => undefined)
        if (isUserCancellation(reason)) return 'cancelled'
        throw reason
      }
    }
  }

  warnIfIncomplete()
  const blob = await repository.getCompleteAudioBlob(sessionId, mimeType)
  if (!blob) return 'empty'
  triggerBlobDownload(blob, filename)
  return 'saved'
}

export type TextDownloadMode = 'original' | 'translation' | 'bilingual'

function formatTimestamp(seconds: number): string {
  const safe = Math.max(0, Math.floor(seconds))
  const hours = Math.floor(safe / 3_600)
  const minutes = Math.floor((safe % 3_600) / 60)
  const remainder = safe % 60
  return [
    String(hours).padStart(2, '0'),
    String(minutes).padStart(2, '0'),
    String(remainder).padStart(2, '0'),
  ].join(':')
}

interface ExportEntry {
  startTime: number
  speaker: string
  original?: string
  translation?: string
}

/**
 * Exports use the same aggregation as the on-screen feed, so a sentence that
 * renders as one card is written as one block instead of one block per
 * provider micro-final.
 */
export async function downloadSessionText(
  repository: IndexedDbSessionRepository<TranscriptSegment, TranslationSegment>,
  sessionId: string,
  title: string,
  mode: TextDownloadMode,
): Promise<boolean> {
  const segments: TranscriptSegment[] = []
  const translations: TranslationSegment[] = []
  const orphanTranslations: TranslationSegment[] = []

  for await (const record of repository.iterateTranscripts(sessionId, 500)) {
    segments.push(record.data)
  }
  if (mode !== 'original') {
    for await (const record of repository.iterateTranslations(sessionId, 500)) {
      if (record.data.segmentId) translations.push(record.data)
      else orphanTranslations.push(record.data)
    }
  }

  const blob = createSessionTextBlob(segments, [...translations, ...orphanTranslations], mode)
  if (!blob) return false
  const suffixes = messages().workspace.runtime.downloads.suffixes
  const suffix = mode === 'bilingual'
    ? suffixes.bilingual
    : mode === 'translation' ? suffixes.translation : suffixes.original
  triggerBlobDownload(blob, `${safeFilename(title)}-${suffix}.txt`)
  return true
}

export function createSessionTextBlob(
  segments: TranscriptSegment[],
  translations: TranslationSegment[],
  mode: TextDownloadMode,
): Blob | null {
  const model = new TranscriptFeedModel({
    sourceLanguage: '',
    targetLanguage: '',
    translationEnabled: mode !== 'original',
  })
  model.hydrate(segments, translations.filter((translation) => translation.segmentId))

  const entries: ExportEntry[] = []
  for (const item of model.getSnapshot().items) {
    entries.push({
      startTime: item.startTime ?? 0,
      speaker: item.speaker,
      ...(item.original?.text ? { original: item.original.text } : {}),
      ...(item.translation?.text ? { translation: item.translation.text } : {}),
    })
  }
  // Translations that never linked to a transcript still belong in the
  // translation-bearing exports, ordered into the same timeline.
  for (const orphan of translations.filter((translation) => !translation.segmentId)) {
    entries.push({
      startTime: orphan.startTime,
      speaker: orphan.speaker,
      translation: orphan.text,
    })
  }
  entries.sort((left, right) => left.startTime - right.startTime)

  const parts: BlobPart[] = []
  for (const entry of entries) {
    if (mode === 'translation' && !entry.translation) continue
    if (mode === 'original' && !entry.original) continue
    parts.push(`[${formatTimestamp(entry.startTime)}] ${entry.speaker}\n`)
    if (mode !== 'translation' && entry.original) parts.push(`${entry.original}\n`)
    if (mode !== 'original' && entry.translation) parts.push(`${entry.translation}\n`)
    parts.push('\n')
  }

  return parts.length ? new Blob(parts, { type: 'text/plain;charset=utf-8' }) : null
}
