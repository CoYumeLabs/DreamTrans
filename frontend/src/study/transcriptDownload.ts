import { messages } from '../i18n'
import {
  getSessionTranscriptsPage,
  getStoredUser,
  type Transcript,
  type TranscriptPageCursor,
} from '../pro/api/auth'
import type { StudyWeekSession } from '../api'
import type { TranscriptSegment, TranslationSegment } from '../core/transcription/types'
import {
  createSessionTextBlob,
  safeFilename,
  triggerBlobDownload,
  type TextDownloadMode,
} from '../unified/workspace/downloads'

/** Reconstruct paragraph translations across page boundaries before formatting. */
function sessionText(rows: Transcript[], mode: TextDownloadMode): Blob | null {
  const segments: TranscriptSegment[] = []
  const seen = new Set<string>()
  const groups = new Map<string, TranslationSegment>()
  for (const row of rows) {
    if (row.is_partial || row.status === 'partial' || !row.text.trim()) continue
    const id = row.client_segment_id || row.id
    if (seen.has(id)) continue
    seen.add(id)
    const segment: TranscriptSegment = {
      id,
      sequence: segments.length,
      speaker: row.speaker.trim() || 'Speaker',
      text: row.text.trim(),
      status: 'final',
      startTime: row.start_time,
      endTime: Math.max(row.start_time, row.end_time ?? row.start_time),
      receivedAt: Date.parse(row.created_at) || 0,
      source: 'cloud',
    }
    segments.push(segment)
    if (mode === 'original') continue
    const text = row.translation?.trim() || ''
    const groupId = row.translation_group_id?.trim() || `single:${id}`
    if (!text && !row.translation_group_id) continue
    const previous = groups.get(groupId)
    const receivedAt = Date.parse(row.updated_at) || 0
    const replaceText = text && (!previous?.text || receivedAt >= previous.receivedAt)
    groups.set(groupId, {
      id: groupId,
      sequence: previous?.sequence ?? groups.size,
      segmentId: replaceText ? id : previous?.segmentId ?? id,
      speaker: replaceText ? segment.speaker : previous?.speaker ?? segment.speaker,
      language: 'translated',
      text: replaceText ? text : previous?.text ?? '',
      startTime: Math.min(previous?.startTime ?? segment.startTime, segment.startTime),
      endTime: Math.max(previous?.endTime ?? segment.endTime, segment.endTime),
      receivedAt: replaceText ? receivedAt : previous?.receivedAt ?? receivedAt,
      status: 'final',
      source: 'cloud',
    })
  }
  return createSessionTextBlob(segments, [...groups.values()].filter(({ text }) => text), mode)
}

export interface TranscriptDownloadOptions {
  title: string
  sessions: StudyWeekSession[]
  mode: TextDownloadMode
  signal: AbortSignal
  onProgress: (completed: number, total: number) => void
}

/** Fetch every page, then offer one archive only after every session succeeds. */
export async function downloadStudyTranscripts(options: TranscriptDownloadOptions): Promise<void> {
  const copy = messages().study.view.download
  const ownerId = getStoredUser()?.id
  const assertCurrent = () => {
    options.signal.throwIfAborted()
    if (!ownerId || getStoredUser()?.id !== ownerId) {
      throw new Error(copy.accountChanged)
    }
  }
  assertCurrent()
  const sessions = [...new Map(options.sessions.map((session) => [session.id, session])).values()]
  if (!sessions.length) throw new Error(copy.noSessions)
  const { default: JSZip } = await import('jszip')
  assertCurrent()
  const zip = new JSZip()
  const suffix = messages().workspace.runtime.downloads.suffixes[options.mode]
  options.onProgress(0, sessions.length)
  for (const [index, session] of sessions.entries()) {
    assertCurrent()
    const rows: Transcript[] = []
    let cursor: TranscriptPageCursor | null = null
    for (;;) {
      assertCurrent()
      const page = await getSessionTranscriptsPage(session.id, {
        limit: 500,
        after: cursor,
        signal: options.signal,
      })
      assertCurrent()
      rows.push(...page.transcripts)
      if (!page.has_more) break
      const next = page.next_cursor
      const last = page.transcripts.at(-1)
      if (!next || !last || !Number.isFinite(next.start_time) || next.start_time < 0
        || next.start_time !== last.start_time || next.id !== last.id
        || (cursor && (next.start_time < cursor.start_time
          || (next.start_time === cursor.start_time && next.id <= cursor.id)))) {
        throw new Error(messages().workspace.runtime.cursorInvalid)
      }
      cursor = next
    }
    const blob = sessionText(rows, options.mode)
    // Keep one file per session, including sessions without content in this mode.
    const text = blob ? await blob.text() : options.mode === 'translation' ? copy.noTranslation : copy.noTranscript
    assertCurrent()
    const date = /^\d{4}-\d{2}-\d{2}/.exec(session.started_at)?.[0] ?? ''
    const filename = `${String(index + 1).padStart(3, '0')}-${date ? `${date}-` : ''}${safeFilename(session.title)}-${suffix}.txt`
    zip.file(filename, text)
    options.onProgress(index + 1, sessions.length)
  }
  assertCurrent()
  // JSZip update callbacks do not turn thrown errors into promise rejections.
  // Check cancellation after compression so a cancelled archive is discarded.
  const archive = await zip.generateAsync({ type: 'blob', compression: 'DEFLATE' })
  assertCurrent()
  triggerBlobDownload(archive, `${safeFilename(options.title)}-${suffix}.zip`)
}
