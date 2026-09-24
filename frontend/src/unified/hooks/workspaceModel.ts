import {
  type AccountBalance
} from '../../api'
import { IndexedDbSessionRepository } from '../../core/session'
import {
  createStableTranscriptId,
  createStableTranslationId,
  SpeechmaticsProxyClient,
  TranscriptStore,
  type TranscriptSegment,
  type TranslationSegment
} from '../../core/transcription'
import { messages } from '../../i18n'
import {
  ApiRequestError,
  updateSession as updateCloudSession,
  type TranscriptInput,
  type User
} from '../../pro/api/auth'
import type {
  HistoryOpenProgress,
  HistorySession,
} from '../components/HistoryPanel'
import type { RecorderStatus } from '../components/RecorderBar'
import {
  type StoredSessionRecords
} from '../workspace/mergeSessionRecords'
import {
  TranscriptFeedModel,
  type TranscriptFeedModelSnapshot,
} from '../workspace/TranscriptFeedModel'
import type { WorkspaceStats } from '../WorkspaceShell'
import type { UnifiedSettings } from './useUnifiedSettings'


export const ANONYMOUS_TOKEN_SENTINEL = '__dreamtrans_anonymous__'
export const backendURL = import.meta.env.VITE_BACKEND_URL || 'http://localhost:8080'

