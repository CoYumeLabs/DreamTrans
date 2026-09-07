export { TranscriptStore } from './TranscriptStore'
export {
  createStableTranscriptId,
  createStableTranslationId,
} from './TranscriptStore'
export type { TranscriptStoreOptions } from './TranscriptStore'

export {
  endsSentence,
  isCjkChar,
  joinSegmentTexts,
  normalizeTranscriptText,
  textWeight,
} from './scriptText'

export {
  SpeechmaticsProxyClient,
  SpeechmaticsPaymentRequiredError,
  resolveSpeechmaticsProxyUrl,
} from './SpeechmaticsProxyClient'
export type {
  SpeechmaticsAudioDroppedEvent,
  SpeechmaticsAudioTransportOptions,
  SpeechmaticsClientErrorEvent,
  SpeechmaticsClientEventMap,
  SpeechmaticsProxyClientOptions,
  SpeechmaticsReconnectEvent,
  SpeechmaticsReconnectedEvent,
  SpeechmaticsReconnectOptions,
  SpeechmaticsSocket,
  SpeechmaticsSocketFactory,
  SpeechmaticsTokenProvider,
} from './SpeechmaticsProxyClient'

export type {
  AppendResult,
  RecordRange,
  SegmentId,
  SpeechmaticsAudioEncoding,
  SpeechmaticsAudioFormat,
  SpeechmaticsClientDiagnostics,
  SpeechmaticsClientSnapshot,
  SpeechmaticsClientStatus,
  SpeechmaticsDiarization,
  SpeechmaticsOperatingPoint,
  SpeechmaticsProxyConfig,
  TimeRange,
  TranscriptPartial,
  TranscriptPartialInput,
  TranscriptSegment,
  TranscriptSegmentInput,
  TranscriptStats,
  TranscriptStoreSnapshot,
  TranslationId,
  TranslationPartial,
  TranslationPartialInput,
  TranslationSegment,
  TranslationSegmentInput,
} from './types'
