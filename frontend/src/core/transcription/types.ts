/**
 * Framework-independent transcription domain types.
 *
 * Final records are immutable and append-only. Frequently changing partial
 * results live outside the final record collections so a partial update never
 * rewrites a long transcript.
 */

export type SegmentId = string
export type TranslationId = string

export interface TimeRange {
  readonly startTime: number
  readonly endTime: number
}

export interface TranscriptSegment extends TimeRange {
  readonly id: SegmentId
  readonly sequence: number
  readonly speaker: string
  readonly text: string
  readonly status: 'final'
  readonly receivedAt: number
  readonly source: string
}

export interface TranscriptSegmentInput extends TimeRange {
  readonly id?: SegmentId
  readonly speaker?: string
  readonly text: string
  readonly receivedAt?: number
  readonly source?: string
}

export interface TranscriptPartial extends TimeRange {
  readonly id: string
  readonly speaker: string
  readonly text: string
  readonly status: 'partial'
  readonly revision: number
  readonly createdAt: number
  readonly updatedAt: number
  readonly source: string
}

export interface TranscriptPartialInput extends TimeRange {
  readonly id?: string
  readonly speaker?: string
  readonly text: string
  readonly receivedAt?: number
  readonly source?: string
}

export interface TranslationSegment extends TimeRange {
  readonly id: TranslationId
  readonly sequence: number
  readonly segmentId: SegmentId | null
  readonly speaker: string
  readonly language: string
  readonly text: string
  readonly status: 'final'
  readonly receivedAt: number
  readonly source: string
}

export interface TranslationSegmentInput extends TimeRange {
  readonly id?: TranslationId
  readonly segmentId?: SegmentId | null
  readonly speaker?: string
  readonly language: string
  readonly text: string
  readonly receivedAt?: number
  readonly source?: string
}

export interface TranslationPartial extends TimeRange {
  readonly id: string
  readonly segmentId: SegmentId | null
  readonly speaker: string
  readonly language: string
  readonly text: string
  readonly status: 'partial'
  readonly revision: number
  readonly createdAt: number
  readonly updatedAt: number
  readonly source: string
}

export interface TranslationPartialInput extends TimeRange {
  readonly id?: string
  readonly segmentId?: SegmentId | null
  readonly speaker?: string
  readonly language: string
  readonly text: string
  readonly receivedAt?: number
  readonly source?: string
}

/**
 * All values are maintained incrementally as final records arrive.
 */
export interface TranscriptStats {
  readonly segmentCount: number
  readonly translationCount: number
  readonly linkedTranslationCount: number
  readonly speakerCount: number
  readonly transcriptWordCount: number
  readonly transcriptCharacterCount: number
  readonly translationWordCount: number
  readonly translationCharacterCount: number
  readonly durationSeconds: number
}

/**
 * A deliberately small, cached snapshot suitable for React's
 * useSyncExternalStore. Use TranscriptStore range accessors to read a visible
 * history window; the snapshot itself never copies the complete history.
 */
export interface TranscriptStoreSnapshot {
  readonly version: number
  readonly generation: number
  readonly segmentCount: number
  readonly translationCount: number
  readonly activePartial: TranscriptPartial | null
  readonly activeTranslationPartials: readonly TranslationPartial[]
  readonly latestSegmentId: SegmentId | null
  readonly latestTranslationId: TranslationId | null
  readonly stats: TranscriptStats
}

export interface RecordRange {
  readonly offset?: number
  readonly limit?: number
}

export interface AppendResult<T> {
  readonly record: T
  readonly inserted: boolean
}

export type SpeechmaticsDiarization = 'none' | 'speaker'
export type SpeechmaticsOperatingPoint = 'standard' | 'enhanced'
export type SpeechmaticsAudioEncoding = 'pcm_f32le'

export interface SpeechmaticsAudioFormat {
  readonly type?: 'raw'
  readonly encoding?: SpeechmaticsAudioEncoding
  readonly sample_rate?: number
  readonly channels?: number
}

/**
 * Mirrors the current proxy configuration closely so the existing Pro
 * settings can migrate without another translation layer.
 */
export interface SpeechmaticsProxyConfig {
  /**
   * Client-only offset applied to timestamps from a fresh socket when
   * continuing an existing local/cloud session. It is never sent upstream.
   */
  readonly timeline_offset_seconds?: number
  readonly language?: string
  readonly enable_partials?: boolean
  readonly diarization?: SpeechmaticsDiarization
  readonly max_delay?: number
  readonly operating_point?: SpeechmaticsOperatingPoint
  readonly audio_format?: SpeechmaticsAudioFormat
  readonly translation_config?: {
    readonly target_languages: readonly string[]
    readonly enable_partials?: boolean
  }
}

export type SpeechmaticsClientStatus =
  | 'idle'
  | 'starting'
  | 'running'
  | 'paused'
  | 'reconnecting'
  | 'stopping'
  | 'stopped'
  | 'error'
  | 'destroyed'

export interface SpeechmaticsClientSnapshot {
  readonly version: number
  readonly status: SpeechmaticsClientStatus
  readonly error: string | null
  readonly reconnectAttempt: number
  readonly maxReconnectAttempts: number
  readonly connected: boolean
  readonly acceptingAudio: boolean
  readonly startedAt: number | null
}

export interface SpeechmaticsClientDiagnostics {
  readonly queuedAudioBytes: number
  readonly pendingFrameBytes: number
  readonly socketBufferedBytes: number
  /**
   * Estimated wall time represented by local outbound audio not yet flushed
   * through the browser WebSocket send buffer (queue + pending frame +
   * socket.bufferedAmount). High values mean the UI will lag even when local
   * recording is healthy.
   */
  readonly outboundQueueMs: number
  readonly bytesPerSecond: number
  readonly acceptedAudioSeconds: number
  readonly timelineEnd: number
  readonly droppedAudioBytes: number
  readonly sentAudioBytes: number
  readonly connectionCount: number
  /** Connected audio destination (host/path only); absent before recognition starts. */
  readonly connectionEndpoint: string | null
  /**
   * Latency samples compare each result's audio endTime against the amount of
   * PCM already accepted when the result was applied (ms).
   */
  readonly lastPartialLagMs: number | null
  readonly lastFinalLagMs: number | null
  readonly avgPartialLagMs: number | null
  readonly avgFinalLagMs: number | null
  readonly maxPartialLagMs: number
  readonly maxFinalLagMs: number
  readonly partialSampleCount: number
  readonly finalSampleCount: number
  /** Live gap: accepted audio timeline minus latest partial end (if any). */
  readonly partialBehindMs: number | null
  /** Live gap: accepted audio timeline minus latest final end. */
  readonly finalBehindMs: number
  readonly lastPartialEndTime: number | null
  readonly lastFinalEndTime: number | null
  /** Wall-clock age of the last partial/final application. */
  readonly lastPartialAgeMs: number | null
  readonly lastFinalAgeMs: number | null
  readonly hasActivePartial: boolean
}