export function resolveTranslateProxyUrl(backendUrl: string): string {
  if(backendUrl === '/') {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    return `${protocol}//${window.location.host}/ws/translate`
  }
  const base = backendUrl
    .replace(/^http:\/\//i, 'ws://')
    .replace(/^https:\/\//i, 'wss://')
    .replace(/\/+$/, '')
  return base.endsWith('/ws/translate') ? base : `${base}/ws/translate`
}

export const commonWords = new Set([
  'a', 'an', 'and', 'are', 'as', 'at', 'be', 'but', 'by', 'for', 'from',
  'had', 'has', 'have', 'he', 'her', 'his', 'i', 'if', 'in', 'is', 'it',
  'its', 'me', 'my', 'not', 'of', 'on', 'or', 'our', 'she', 'so', 'that',
  'the', 'their', 'them', 'there', 'they', 'this', 'to', 'was', 'we', 'were',
  'will', 'with', 'you', 'your',
  '一个', '一些', '这个', '那个', '然后', '就是', '可以', '我们', '你们',
  '他们', '因为', '所以', '但是', '还是', '没有', '已经', '现在', '什么',
])

export function formatDiagSeconds(seconds: number): string {
  if(!Number.isFinite(seconds) || seconds < 0) return '0s'
  if(seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`
  const minutes = Math.floor(seconds / 60)
  const rest = Math.floor(seconds % 60)
  return `${minutes}m${rest.toString().padStart(2, '0')}s`
}

export function formatDiagBytes(bytes: number): string {
  if(!Number.isFinite(bytes) || bytes <= 0) return '0'
  if(bytes < 1024) return `${Math.round(bytes)}B`
  if(bytes < 1024 * 1024) return `${Math.ceil(bytes / 1024)}KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)}MB`
}

export function formatDiagMs(ms: number | null | undefined): string {
  if(ms === null || ms === undefined || !Number.isFinite(ms)) return '—'
  if(ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

export function formatLagSample(
  lastMs: number | null,
  avgMs: number | null,
  maxMs: number,
  samples: number,
): string {
  const r = messages().workspace.runtime
  if(samples <= 0 || lastMs === null) return r.noSamples
  const parts = [r.recent(formatDiagMs(lastMs))]
  if(avgMs !== null) parts.push(r.average(formatDiagMs(avgMs)))
  if(maxMs > 0) parts.push(r.peak(formatDiagMs(maxMs)))
  parts.push(`n=${samples}`)
  return parts.join(' · ')
}

export function buildTransportDiagnostics(
  audio: ReturnType<SpeechmaticsProxyClient['getDiagnostics']>,
  ai: { pendingChunks: number; bufferedChars: number },
): TransportDiagnostics {
  const r = messages().workspace.runtime
  const outboundQueueMs = Math.max(0, audio.outboundQueueMs)
  const finalBehindMs = Math.max(0, audio.finalBehindMs)
  const partialBehindMs = audio.partialBehindMs
  const sentAudioSeconds = audio.bytesPerSecond > 0
    ? audio.sentAudioBytes / audio.bytesPerSecond
    : 0
  const dropped = audio.droppedAudioBytes
  const partialSlow = (audio.avgPartialLagMs ?? audio.lastPartialLagMs ?? 0) >= 900
    || (partialBehindMs !== null && partialBehindMs >= 1_200)
  const finalSlow = (audio.avgFinalLagMs ?? audio.lastFinalLagMs ?? 0) >= 2_500
    || finalBehindMs >= 2_500

  let tone: TransportDiagnostics['tone'] = 'ok'
  if(dropped > 0 || outboundQueueMs >= 800 || ai.pendingChunks >= 4 || partialSlow) {
    tone = 'bad'
  } else if(
    outboundQueueMs >= 200
    || finalSlow
    || ai.pendingChunks >= 2
  ) {
    tone = 'warn'
  }

  const partialLive = audio.hasActivePartial && partialBehindMs !== null
    ? r.liveBehind(formatDiagMs(partialBehindMs))
    : audio.hasActivePartial
      ? r.partialPending
      : r.noPartial
  const partialSample = formatLagSample(
    audio.lastPartialLagMs,
    audio.avgPartialLagMs,
    audio.maxPartialLagMs,
    audio.partialSampleCount,
  )
  const finalSample = formatLagSample(
    audio.lastFinalLagMs,
    audio.avgFinalLagMs,
    audio.maxFinalLagMs,
    audio.finalSampleCount,
  )

  const rows: TransportDiagRow[] = [
    {
      label: r.labels.queue,
      value: formatDiagMs(outboundQueueMs),
      note: outboundQueueMs >= 200 ? r.networkMain : r.normal,
    },
    {
      label: r.labels.partial,
      value: partialLive,
      note: partialSample,
    },
    {
      label: r.labels.final,
      value: r.liveBehind(formatDiagMs(finalBehindMs)),
      note: finalSample,
    },
    {
      label: r.labels.sent,
      value: formatDiagSeconds(sentAudioSeconds),
      note: audio.lastPartialAgeMs !== null
        ? r.lastPartialFinal(formatDiagMs(audio.lastPartialAgeMs), formatDiagMs(audio.lastFinalAgeMs))
        : r.lastFinal(formatDiagMs(audio.lastFinalAgeMs)),
    },
  ]
  if(audio.connectionEndpoint) {
    rows.unshift({
      label: r.labels.connection,
      value: audio.connectionEndpoint,
      note: r.connectionCount(audio.connectionCount),
    })
  }
  if(dropped > 0) {
    rows.push({
      label: r.labels.dropped,
      value: formatDiagBytes(dropped),
      note: r.droppedNote,
    })
  }
  if(ai.pendingChunks > 0 || ai.bufferedChars > 0) {
    rows.push({
      label: r.labels.ai,
      value: r.pending(ai.pendingChunks),
      note: ai.bufferedChars > 0 ? r.buffered(ai.bufferedChars) : r.queued,
    })
  }

  const summary = [
    r.backlog(formatDiagMs(outboundQueueMs)),
    `${r.labels.partial} ${audio.lastPartialLagMs === null
      ? '—'
      : `${formatDiagMs(audio.lastPartialLagMs)} (${r.average(formatDiagMs(audio.avgPartialLagMs))})`
    }`,
    `${r.labels.final} ${audio.lastFinalLagMs === null
      ? '—'
      : `${formatDiagMs(audio.lastFinalLagMs)} (${r.average(formatDiagMs(audio.avgFinalLagMs))})`
    }`,
    r.sentFor(formatDiagSeconds(sentAudioSeconds)),
  ]
  if(dropped > 0) summary.push(r.droppedBytes(formatDiagBytes(dropped)))
  if(ai.pendingChunks > 0) summary.push(`AI ${ai.pendingChunks}`)

  const hints: string[] = []
  if(outboundQueueMs >= 200) {
    hints.push(r.hints.queue)
  }
  if(partialSlow && outboundQueueMs < 200) {
    hints.push(r.hints.partial)
  }
  if(!partialSlow && finalSlow && outboundQueueMs < 200) {
    hints.push(r.hints.final)
  }
  if(dropped > 0) {
    hints.push(r.hints.dropped)
  }
  if(ai.pendingChunks >= 2) {
    hints.push(r.hints.ai)
  }
  if(hints.length === 0) {
    hints.push(r.hints.healthy)
  }

  return {
    outboundQueueMs,
    socketBufferedBytes: audio.socketBufferedBytes,
    droppedAudioBytes: dropped,
    sentAudioSeconds,
    acceptedAudioSeconds: audio.acceptedAudioSeconds,
    transcriptBehindMs: finalBehindMs,
    partialBehindMs,
    finalBehindMs,
    lastPartialLagMs: audio.lastPartialLagMs,
    lastFinalLagMs: audio.lastFinalLagMs,
    avgPartialLagMs: audio.avgPartialLagMs,
    avgFinalLagMs: audio.avgFinalLagMs,
    maxPartialLagMs: audio.maxPartialLagMs,
    maxFinalLagMs: audio.maxFinalLagMs,
    partialSampleCount: audio.partialSampleCount,
    finalSampleCount: audio.finalSampleCount,
    lastPartialAgeMs: audio.lastPartialAgeMs,
    lastFinalAgeMs: audio.lastFinalAgeMs,
    hasActivePartial: audio.hasActivePartial,
    aiPendingChunks: ai.pendingChunks,
    aiBufferedChars: ai.bufferedChars,
    tone,
    summary: summary.join(' · '),
    detail: hints.join(' '),
    rows,
  }
}

export interface UnifiedWorkspaceOptions {
  ragEnabled: boolean
  settings: UnifiedSettings
  user: User | null
  /** Balance pushed by the transcription proxy; `null` asks for a fresh read. */
  onBalanceUpdated?: (balance: AccountBalance | null) => void
}

/** What the current session has cost its owner so far. */
export interface SessionCostView {
  /** Transcription + translation charges, USD — the headline figure. */
  realtimeUsd: number
  /** Separately billed AI features (chat, summaries) on this session, USD. */
  aiUsd: number
  /** True while recording: 5s-quantized reservation tails are not refunded yet. */
  approximate: boolean
}

export interface TransportDiagRow {
  label: string
  value: string
  note: string
}

/**
 * Live transport / recognition lag snapshot. Local recording can stay healthy
 * while outboundQueueMs climbs — that mismatch is exactly the "录音正常、字幕慢"
 * class of bugs.
 */
export interface TransportDiagnostics {
  /** Audio still waiting to leave the browser (queue + WS buffer), in ms. */
  outboundQueueMs: number
  socketBufferedBytes: number
  droppedAudioBytes: number
  sentAudioSeconds: number
  acceptedAudioSeconds: number
  /** @deprecated Prefer finalBehindMs. */
  transcriptBehindMs: number
  /** Live gap for the active preliminary (partial) text, if any. */
  partialBehindMs: number | null
  /** Live gap for the latest confirmed (final) text. */
  finalBehindMs: number
  lastPartialLagMs: number | null
  lastFinalLagMs: number | null
  avgPartialLagMs: number | null
  avgFinalLagMs: number | null
  maxPartialLagMs: number
  maxFinalLagMs: number
  partialSampleCount: number
  finalSampleCount: number
  lastPartialAgeMs: number | null
  lastFinalAgeMs: number | null
  hasActivePartial: boolean
  aiPendingChunks: number
  aiBufferedChars: number
  tone: 'ok' | 'warn' | 'bad'
  /** Compact Chinese status for the top bar / toolbar. */
  summary: string
  detail: string
  /** Structured rows for the debug panel. */
  rows: TransportDiagRow[]
}

export interface UnifiedWorkspaceState {
  paymentRequired: boolean
  connectionLabel: string
  durationLabel: string
  error: string | null
  feedGeneration: number
  feedItems: ReturnType<TranscriptFeedModel['getSnapshot']>['items']
  historyLoading: boolean
  historyOpening: HistoryOpenProgress | null
  historySessions: HistorySession[]
  legacyHistoryCount: number
  pendingWrites: number
  recorderStatus: RecorderStatus
  /** Null until the current session has any attributed cost. */
  sessionCost: SessionCostView | null
  sessionId: string
  sessionSourceLanguage: string
  stats: WorkspaceStats
  title: string
  /** Null when not in an active capture session. */
  transportDiagnostics: TransportDiagnostics | null
  transcriptContext: string
  clearError: () => void
  deleteHistory: (session: HistorySession) => Promise<void>
  /** Ends an active cloud session and cuts its live stream on other devices. */
  endHistorySession: (session: HistorySession) => Promise<void>
  /** Uploads a local-only session's transcripts to the cloud. */
  uploadHistorySessionToCloud: (session: HistorySession) => Promise<void>
  downloadAudio: () => Promise<void>
  downloadText: (mode: 'original' | 'translation' | 'bilingual') => Promise<void>
  loadHistory: (session: HistorySession) => Promise<void>
  migrateLegacyHistory: () => Promise<void>
  pauseToggle: () => void
  refreshHistory: () => Promise<void>
  continueSession: () => Promise<void>
  start: () => Promise<void>
  stop: () => Promise<void>
  updateTitle: (title: string) => Promise<void>
  /** True while an AI title request is in flight for the current session. */
  titleGenerating: boolean
  /** Ask the AI to (re)name the current session from its transcript. */
  generateTitle: () => Promise<void>
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function isTranscriptInput(value: unknown): value is TranscriptInput {
  if(!isRecord(value)) return false
  return (
    typeof value.client_segment_id === 'string'
    && value.client_segment_id.trim().length > 0
    && typeof value.text === 'string'
    && typeof value.start_time === 'number'
    && Number.isFinite(value.start_time)
  )
}

export function numberValue(value: unknown, fallback = 0): number {
  return typeof value === 'number' && Number.isFinite(value)
    ? Math.max(0, value)
    : fallback
}

export function stringValue(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback
}

export function defaultSessionTitle(now = Date.now()): string {
  const date = new Intl.DateTimeFormat('zh-CN', {
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(now)
  return messages().workspace.runtime.sessionTitle(date)
}

/**
 * Titles the app assigned itself. Auto-naming only replaces these so a title
 * the user typed (or an earlier AI title) is never silently overwritten.
 */
export function isDefaultSessionTitle(title: string): boolean {
  const trimmed = title.trim()
  return trimmed === ''
    || trimmed === '未命名会话'
    || trimmed === 'Untitled session'
    || trimmed.startsWith('会话 · ')
    || trimmed.startsWith('Session · ')
    || /^Session \d{4}-\d{2}-\d{2}/.test(trimmed)
}

/** How often a running session persists its duration (see checkpointDuration). */
export const DURATION_CHECKPOINT_INTERVAL_MS = 15_000

/** Opening of the conversation, enough for naming without paying for all of it. */
export const TITLE_EXCERPT_MAX_CHARS = 2000

export function buildTitleExcerpt(snapshot: TranscriptFeedModelSnapshot): string {
  const lines: string[] = []
  let total = 0
  for(const item of snapshot.items) {
    if(item.original?.status !== 'final') continue
    const text = item.original.text?.trim()
    if(!text) continue
    const line = `${item.speaker}: ${text}`
    lines.push(line)
    total += line.length + 1
    if(total >= TITLE_EXCERPT_MAX_CHARS) break
  }
  return lines.join('\n').slice(0, TITLE_EXCERPT_MAX_CHARS)
}

export function formatDuration(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds))
  const hours = Math.floor(seconds / 3_600)
  const minutes = Math.floor(seconds % 3_600 / 60)
  const remainder = seconds % 60
  return hours > 0
    ? `${hours}:${String(minutes).padStart(2, '0')}:${String(remainder).padStart(2, '0')}`
    : `${String(minutes).padStart(2, '0')}:${String(remainder).padStart(2, '0')}`
}

export async function updateCloudSessionWithTimeout(
  sessionId: string,
  data: Parameters<typeof updateCloudSession>[1],
  timeoutMs = 15_000,
): Promise<void> {
  const controller = new AbortController()
  const timeout = globalThis.setTimeout(() => controller.abort(), timeoutMs)
  try {
    await updateCloudSession(sessionId, data, controller.signal)
  } finally {
    globalThis.clearTimeout(timeout)
  }
}

export function shouldRetryCloudRequest(reason: unknown): boolean {
  if(!(reason instanceof ApiRequestError)) return true
  return reason.status === 408
    || reason.status === 425
    || reason.status === 429
    || reason.status >= 500
}

export async function waitForRetry(delayMs: number): Promise<void> {
  await new Promise<void>((resolve) => {
    globalThis.setTimeout(resolve, delayMs)
  })
}

export async function withOperationTimeout<T>(
  operation: Promise<T>,
  timeoutMs: number,
  message: string,
): Promise<T> {
  let timeout: ReturnType<typeof globalThis.setTimeout> | null = null
  const timeoutPromise = new Promise<never>((_resolve, reject) => {
    timeout = globalThis.setTimeout(() => reject(new Error(message)), timeoutMs)
  })
  try {
    return await Promise.race([operation, timeoutPromise])
  } finally {
    if(timeout !== null) globalThis.clearTimeout(timeout)
  }
}

export async function updateCloudSessionWithRetry(
  sessionId: string,
  data: Parameters<typeof updateCloudSession>[1],
  attempts = 3,
  isCurrent: () => boolean = () => true,
): Promise<void> {
  let lastFailure: unknown
  const delays = [0, 500, 1_500, 4_000]
  for(let attempt = 0;attempt < Math.max(1, attempts);attempt += 1) {
    if(!isCurrent()) throw new Error(messages().workspace.runtime.ownerChanged)
    const delayMs = delays[Math.min(attempt, delays.length - 1)] ?? 4_000
    if(delayMs > 0) await waitForRetry(delayMs)
    if(!isCurrent()) throw new Error(messages().workspace.runtime.ownerChanged)
    try {
      await updateCloudSessionWithTimeout(sessionId, data, 8_000)
      return
    } catch(reason) {
      lastFailure = reason
      if(!shouldRetryCloudRequest(reason)) throw reason
    }
  }
  throw lastFailure ?? new Error(messages().workspace.runtime.cloudUpdateFailed)
}

export function canonicalTranscript(
  value: unknown,
  fallbackSequence: number,
  receivedAt: number,
): TranscriptSegment | null {
  if(!isRecord(value)) return null
  const text = stringValue(value.text).trim()
  const startTime = numberValue(value.startTime)
  const endTime = Math.max(startTime, numberValue(value.endTime, startTime))
  if(!text || value.status !== 'final') return null
  const speaker = stringValue(value.speaker, 'Speaker').trim() || 'Speaker'
  const id = stringValue(value.id).trim() || createStableTranscriptId({
    speaker,
    text,
    startTime,
    endTime,
  })
  return Object.freeze({
    id,
    sequence: fallbackSequence,
    speaker,
    text,
    status: 'final',
    startTime,
    endTime,
    receivedAt: numberValue(value.receivedAt, receivedAt),
    source: stringValue(value.source, 'local'),
  })
}

export function appendLegacyTranscriptLine(
  target: TranscriptSegment[],
  value: Record<string, unknown>,
  receivedAt: number,
): boolean {
  if(!Array.isArray(value.confirmedSegments)) return false
  const speaker = stringValue(value.speaker, 'Speaker').trim() || 'Speaker'
  for(const rawSegment of value.confirmedSegments) {
    if(!isRecord(rawSegment)) continue
    const text = stringValue(rawSegment.text).trim()
    if(!text) continue
    const startTime = numberValue(rawSegment.startTime)
    const endTime = Math.max(startTime, numberValue(rawSegment.endTime, startTime))
    const id = createStableTranscriptId({ speaker, text, startTime, endTime })
    target.push(Object.freeze({
      id,
      sequence: target.length,
      speaker,
      text,
      status: 'final',
      startTime,
      endTime,
      receivedAt,
      source: 'legacy-classic',
    }))
  }
  return true
}

export function canonicalTranslation(
  value: unknown,
  fallbackSequence: number,
  receivedAt: number,
  store: TranscriptStore,
  targetLanguage: string,
): TranslationSegment | null {
  if(!isRecord(value)) return null
  const canonicalText = stringValue(value.text).trim()
  const legacyText = stringValue(value.content).trim()
  const text = canonicalText || legacyText
  if(!text || value.isPartial === true) return null
  const speaker = stringValue(value.speaker, 'Speaker').trim() || 'Speaker'
  const startTime = numberValue(value.startTime)
  const endTime = Math.max(startTime, numberValue(value.endTime, startTime))
  const language = stringValue(value.language, targetLanguage).trim() || targetLanguage
  const storedSegmentId = typeof value.segmentId === 'string' ? value.segmentId : undefined
  const segmentId = storedSegmentId && store.getSegment(storedSegmentId)
    ? storedSegmentId
    : store.findSegmentId(speaker, startTime, endTime)
  const id = stringValue(value.id).trim() || createStableTranslationId({
    segmentId,
    speaker,
    language,
    text,
    startTime,
    endTime,
  })
  return Object.freeze({
    id,
    sequence: fallbackSequence,
    segmentId,
    speaker,
    language,
    text,
    status: 'final',
    startTime,
    endTime,
    receivedAt: numberValue(value.receivedAt, receivedAt),
    source: stringValue(value.source, canonicalText ? 'local' : 'legacy-classic'),
  })
}

export function transcriptRecordIsCanonical(
  record: {
    sequence: number
    recordId: string
    data: unknown
  },
  canonical: TranscriptSegment,
): boolean {
  const data = record.data
  return isRecord(data)
    && record.sequence === canonical.sequence
    && record.recordId === canonical.id
    && data.id === canonical.id
    && data.sequence === canonical.sequence
    && data.speaker === canonical.speaker
    && data.text === canonical.text
    && data.status === canonical.status
    && data.startTime === canonical.startTime
    && data.endTime === canonical.endTime
    && data.receivedAt === canonical.receivedAt
    && data.source === canonical.source
}

export function translationRecordIsCanonical(
  record: {
    sequence: number
    recordId: string
    data: unknown
  },
  canonical: TranslationSegment,
): boolean {
  const data = record.data
  return isRecord(data)
    && record.sequence === canonical.sequence
    && record.recordId === canonical.id
    && data.id === canonical.id
    && data.sequence === canonical.sequence
    && data.segmentId === canonical.segmentId
    && data.speaker === canonical.speaker
    && data.language === canonical.language
    && data.text === canonical.text
    && data.status === canonical.status
    && data.startTime === canonical.startTime
    && data.endTime === canonical.endTime
    && data.receivedAt === canonical.receivedAt
    && data.source === canonical.source
}

export class WordCounter {
  private readonly counts = new Map<string, number>()
  private topEntries: Array<{ word: string; count: number }> = []

  reset(): void {
    this.counts.clear()
    this.topEntries = []
  }

  add(text: string): void {
    const words = text.toLocaleLowerCase().match(/[\p{L}\p{N}]{2,}/gu) ?? []
    for(const word of words) {
      if(commonWords.has(word)) continue
      const count = (this.counts.get(word) ?? 0) + 1
      this.counts.set(word, count)
      const existing = this.topEntries.find((entry) => entry.word === word)
      if(existing) {
        existing.count = count
      } else if(
        this.topEntries.length < 12
        || count > (this.topEntries.at(-1)?.count ?? 0)
      ) {
        if(this.topEntries.length >= 12) this.topEntries.pop()
        this.topEntries.push({ word, count })
      }
      this.topEntries.sort((left, right) => (
        right.count - left.count || left.word.localeCompare(right.word)
      ))
    }
  }

  getTop(): Array<{ word: string; count: number }> {
    return this.topEntries.map((entry) => ({ ...entry }))
  }
}

export async function canonicalizeLocalSession(
  repository: IndexedDbSessionRepository<TranscriptSegment, TranslationSegment>,
  sessionId: string,
  targetLanguage: string,
): Promise<StoredSessionRecords> {
  const metadata = await repository.getSessionMetadata(sessionId)
  const receivedAt = metadata?.createdAt ?? Date.now()
  const segments: TranscriptSegment[] = []
  let rewriteTranscripts = false

  for await(const record of repository.iterateTranscripts(sessionId, 500)) {
    const canonical = canonicalTranscript(record.data, segments.length, receivedAt)
    if(canonical) {
      segments.push(Object.freeze({ ...canonical, sequence: segments.length }))
      if(!transcriptRecordIsCanonical(record, canonical)) rewriteTranscripts = true
      continue
    }
    rewriteTranscripts = true
    if(isRecord(record.data) && appendLegacyTranscriptLine(segments, record.data, receivedAt)) {
      continue
    }
  }

  const lookupStore = new TranscriptStore()
  lookupStore.batch(() => {
    for(const segment of segments) {
      lookupStore.appendTranscript(segment)
    }
  })

  const translations: TranslationSegment[] = []
  let rewriteTranslations = false
  for await(const record of repository.iterateTranslations(sessionId, 500)) {
    const translation = canonicalTranslation(
      record.data,
      translations.length,
      receivedAt,
      lookupStore,
      targetLanguage,
    )
    if(!translation) {
      rewriteTranslations = true
      continue
    }
    translations.push(Object.freeze({ ...translation, sequence: translations.length }))
    if(!translationRecordIsCanonical(record, translation)) rewriteTranslations = true
  }

  if(rewriteTranscripts) {
    await repository.writeTranscriptRecords(
      sessionId,
      segments.map((segment) => ({
        sequence: segment.sequence,
        recordId: segment.id,
        data: segment,
      })),
      segments.length,
    )
  }
  if(rewriteTranslations) {
    await repository.writeTranslationRecords(
      sessionId,
      translations.map((translation) => ({
        sequence: translation.sequence,
        recordId: translation.id,
        data: translation,
      })),
      translations.length,
    )
  }
  return {
    segments,
    translations,
  }
}
