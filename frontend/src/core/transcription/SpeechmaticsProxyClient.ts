import { TranscriptStore } from './TranscriptStore'
import { websocketAuthProtocols } from '../../utils/websocketAuth'
import type {
  SpeechmaticsClientDiagnostics,
  SpeechmaticsClientSnapshot,
  SpeechmaticsClientStatus,
  SpeechmaticsProxyConfig,
  TranscriptPartial,
  TranscriptPartialInput,
  TranscriptSegment,
  TranslationPartial,
  TranslationPartialInput,
  TranslationSegment,
} from './types'
import { normalizeTranscriptText } from './scriptText'

const SOCKET_CONNECTING = 0
const SOCKET_OPEN = 1
const DEFAULT_STARTUP_TIMEOUT_MS = 15_000
const DEFAULT_STOP_TIMEOUT_MS = 8_000
const DEFAULT_PARTIAL_INTERVAL_MS = 50
const DEFAULT_SAMPLE_RATE = 48_000
const DEFAULT_CHANNELS = 1
const PCM_F32_BYTES_PER_SAMPLE = 4
const DEFAULT_FRAME_DURATION_MS = 40
const DEFAULT_HIGH_WATER_MARK_BYTES = 256 * 1024
const DEFAULT_DRAIN_INTERVAL_MS = 20
const DEFAULT_MAX_QUEUED_AUDIO_SECONDS = 5
const DEFAULT_MAX_RECONNECT_ATTEMPTS = 5
const DEFAULT_RECONNECT_BASE_DELAY_MS = 1_000
const DEFAULT_RECONNECT_MAX_DELAY_MS = 30_000
const DEFAULT_RECONNECT_JITTER_MS = 1_000
const MAX_RECENT_FINAL_EVENTS = 512

type TimerHandle = ReturnType<typeof globalThis.setTimeout>

export type SpeechmaticsTokenProvider = () => string | Promise<string>

export interface SpeechmaticsSocket {
  readonly readyState: number
  readonly bufferedAmount: number
  binaryType: BinaryType
  onopen: ((event: Event) => void) | null
  onmessage: ((event: { readonly data: unknown }) => void) | null
  onerror: ((event: Event) => void) | null
  onclose:
    | ((event: {
        readonly code: number
        readonly reason: string
        readonly wasClean?: boolean
      }) => void)
    | null
  send(data: string | ArrayBuffer | ArrayBufferView | Blob): void
  close(code?: number, reason?: string): void
}

export type SpeechmaticsSocketFactory = (
  url: string,
  protocols: readonly string[],
) => SpeechmaticsSocket

export interface SpeechmaticsReconnectOptions {
  readonly maxAttempts?: number
  readonly baseDelayMs?: number
  readonly maxDelayMs?: number
  readonly jitterMs?: number
}

export interface SpeechmaticsAudioTransportOptions {
  readonly sampleRate?: number
  readonly channels?: number
  readonly frameDurationMs?: number
  readonly maxFrameLatencyMs?: number
  readonly highWaterMarkBytes?: number
  readonly drainIntervalMs?: number
  readonly maxQueuedAudioSeconds?: number
  readonly maxQueuedAudioBytes?: number
}

export interface SpeechmaticsProxyClientOptions {
  /** Readable HTTP access check before reconnecting (WebSocket hides HTTP 402). */
  readonly beforeReconnect?: () => Promise<void>
  /**
   * Defaults to /ws/speechmatics on the current browser origin.
   */
  readonly url?: string | (() => string)
  /**
   * Called before every initial connection and reconnect. This allows the
   * existing auth layer to refresh short-lived access tokens proactively.
   */
  readonly tokenProvider: SpeechmaticsTokenProvider
  readonly store?: TranscriptStore
  readonly socketFactory?: SpeechmaticsSocketFactory
  readonly protocolFactory?: (token: string) => readonly string[]
  readonly reconnect?: SpeechmaticsReconnectOptions
  readonly audio?: SpeechmaticsAudioTransportOptions
  readonly startupTimeoutMs?: number
  readonly stopTimeoutMs?: number
  readonly partialUpdateIntervalMs?: number
  readonly resetStoreOnStart?: boolean
  readonly clock?: () => number
  readonly random?: () => number
}

export interface SpeechmaticsClientErrorEvent {
  readonly message: string
  readonly fatal: boolean
  readonly cause?: unknown
}

export interface SpeechmaticsReconnectEvent {
  readonly attempt: number
  readonly maxAttempts: number
  readonly delayMs: number
}

export interface SpeechmaticsReconnectedEvent {
  readonly connectionCount: number
  readonly timelineOffset: number
}

export interface SpeechmaticsAudioDroppedEvent {
  readonly bytes: number
  readonly totalDroppedBytes: number
}

/**
 * Billing update pushed by the proxy after each metered charge. `balance` is
 * the account snapshot (`available_usd` etc.) when the server included one.
 */
export interface SpeechmaticsBalanceEvent {
  readonly costUsd: number
  readonly balanceUsd: number | null
  readonly balance: Readonly<Record<string, unknown>> | null
}

export interface SpeechmaticsTerminatedEvent {
  readonly reason: string
}

export interface SpeechmaticsClientEventMap {
  readonly paymentRequired: { readonly reason: string }
  readonly state: SpeechmaticsClientSnapshot
  readonly transcript: TranscriptSegment
  readonly partial: TranscriptPartial | null
  readonly translation: TranslationSegment
  readonly translationPartial: TranslationPartial | null
  readonly error: SpeechmaticsClientErrorEvent
  readonly reconnecting: SpeechmaticsReconnectEvent
  readonly reconnected: SpeechmaticsReconnectedEvent
  readonly balance: SpeechmaticsBalanceEvent
  readonly audioDropped: SpeechmaticsAudioDroppedEvent
  readonly terminated: SpeechmaticsTerminatedEvent
  readonly rawMessage: Readonly<Record<string, unknown>>
}

interface AudioFrame {
  readonly data: ArrayBuffer
  readonly startTime: number
  readonly endTime: number
}

export class SpeechmaticsPaymentRequiredError extends Error {}

interface SocketContext {
  readonly serial: number
  readonly socket: SpeechmaticsSocket
  readonly reconnect: boolean
  timelineOffset: number
  recognitionStarted: boolean
  startupSettled: boolean
  startupTimer: TimerHandle | null
  resolveStartup: () => void
  rejectStartup: (error: Error) => void
  messageChain: Promise<void>
  /**
   * Binary AddAudio messages sent on this connection. The Speechmatics
   * real-time API requires EndOfStream to carry this count as last_seq_no.
   */
  sentAudioChunks: number
}

interface EndWaiter {
  readonly promise: Promise<void>
  readonly resolve: () => void
  readonly reject: (error: Error) => void
  readonly timer: TimerHandle
}

function positiveNumber(value: number | undefined, fallback: number, field: string): number {
  const resolved = value ?? fallback
  if (!Number.isFinite(resolved) || resolved <= 0) {
    throw new TypeError(`${field} must be a positive finite number`)
  }
  return resolved
}

function nonNegativeNumber(value: number | undefined, fallback: number, field: string): number {
  const resolved = value ?? fallback
  if (!Number.isFinite(resolved) || resolved < 0) {
    throw new TypeError(`${field} must be a non-negative finite number`)
  }
  return resolved
}

function nonNegativeInteger(
  value: number | undefined,
  fallback: number,
  field: string,
): number {
  const resolved = value ?? fallback
  if (!Number.isInteger(resolved) || resolved < 0) {
    throw new TypeError(`${field} must be a non-negative integer`)
  }
  return resolved
}

function positiveInteger(value: number | undefined, fallback: number, field: string): number {
  const resolved = value ?? fallback
  if (!Number.isInteger(resolved) || resolved <= 0) {
    throw new TypeError(`${field} must be a positive integer`)
  }
  return resolved
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null
  return value as Record<string, unknown>
}

function parseBalanceUpdate(message: Record<string, unknown>): SpeechmaticsBalanceEvent {
  const cost = typeof message.cost_usd === 'number'
    ? message.cost_usd
    : typeof message.cost === 'number' ? message.cost : 0
  const balance = message.balance && typeof message.balance === 'object'
    ? Object.freeze({ ...(message.balance as Record<string, unknown>) })
    : null
  const balanceUsd = typeof message.balance_usd === 'number'
    ? message.balance_usd
    : balance && typeof balance.available_usd === 'number' ? balance.available_usd : null
  return Object.freeze({ costUsd: cost, balanceUsd, balance })
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined
}

function rawTime(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? Math.max(0, value) : 0
}

function socketMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback
}

function defaultSocketFactory(
  url: string,
  protocols: readonly string[],
): SpeechmaticsSocket {
  return new WebSocket(url, [...protocols]) as unknown as SpeechmaticsSocket
}

function defaultProtocolFactory(token: string): readonly string[] {
  return websocketAuthProtocols(token)
}

async function socketDataToText(data: unknown): Promise<string> {
  if (typeof data === 'string') return data
  if (data instanceof ArrayBuffer) return new TextDecoder().decode(data)
  if (ArrayBuffer.isView(data)) {
    return new TextDecoder().decode(
      new Uint8Array(data.buffer, data.byteOffset, data.byteLength),
    )
  }
  if (typeof Blob !== 'undefined' && data instanceof Blob) return data.text()
  throw new TypeError('Unsupported transcription WebSocket message type')
}

function audioBytes(data: ArrayBuffer | ArrayBufferView): Uint8Array {
  if (data instanceof ArrayBuffer) return new Uint8Array(data)
  return new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
}

/**
 * Resolve a backend HTTP/WS base URL into the Speechmatics proxy endpoint.
 * A session id, when known, is attached so the server can associate the live
 * stream with the session and let the user end it from another device.
 */
export function resolveSpeechmaticsProxyUrl(
  backendUrl = '/',
  sessionId?: string | null,
): string {
  let endpoint: string
  if (backendUrl === '/') {
    if (typeof window === 'undefined') {
      throw new Error('Transcription proxy URL is required outside a browser')
    }
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    endpoint = `${protocol}//${window.location.host}/ws/speechmatics`
  } else {
    const base = backendUrl
      .replace(/^http:\/\//i, 'ws://')
      .replace(/^https:\/\//i, 'wss://')
      .replace(/\/+$/, '')
    endpoint = base.endsWith('/ws/speechmatics') ? base : `${base}/ws/speechmatics`
  }
  const trimmedSessionId = sessionId?.trim()
  if (!trimmedSessionId) return endpoint
  return `${endpoint}?session_id=${encodeURIComponent(trimmedSessionId)}`
}

/**
 * Framework-independent Speechmatics proxy transport and session controller.
 *
 * - Its client snapshot is directly compatible with useSyncExternalStore.
 * - Final transcript data is written into the normalized TranscriptStore.
 * - Tiny AudioWorklet chunks are coalesced before WebSocket sends.
 * - Reconnect/backpressure queues are byte-bounded and drained by water level.
 */
export class SpeechmaticsProxyClient {
  readonly store: TranscriptStore

  private readonly url: string | (() => string)
  private readonly tokenProvider: SpeechmaticsTokenProvider
  private readonly socketFactory: SpeechmaticsSocketFactory
  private readonly beforeReconnect?: () => Promise<void>
  private paymentBlocked = false
  private paymentResumeInProgress = false
  private connectionGeneration = 0
  private readonly protocolFactory: (token: string) => readonly string[]
  private readonly clock: () => number
  private readonly random: () => number
  private readonly resetStoreOnStart: boolean
  private readonly startupTimeoutMs: number
  private readonly stopTimeoutMs: number
  private readonly partialUpdateIntervalMs: number
  private readonly maxReconnectAttempts: number
  private readonly reconnectBaseDelayMs: number
  private readonly reconnectMaxDelayMs: number
  private readonly reconnectJitterMs: number
  private readonly frameDurationMs: number
  private readonly maxFrameLatencyMs: number
  private readonly highWaterMarkBytes: number
  private readonly drainIntervalMs: number
  private readonly maxQueuedAudioSeconds: number
  private readonly configuredMaxQueuedAudioBytes: number | null
  private readonly configuredSampleRate: number
  private readonly configuredChannels: number
  private readonly subscribers = new Set<() => void>()
  private readonly eventListeners = new Map<
    keyof SpeechmaticsClientEventMap,
    Set<(payload: unknown) => void>
  >()
  private readonly recentFinalKeys = new Set<string>()
  private readonly recentFinalOrder: string[] = []

  private statusValue: SpeechmaticsClientStatus = 'idle'
  private errorValue: string | null = null
  private clientVersion = 0
  private snapshotValue: SpeechmaticsClientSnapshot
  private startedAtValue: number | null = null
  private reconnectAttemptValue = 0
  private desiredSession = false
  private capturePaused = false
  private stopping = false
  private destroyed = false
  private recognitionReady = false
  private configValue: SpeechmaticsProxyConfig = {}
  private context: SocketContext | null = null
  private socketSerial = 0
  private reconnectTimer: TimerHandle | null = null
  private drainTimer: TimerHandle | null = null
  private frameTimer: TimerHandle | null = null
  private transcriptPartialTimer: TimerHandle | null = null
  private translationPartialTimer: TimerHandle | null = null
  private endWaiter: EndWaiter | null = null
  private stopPromise: Promise<void> | null = null
  private lifecycleGeneration = 0
  private lastPartialAppliedAt = 0
  private pendingTranscriptPartial: TranscriptPartialInput | null = null
  private pendingTranslationPartials = new Map<string, TranslationPartialInput>()

  private sampleRate = DEFAULT_SAMPLE_RATE
  private channels = DEFAULT_CHANNELS
  private bytesPerSecond = DEFAULT_SAMPLE_RATE * DEFAULT_CHANNELS * PCM_F32_BYTES_PER_SAMPLE
  private frameBytes = Math.round(
    (DEFAULT_SAMPLE_RATE *
      DEFAULT_CHANNELS *
      PCM_F32_BYTES_PER_SAMPLE *
      DEFAULT_FRAME_DURATION_MS) /
      1000,
  )
  private maxQueuedAudioBytes =
    DEFAULT_SAMPLE_RATE *
    DEFAULT_CHANNELS *
    PCM_F32_BYTES_PER_SAMPLE *
    DEFAULT_MAX_QUEUED_AUDIO_SECONDS
  private pendingFrame = new Uint8Array(this.frameBytes)
  private pendingFrameBytes = 0
  private pendingFrameStartTime = 0
  private audioQueue: AudioFrame[] = []
  private queuedAudioBytes = 0
  private acceptedAudioSeconds = 0
  private sessionTimelineBase = 0
  private lastTimelineEnd = 0
  private droppedAudioBytes = 0
  private sentAudioBytes = 0
  private connectionCount = 0
  // Result latency vs accepted PCM timeline (partial = 待确定, final = 已确认).
  private lastPartialLagMs: number | null = null
  private lastFinalLagMs: number | null = null
  private emaPartialLagMs: number | null = null
  private emaFinalLagMs: number | null = null
  private maxPartialLagMs = 0
  private maxFinalLagMs = 0
  private partialSampleCount = 0
  private finalSampleCount = 0
  private lastPartialEndTime: number | null = null
  private lastFinalEndTime: number | null = null
  private lastPartialWallAt = 0
  private lastFinalWallAt = 0
  private hasActivePartial = false

  constructor(options: SpeechmaticsProxyClientOptions) {
    this.beforeReconnect = options.beforeReconnect
    this.url = options.url ?? (() => resolveSpeechmaticsProxyUrl('/'))
    this.tokenProvider = options.tokenProvider
    this.store = options.store ?? new TranscriptStore({ clock: options.clock })
    this.socketFactory = options.socketFactory ?? defaultSocketFactory
    this.protocolFactory = options.protocolFactory ?? defaultProtocolFactory
    this.clock = options.clock ?? Date.now
    this.random = options.random ?? Math.random
    this.resetStoreOnStart = options.resetStoreOnStart ?? true
    this.startupTimeoutMs = positiveNumber(
      options.startupTimeoutMs,
      DEFAULT_STARTUP_TIMEOUT_MS,
      'startupTimeoutMs',
    )
    this.stopTimeoutMs = positiveNumber(
      options.stopTimeoutMs,
      DEFAULT_STOP_TIMEOUT_MS,
      'stopTimeoutMs',
    )
    this.partialUpdateIntervalMs = nonNegativeInteger(
      options.partialUpdateIntervalMs,
      DEFAULT_PARTIAL_INTERVAL_MS,
      'partialUpdateIntervalMs',
    )
    this.maxReconnectAttempts = nonNegativeInteger(
      options.reconnect?.maxAttempts,
      DEFAULT_MAX_RECONNECT_ATTEMPTS,
      'reconnect.maxAttempts',
    )
    this.reconnectBaseDelayMs = positiveNumber(
      options.reconnect?.baseDelayMs,
      DEFAULT_RECONNECT_BASE_DELAY_MS,
      'reconnect.baseDelayMs',
    )
    this.reconnectMaxDelayMs = positiveNumber(
      options.reconnect?.maxDelayMs,
      DEFAULT_RECONNECT_MAX_DELAY_MS,
      'reconnect.maxDelayMs',
    )
    this.reconnectJitterMs = nonNegativeInteger(
      options.reconnect?.jitterMs,
      DEFAULT_RECONNECT_JITTER_MS,
      'reconnect.jitterMs',
    )
    this.frameDurationMs = positiveNumber(
      options.audio?.frameDurationMs,
      DEFAULT_FRAME_DURATION_MS,
      'audio.frameDurationMs',
    )
    this.maxFrameLatencyMs = positiveNumber(
      options.audio?.maxFrameLatencyMs,
      this.frameDurationMs,
      'audio.maxFrameLatencyMs',
    )
    this.highWaterMarkBytes = positiveNumber(
      options.audio?.highWaterMarkBytes,
      DEFAULT_HIGH_WATER_MARK_BYTES,
      'audio.highWaterMarkBytes',
    )
    this.drainIntervalMs = positiveNumber(
      options.audio?.drainIntervalMs,
      DEFAULT_DRAIN_INTERVAL_MS,
      'audio.drainIntervalMs',
    )
    this.maxQueuedAudioSeconds = positiveNumber(
      options.audio?.maxQueuedAudioSeconds,
      DEFAULT_MAX_QUEUED_AUDIO_SECONDS,
      'audio.maxQueuedAudioSeconds',
    )
    this.configuredMaxQueuedAudioBytes =
      options.audio?.maxQueuedAudioBytes === undefined
        ? null
        : positiveNumber(
            options.audio.maxQueuedAudioBytes,
            1,
            'audio.maxQueuedAudioBytes',
          )
    this.configuredSampleRate = positiveNumber(
      options.audio?.sampleRate,
      DEFAULT_SAMPLE_RATE,
      'audio.sampleRate',
    )
    this.configuredChannels = positiveInteger(
      options.audio?.channels,
      DEFAULT_CHANNELS,
      'audio.channels',
    )
    this.sampleRate = this.configuredSampleRate
    this.channels = this.configuredChannels
    this.configureAudio({})
    this.snapshotValue = this.buildSnapshot()
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.subscribers.add(listener)
    return () => {
      this.subscribers.delete(listener)
    }
  }

  readonly getSnapshot = (): SpeechmaticsClientSnapshot => this.snapshotValue

  on<EventName extends keyof SpeechmaticsClientEventMap>(
    eventName: EventName,
    listener: (payload: SpeechmaticsClientEventMap[EventName]) => void,
  ): () => void {
    let listeners = this.eventListeners.get(eventName)
    if (!listeners) {
      listeners = new Set()
      this.eventListeners.set(eventName, listeners)
    }
    const wrapped = (payload: unknown) => {
      listener(payload as SpeechmaticsClientEventMap[EventName])
    }
    listeners.add(wrapped)
    return () => {
      listeners?.delete(wrapped)
      if (listeners?.size === 0) this.eventListeners.delete(eventName)
    }
  }

  async start(config: SpeechmaticsProxyConfig = {}): Promise<void> {
    this.assertUsable()
    if (
      this.desiredSession ||
      this.statusValue === 'starting' ||
      this.statusValue === 'running' ||
      this.statusValue === 'paused' ||
      this.statusValue === 'reconnecting' ||
      this.statusValue === 'stopping'
    ) {
      throw new Error(`Cannot start while client is ${this.statusValue}`)
    }

    const generation = ++this.lifecycleGeneration
    this.resetRuntime()
    if (this.resetStoreOnStart) this.store.reset()
    this.configValue = this.copyConfig(config)
    this.sessionTimelineBase = nonNegativeNumber(
      config.timeline_offset_seconds,
      0,
      'timeline_offset_seconds',
    )
    this.lastTimelineEnd = this.sessionTimelineBase
    this.configureAudio(this.configValue)
    this.desiredSession = true
    this.capturePaused = false
    this.stopping = false
    this.startedAtValue = this.clock()
    this.errorValue = null
    this.setStatus('starting')

    try {
      await this.connectSocket(false)
    } catch (error) {
      if (this.destroyed) throw error
      if (generation !== this.lifecycleGeneration || !this.desiredSession) {
        throw error
      }
      const message = socketMessage(error, 'Transcription connection failed')
      this.desiredSession = false
      this.closeActiveSocket(4000, 'Startup failed')
      this.reportError(message, true, error)
      this.setStatus('error')
      throw error
    }
  }

  pause(): void {
    this.assertUsable()
    if (!this.desiredSession || this.stopping) {
      throw new Error('No active transcription session to pause')
    }
    if (this.capturePaused) return

    this.capturePaused = true
    this.flushPendingAudioFrame()
    this.drainAudioQueue()
    if (this.recognitionReady) this.setStatus('paused')
    else this.publishSnapshot()
  }

  async resume(timeoutMs = this.startupTimeoutMs): Promise<void> {
    this.assertUsable()
    if (!this.desiredSession || this.stopping) {
      throw new Error('No paused transcription session to resume')
    }
    if (this.paymentResumeInProgress) return
    if (this.paymentBlocked) {
      const generation = this.connectionGeneration
      this.paymentResumeInProgress = true
      this.paymentBlocked = false
      try {
        await this.connectSocket(true)
        if (!this.desiredSession || this.stopping || this.destroyed || generation !== this.connectionGeneration) return
        this.capturePaused = false
        this.errorValue = null
        this.setStatus('running')
        this.drainAudioQueue()
      } catch (error) {
        if (this.desiredSession && !this.stopping && !this.destroyed && generation === this.connectionGeneration) {
          this.suspendForPayment(socketMessage(error, 'Transcription resume failed'))
        }
        throw error
      } finally {
        this.paymentResumeInProgress = false
      }
      return
    }
    if (!this.capturePaused && this.recognitionReady) return

    this.capturePaused = false
    if (this.recognitionReady && this.isSocketOpen()) {
      this.setStatus('running')
      return
    }

    if (this.reconnectTimer === null && this.context === null) {
      this.scheduleReconnect('Resume requested without an active connection', true)
    }
    this.publishSnapshot()
    await this.waitForReady(timeoutMs)
    if (this.recognitionReady) this.setStatus('running')
  }

  /** Hold the same session and its unsent audio until an explicit resume. */
  suspendForPayment(reason = 'insufficient balance'): void {
    if (!this.desiredSession || this.destroyed || this.stopping) return
    this.connectionGeneration += 1
    this.paymentBlocked = true
    this.capturePaused = true
    this.recognitionReady = false
    this.clearReconnectTimer()
    if (this.context) {
      this.settleStartup(this.context, new SpeechmaticsPaymentRequiredError(reason))
    }
    this.closeActiveSocket(4003, 'Payment required')
    this.discardPendingPartials()
    this.store.clearPartial()
    this.store.clearTranslationPartials()
    this.setStatus('paused')
    this.emit('paymentRequired', { reason })
  }

  /**
   * Accepts Float32 little-endian PCM. Small worklet messages are copied into
   * a frame assembler and normally produce one WebSocket frame every ~40 ms.
   */
  sendAudio(data: ArrayBuffer | ArrayBufferView): boolean {
    this.assertUsable()
    if (!this.desiredSession || this.capturePaused || this.stopping) return false

    const bytes = audioBytes(data)
    const sampleFrameBytes = PCM_F32_BYTES_PER_SAMPLE * this.channels
    if (bytes.byteLength === 0) return true
    if (bytes.byteLength % sampleFrameBytes !== 0) {
      throw new RangeError(
        `PCM audio byte length must be a multiple of ${sampleFrameBytes}`,
      )
    }

    let sourceOffset = 0
    while (sourceOffset < bytes.byteLength) {
      if (this.pendingFrameBytes === 0) {
        this.pendingFrameStartTime = this.acceptedAudioSeconds
      }
      const available = this.frameBytes - this.pendingFrameBytes
      const copyLength = Math.min(available, bytes.byteLength - sourceOffset)
      this.pendingFrame.set(
        bytes.subarray(sourceOffset, sourceOffset + copyLength),
        this.pendingFrameBytes,
      )
      this.pendingFrameBytes += copyLength
      sourceOffset += copyLength
      this.acceptedAudioSeconds += copyLength / this.bytesPerSecond

      if (this.pendingFrameBytes === this.frameBytes) {
        this.flushPendingAudioFrame()
      }
    }

    if (this.pendingFrameBytes > 0) this.scheduleFrameFlush()
    return true
  }

  async reconnect(): Promise<void> {
    this.assertUsable()
    if (!this.desiredSession || this.stopping) {
      throw new Error('No active transcription session to reconnect')
    }

    this.flushPendingAudioFrame()
    this.clearReconnectTimer()
    this.recognitionReady = false
    this.reconnectAttemptValue = 0
    this.setStatus('reconnecting')
    this.closeActiveSocket(4001, 'Manual reconnect')

    try {
      await this.connectSocket(true)
    } catch (error) {
      if (this.destroyed) throw error
      const message = socketMessage(error, 'Transcription reconnect failed')
      this.reportError(message, false, error)
      this.scheduleReconnect(message)
      throw error
    }
  }

  async stop(timeoutMs = this.stopTimeoutMs): Promise<void> {
    this.assertUsable()
    if (this.stopPromise) return this.stopPromise
    this.connectionGeneration += 1
    if (this.paymentBlocked || this.paymentResumeInProgress) {
      this.desiredSession = false
      this.capturePaused = true
      this.clearReconnectTimer()
      this.closeActiveSocket(1000, 'Session complete')
      this.clearAudioQueue()
      this.setStatus('stopped')
      return
    }
    if (!this.desiredSession) {
      if (this.statusValue !== 'stopped') this.setStatus('stopped')
      return
    }
    this.lifecycleGeneration += 1
    if (this.statusValue === 'starting' && !this.recognitionReady) {
      this.desiredSession = false
      this.capturePaused = true
      this.stopping = false
      this.clearReconnectTimer()
      this.closeActiveSocket(4000, 'Session start cancelled')
      this.clearAudioQueue()
      this.discardPendingPartials()
      this.setStatus('stopped')
      return
    }

    this.stopPromise = this.performStop(timeoutMs).finally(() => {
      this.stopPromise = null
    })
    return this.stopPromise
  }

  getDiagnostics(): SpeechmaticsClientDiagnostics {
    const socketBufferedBytes = this.context?.socket.bufferedAmount ?? 0
    const outboundBytes =
      this.queuedAudioBytes + this.pendingFrameBytes + socketBufferedBytes
    const outboundQueueMs = this.bytesPerSecond > 0
      ? Math.round((outboundBytes / this.bytesPerSecond) * 1_000)
      : 0
    const now = this.clock()
    // Continue sessions offset result times; compare against session timeline.
    const audioCursor = this.sessionTimelineBase + this.acceptedAudioSeconds
    const partialBehindMs = this.lastPartialEndTime === null
      ? null
      : Math.max(
        0,
        Math.round((audioCursor - this.lastPartialEndTime) * 1_000),
      )
    const finalBehindMs = Math.max(
      0,
      Math.round((audioCursor - this.lastTimelineEnd) * 1_000),
    )
    return Object.freeze({
      queuedAudioBytes: this.queuedAudioBytes,
      pendingFrameBytes: this.pendingFrameBytes,
      socketBufferedBytes,
      outboundQueueMs,
      bytesPerSecond: this.bytesPerSecond,
      acceptedAudioSeconds: this.acceptedAudioSeconds,
      timelineEnd: this.lastTimelineEnd,
      droppedAudioBytes: this.droppedAudioBytes,
      sentAudioBytes: this.sentAudioBytes,
      connectionCount: this.connectionCount,
      lastPartialLagMs: this.lastPartialLagMs,
      lastFinalLagMs: this.lastFinalLagMs,
      avgPartialLagMs: this.emaPartialLagMs === null
        ? null
        : Math.round(this.emaPartialLagMs),
      avgFinalLagMs: this.emaFinalLagMs === null
        ? null
        : Math.round(this.emaFinalLagMs),
      maxPartialLagMs: this.maxPartialLagMs,
      maxFinalLagMs: this.maxFinalLagMs,
      partialSampleCount: this.partialSampleCount,
      finalSampleCount: this.finalSampleCount,
      partialBehindMs,
      finalBehindMs,
      lastPartialEndTime: this.lastPartialEndTime,
      lastFinalEndTime: this.lastFinalEndTime,
      lastPartialAgeMs: this.lastPartialWallAt > 0
        ? Math.max(0, now - this.lastPartialWallAt)
        : null,
      lastFinalAgeMs: this.lastFinalWallAt > 0
        ? Math.max(0, now - this.lastFinalWallAt)
        : null,
      hasActivePartial: this.hasActivePartial,
    })
  }

  /**
   * Measure how far accepted PCM has already advanced past a result's audio
   * endTime when we apply that result to the UI.
   */
  private recordResultLatency(kind: 'partial' | 'final', endTime: number): void {
    if (!Number.isFinite(endTime)) return
    const audioCursor = this.sessionTimelineBase + this.acceptedAudioSeconds
    const lagMs = Math.max(
      0,
      Math.round((audioCursor - endTime) * 1_000),
    )
    const now = this.clock()
    if (kind === 'partial') {
      this.lastPartialLagMs = lagMs
      this.lastPartialEndTime = endTime
      this.lastPartialWallAt = now
      this.hasActivePartial = true
      this.partialSampleCount += 1
      this.emaPartialLagMs = this.emaPartialLagMs === null
        ? lagMs
        : this.emaPartialLagMs * 0.8 + lagMs * 0.2
      this.maxPartialLagMs = Math.max(this.maxPartialLagMs, lagMs)
      return
    }
    this.lastFinalLagMs = lagMs
    this.lastFinalEndTime = endTime
    this.lastFinalWallAt = now
    this.hasActivePartial = false
    this.finalSampleCount += 1
    this.emaFinalLagMs = this.emaFinalLagMs === null
      ? lagMs
      : this.emaFinalLagMs * 0.8 + lagMs * 0.2
    this.maxFinalLagMs = Math.max(this.maxFinalLagMs, lagMs)
  }

  private resetResultLatencyStats(): void {
    this.lastPartialLagMs = null
    this.lastFinalLagMs = null
    this.emaPartialLagMs = null
    this.emaFinalLagMs = null
    this.maxPartialLagMs = 0
    this.maxFinalLagMs = 0
    this.partialSampleCount = 0
    this.finalSampleCount = 0
    this.lastPartialEndTime = null
    this.lastFinalEndTime = null
    this.lastPartialWallAt = 0
    this.lastFinalWallAt = 0
    this.hasActivePartial = false
  }

  destroy(): void {
    if (this.destroyed) return
    this.lifecycleGeneration += 1
    this.desiredSession = false
    this.stopping = false
    this.destroyed = true
    this.clearAllTimers()
    this.settleEnd(new Error('Transcription client was destroyed'))
    this.closeActiveSocket(1000, 'Client destroyed')
    this.clearAudioQueue()
    this.pendingTranscriptPartial = null
    this.pendingTranslationPartials.clear()
    this.setStatus('destroyed')
    this.eventListeners.clear()
    this.subscribers.clear()
  }

  private async performStop(timeoutMs: number): Promise<void> {
    const normalizedTimeout = positiveNumber(timeoutMs, this.stopTimeoutMs, 'timeoutMs')
    const deadline = this.clock() + normalizedTimeout
    this.stopping = true
    this.capturePaused = true
    this.clearReconnectTimer()
    this.setStatus('stopping')
    this.flushPendingAudioFrame()

    try {
      if (!this.recognitionReady || !this.isSocketOpen()) {
        if (this.context === null) {
          await this.connectSocket(true)
        }
        await this.waitForReady(this.remainingTime(deadline))
      }

      await this.flushAudioQueue(this.remainingTime(deadline))
      const context = this.context
      if (!context || !this.recognitionReady || context.socket.readyState !== SOCKET_OPEN) {
        throw new Error('Transcription connection is unavailable while stopping')
      }

      const endPromise = this.createEndWaiter(this.remainingTime(deadline))
      context.socket.send(JSON.stringify({
        message: 'EndOfStream',
        last_seq_no: context.sentAudioChunks,
      }))
      await endPromise

      this.desiredSession = false
      this.stopping = false
      this.recognitionReady = false
      this.discardPendingPartials()
      this.store.clearPartial()
      this.store.clearTranslationPartials()
      this.closeActiveSocket(1000, 'Session complete')
      this.setStatus('stopped')
    } catch (error) {
      if (this.destroyed) throw error
      const message = socketMessage(error, 'Failed to stop transcription session')
      this.desiredSession = false
      this.stopping = false
      this.recognitionReady = false
      this.clearReconnectTimer()
      this.closeActiveSocket(4002, 'Stop failed')
      this.reportError(message, true, error)
      this.setStatus('error')
      throw error
    }
  }

  private async connectSocket(reconnect: boolean): Promise<void> {
    const generation = this.connectionGeneration
    if (!this.desiredSession || this.destroyed) {
      throw new Error('Transcription session is no longer active')
    }

    if (reconnect && this.beforeReconnect) {
      try {
        await this.beforeReconnect()
      } catch (error) {
        if (generation === this.connectionGeneration && error instanceof SpeechmaticsPaymentRequiredError) {
          this.suspendForPayment(error.message)
        }
        throw error
      }
    }

    const token = (await this.tokenProvider()).trim()
    if (!token) throw new Error('Transcription token provider returned an empty token')
    if (!this.desiredSession || this.destroyed || this.paymentBlocked || generation !== this.connectionGeneration) {
      throw new Error('Transcription session ended while refreshing authentication')
    }

    const url = typeof this.url === 'function' ? this.url() : this.url
    if (!url.trim()) throw new Error('Transcription proxy URL must not be empty')
    const socket = this.socketFactory(url, this.protocolFactory(token))
    socket.binaryType = 'arraybuffer'
    const serial = ++this.socketSerial

    let resolveStartup!: () => void
    let rejectStartup!: (error: Error) => void
    const startupPromise = new Promise<void>((resolve, reject) => {
      resolveStartup = resolve
      rejectStartup = reject
    })
    const context: SocketContext = {
      serial,
      socket,
      reconnect,
      timelineOffset: reconnect
        ? this.reconnectTimelineOffset()
        : nonNegativeNumber(
            this.configValue.timeline_offset_seconds,
            0,
            'timeline_offset_seconds',
          ),
      recognitionStarted: false,
      startupSettled: false,
      startupTimer: null,
      resolveStartup,
      rejectStartup,
      messageChain: Promise.resolve(),
      sentAudioChunks: 0,
    }

    this.closeActiveSocket(4001, 'Connection replaced')
    this.context = context
    this.recognitionReady = false
    if (reconnect) this.setStatus('reconnecting')

    context.startupTimer = globalThis.setTimeout(() => {
      const error = new Error('Timed out waiting for transcription recognition to start')
      this.settleStartup(context, error)
      if (this.context === context) {
        try {
          context.socket.close(4000, 'Recognition startup timeout')
        } catch {
          // A timed-out socket may already be closed.
        }
      }
    }, this.startupTimeoutMs)

    socket.onopen = () => {
      if (this.context !== context) return
      try {
        socket.send(JSON.stringify(this.startRecognitionMessage()))
      } catch (error) {
        this.settleStartup(
          context,
          error instanceof Error ? error : new Error('Failed to start recognition'),
        )
      }
    }

    socket.onmessage = (event) => {
      if (this.context !== context) return
      context.messageChain = context.messageChain
        .then(async () => {
          const text = await socketDataToText(event.data)
          const parsed: unknown = JSON.parse(text)
          const message = asRecord(parsed)
          if (!message) throw new TypeError('Transcription message must be an object')
          if (this.context === context) this.handleMessage(context, message)
        })
        .catch((error: unknown) => {
          if (this.destroyed) return
          this.reportError(
            socketMessage(error, 'Failed to parse transcription message'),
            false,
            error,
          )
        })
    }

    socket.onerror = (event) => {
      if (this.context !== context) return
      if (!context.recognitionStarted) {
        this.settleStartup(
          context,
          new Error('Transcription WebSocket connection failed'),
        )
      }
      this.emit('error', {
        message: 'Transcription WebSocket transport error',
        fatal: false,
        cause: event,
      })
    }

    socket.onclose = (event) => {
      this.handleSocketClose(context, event)
    }

    await startupPromise
  }

  private handleSocketClose(
    context: SocketContext,
    event: { readonly code: number; readonly reason: string },
  ): void {
    if (this.context !== context) return
    this.context = null
    this.recognitionReady = false
    const reason = event.reason || `WebSocket closed with code ${event.code}`
    this.settleStartup(context, new Error(reason))

    if (this.paymentBlocked || this.paymentResumeInProgress) return

    if (this.stopping) {
      this.settleEnd(new Error('Connection closed before the final transcript'))
      return
    }
    if (!this.desiredSession || this.destroyed) {
      if (!this.destroyed) this.setStatus('stopped')
      return
    }

    if (!context.recognitionStarted && !context.reconnect) {
      return
    }

    this.reportError(`Connection lost: ${reason}`, false)
    this.scheduleReconnect(reason)
  }

  private handleMessage(
    context: SocketContext,
    message: Record<string, unknown>,
  ): void {
    this.emit('rawMessage', Object.freeze({ ...message }))
    const messageType = asString(message.message)

    switch (messageType) {
      case 'RecognitionStarted':
        this.handleRecognitionStarted(context)
        break
      case 'AddTranscript':
        this.handleTranscript(message, context.timelineOffset, false)
        break
      case 'AddPartialTranscript':
        this.handleTranscript(message, context.timelineOffset, true)
        break
      case 'AddTranslation':
        this.handleTranslation(message, context.timelineOffset, false)
        break
      case 'AddPartialTranslation':
        this.handleTranslation(message, context.timelineOffset, true)
        break
      case 'EndOfTranscript':
        this.discardPendingPartials()
        this.store.clearPartial()
        if (this.store.clearTranslationPartials()) {
          this.emit('translationPartial', null)
        }
        this.settleEnd()
        break
      case 'BalanceUpdated':
        this.emit('balance', parseBalanceUpdate(message))
        break
      case 'Error': {
        const reason = asString(message.reason) || 'Transcription service returned an error'
        if (
          asString(message.type) === 'insufficient_balance'
          || /insufficient balance|balance is insufficient/i.test(reason)
        ) {
          this.suspendForPayment(reason)
          if (this.stopping) this.settleEnd(new Error(reason))
          break
        }
        // The stream was cut on purpose from another device or by an
        // administrator: never reconnect, surface a dedicated event so the
        // recording UI can wind down cleanly.
        if (asString(message.type) === 'stream_terminated') {
          this.desiredSession = false
          if (!context.recognitionStarted) {
            this.settleStartup(context, new Error(reason))
          }
          this.reportError(reason, true)
          // Emitted after the error event so a friendlier terminated-handler
          // message is not overwritten by the raw server reason.
          this.emit('terminated', Object.freeze({ reason }))
          if (this.stopping) this.settleEnd(new Error(reason))
          break
        }
        if (!context.recognitionStarted) {
          this.settleStartup(context, new Error(reason))
        }
        this.reportError(reason, this.stopping)
        if (this.stopping) this.settleEnd(new Error(reason))
        break
      }
      default:
        break
    }
  }

  private handleRecognitionStarted(context: SocketContext): void {
    if (this.context !== context || context.recognitionStarted) return
    context.recognitionStarted = true
    if (context.reconnect) context.timelineOffset = this.reconnectTimelineOffset()
    this.recognitionReady = true
    this.connectionCount += 1
    this.reconnectAttemptValue = 0
    this.errorValue = null
    this.settleStartup(context)

    if (this.stopping) this.setStatus('stopping')
    else this.setStatus(this.capturePaused ? 'paused' : 'running')

    if (context.reconnect) {
      this.emit('reconnected', {
        connectionCount: this.connectionCount,
        timelineOffset: context.timelineOffset,
      })
    }
    this.drainAudioQueue()
  }

  private handleTranscript(
    message: Record<string, unknown>,
    timelineOffset: number,
    partial: boolean,
  ): void {
    const metadata = asRecord(message.metadata)
    if (!metadata) return
    // Chinese/Japanese transcripts arrive word-segmented with spaces; readers
    // of those scripts expect none, and the spaces skew length thresholds.
    const text = normalizeTranscriptText(asString(metadata.transcript) ?? '')
    const speaker = this.transcriptSpeaker(message)
    const { startTime, endTime } = this.timelineRange(
      metadata.start_time,
      metadata.end_time,
      timelineOffset,
    )

    if (partial) {
      this.queueTranscriptPartial({
        speaker,
        text,
        startTime,
        endTime,
        source: 'speechmatics',
      })
      return
    }
    if (!text || this.isDuplicateFinal('transcript', speaker, text, startTime, endTime)) {
      return
    }

    this.discardTranscriptPartial()
    const hadPartial = this.store.getSnapshot().activePartial !== null
    const result = this.store.appendTranscript({
      speaker,
      text,
      startTime,
      endTime,
      source: 'speechmatics',
    })
    this.lastTimelineEnd = Math.max(this.lastTimelineEnd, endTime)
    if (result.inserted) {
      this.recordResultLatency('final', endTime)
      this.emit('transcript', result.record)
    }
    if (hadPartial && this.store.getSnapshot().activePartial === null) {
      this.hasActivePartial = false
      this.emit('partial', null)
    }
  }

  private handleTranslation(
    message: Record<string, unknown>,
    timelineOffset: number,
    partial: boolean,
  ): void {
    const rawResults = Array.isArray(message.results) ? message.results : []
    const fallbackLanguage =
      asString(message.language) ||
      asString(message.target_language) ||
      this.configValue.translation_config?.target_languages[0] ||
      'und'

    for (const rawResult of rawResults) {
      const result = asRecord(rawResult)
      if (!result) continue
      const text = asString(result.content)?.trim() ?? ''
      const speaker = asString(result.speaker)?.trim() || 'Speaker'
      const language =
        asString(result.language) ||
        asString(result.target_language) ||
        fallbackLanguage
      const { startTime, endTime } = this.timelineRange(
        result.start_time,
        result.end_time,
        timelineOffset,
      )

      if (partial) {
        this.queueTranslationPartial({
          speaker,
          language,
          text,
          startTime,
          endTime,
          source: 'speechmatics',
        })
        continue
      }
      if (
        !text ||
        this.isDuplicateFinal('translation', speaker, text, startTime, endTime)
      ) {
        continue
      }

      this.discardTranslationPartial(language, speaker)
      const resultRecord = this.store.appendTranslation({
        speaker,
        language,
        text,
        startTime,
        endTime,
        source: 'speechmatics',
      })
      this.lastTimelineEnd = Math.max(this.lastTimelineEnd, endTime)
      if (resultRecord.inserted) this.emit('translation', resultRecord.record)
    }
  }

  private transcriptSpeaker(message: Record<string, unknown>): string {
    const results = Array.isArray(message.results) ? message.results : []
    const firstResult = asRecord(results[0])
    const alternatives = Array.isArray(firstResult?.alternatives)
      ? firstResult.alternatives
      : []
    const firstAlternative = asRecord(alternatives[0])
    return asString(firstAlternative?.speaker)?.trim() || 'Speaker'
  }

  private queueTranscriptPartial(input: TranscriptPartialInput): void {
    if (!input.text.trim()) {
      this.discardTranscriptPartial()
      this.hasActivePartial = false
      if (this.store.clearPartial()) this.emit('partial', null)
      return
    }

    const elapsed = this.clock() - this.lastPartialAppliedAt
    if (
      this.partialUpdateIntervalMs === 0 ||
      (this.transcriptPartialTimer === null && elapsed >= this.partialUpdateIntervalMs)
    ) {
      this.applyTranscriptPartial(input)
      return
    }

    this.pendingTranscriptPartial = input
    if (this.transcriptPartialTimer !== null) return
    const delay = Math.max(0, this.partialUpdateIntervalMs - Math.max(0, elapsed))
    this.transcriptPartialTimer = globalThis.setTimeout(() => {
      this.transcriptPartialTimer = null
      const pending = this.pendingTranscriptPartial
      this.pendingTranscriptPartial = null
      if (pending) this.applyTranscriptPartial(pending)
    }, delay)
  }

  private applyTranscriptPartial(input: TranscriptPartialInput): void {
    this.lastPartialAppliedAt = this.clock()
    this.recordResultLatency('partial', input.endTime)
    const partial = this.store.setPartial(input)
    this.emit('partial', partial)
  }

  private queueTranslationPartial(input: TranslationPartialInput): void {
    const key = `${input.language.toLowerCase()}\u0000${input.speaker ?? 'Speaker'}`
    if (!input.text.trim()) {
      this.discardTranslationPartial(input.language, input.speaker ?? 'Speaker')
      if (this.store.clearTranslationPartial(input.language, input.speaker)) {
        this.emit('translationPartial', null)
      }
      return
    }
    this.pendingTranslationPartials.set(key, input)
    if (this.partialUpdateIntervalMs === 0) {
      this.flushTranslationPartials()
      return
    }
    if (this.translationPartialTimer !== null) return
    this.translationPartialTimer = globalThis.setTimeout(() => {
      this.translationPartialTimer = null
      this.flushTranslationPartials()
    }, this.partialUpdateIntervalMs)
  }

  private flushTranslationPartials(): void {
    const partials = [...this.pendingTranslationPartials.values()]
    this.pendingTranslationPartials.clear()
    for (const input of partials) {
      const partial = this.store.setTranslationPartial(input)
      this.emit('translationPartial', partial)
    }
  }

  private discardTranscriptPartial(): void {
    this.pendingTranscriptPartial = null
    if (this.transcriptPartialTimer !== null) {
      globalThis.clearTimeout(this.transcriptPartialTimer)
      this.transcriptPartialTimer = null
    }
  }

  private discardTranslationPartial(language: string, speaker: string): void {
    const key = `${language.toLowerCase()}\u0000${speaker}`
    this.pendingTranslationPartials.delete(key)
    if (this.store.clearTranslationPartial(language, speaker)) {
      this.emit('translationPartial', null)
    }
  }

  private discardPendingPartials(): void {
    this.discardTranscriptPartial()
    this.pendingTranslationPartials.clear()
    if (this.translationPartialTimer !== null) {
      globalThis.clearTimeout(this.translationPartialTimer)
      this.translationPartialTimer = null
    }
  }

  private appendAudioFrame(frame: AudioFrame): void {
    this.audioQueue.push(frame)
    this.queuedAudioBytes += frame.data.byteLength
    let dropped = 0

    while (
      this.audioQueue.length > 0 &&
      this.queuedAudioBytes > this.maxQueuedAudioBytes
    ) {
      const removed = this.audioQueue.shift()
      if (!removed) break
      this.queuedAudioBytes -= removed.data.byteLength
      dropped += removed.data.byteLength
    }

    if (dropped > 0) {
      this.droppedAudioBytes += dropped
      this.emit('audioDropped', {
        bytes: dropped,
        totalDroppedBytes: this.droppedAudioBytes,
      })
    }
    this.drainAudioQueue()
  }

  private flushPendingAudioFrame(): void {
    if (this.pendingFrameBytes === 0) return
    if (this.frameTimer !== null) {
      globalThis.clearTimeout(this.frameTimer)
      this.frameTimer = null
    }
    const data = this.pendingFrame.slice(0, this.pendingFrameBytes).buffer
    const frame: AudioFrame = {
      data,
      startTime: this.pendingFrameStartTime,
      endTime: this.acceptedAudioSeconds,
    }
    this.pendingFrame = new Uint8Array(this.frameBytes)
    this.pendingFrameBytes = 0
    this.appendAudioFrame(frame)
  }

  private scheduleFrameFlush(): void {
    if (this.frameTimer !== null) return
    this.frameTimer = globalThis.setTimeout(() => {
      this.frameTimer = null
      this.flushPendingAudioFrame()
    }, this.maxFrameLatencyMs)
  }

  private drainAudioQueue(): void {
    if (!this.recognitionReady || !this.isSocketOpen()) return
    const context = this.context
    const socket = context?.socket
    if (!context || !socket) return

    while (
      this.audioQueue.length > 0 &&
      socket.readyState === SOCKET_OPEN &&
      socket.bufferedAmount < this.highWaterMarkBytes
    ) {
      const frame = this.audioQueue[0]
      if (!frame) break
      try {
        socket.send(frame.data)
        context.sentAudioChunks += 1
      } catch (error) {
        this.reportError('Failed to send an audio frame', false, error)
        try {
          socket.close(1011, 'Audio send failure')
        } catch {
          // onclose/reconnect will handle the transport if possible.
        }
        break
      }
      this.audioQueue.shift()
      this.queuedAudioBytes -= frame.data.byteLength
      this.sentAudioBytes += frame.data.byteLength
    }

    if (this.audioQueue.length === 0) {
      this.queuedAudioBytes = 0
      if (this.drainTimer !== null) {
        globalThis.clearTimeout(this.drainTimer)
        this.drainTimer = null
      }
      return
    }
    this.scheduleDrain()
  }

  private scheduleDrain(): void {
    if (this.drainTimer !== null || !this.recognitionReady || !this.isSocketOpen()) return
    this.drainTimer = globalThis.setTimeout(() => {
      this.drainTimer = null
      this.drainAudioQueue()
    }, this.drainIntervalMs)
  }

  private async flushAudioQueue(timeoutMs: number): Promise<void> {
    const deadline = this.clock() + positiveNumber(timeoutMs, 1, 'timeoutMs')
    this.flushPendingAudioFrame()

    while (this.audioQueue.length > 0) {
      if (!this.recognitionReady || !this.isSocketOpen()) {
        throw new Error('Connection lost while flushing buffered audio')
      }
      this.drainAudioQueue()
      if (this.audioQueue.length === 0) return
      const remaining = this.remainingTime(deadline)
      await this.wait(Math.min(this.drainIntervalMs, remaining))
    }
  }

  private clearAudioQueue(): void {
    this.audioQueue = []
    this.queuedAudioBytes = 0
    this.pendingFrame = new Uint8Array(this.frameBytes)
    this.pendingFrameBytes = 0
  }

  private scheduleReconnect(reason: string, immediate = false): void {
    if (
      this.paymentBlocked ||
      this.paymentResumeInProgress ||
      !this.desiredSession ||
      this.destroyed ||
      this.stopping ||
      this.reconnectTimer !== null ||
      this.context !== null
    ) {
      return
    }
    if (this.reconnectAttemptValue >= this.maxReconnectAttempts) {
      const message = `Failed to reconnect after ${this.maxReconnectAttempts} attempts: ${reason}`
      this.desiredSession = false
      this.reportError(message, true)
      this.setStatus('error')
      return
    }

    this.reconnectAttemptValue += 1
    const delayMs = immediate ? 0 : this.reconnectDelay(this.reconnectAttemptValue)
    this.setStatus('reconnecting')
    this.emit('reconnecting', {
      attempt: this.reconnectAttemptValue,
      maxAttempts: this.maxReconnectAttempts,
      delayMs,
    })
    this.reconnectTimer = globalThis.setTimeout(() => {
      this.reconnectTimer = null
      void this.connectSocket(true).catch((error: unknown) => {
        if (this.destroyed || this.paymentBlocked) return
        const message = socketMessage(error, 'Transcription reconnect failed')
        this.reportError(message, false, error)
        this.scheduleReconnect(message)
      })
    }, delayMs)
  }

  private reconnectDelay(attempt: number): number {
    const exponential = this.reconnectBaseDelayMs * 2 ** Math.max(0, attempt - 1)
    const jitter = this.random() * this.reconnectJitterMs
    return Math.min(this.reconnectMaxDelayMs, exponential + jitter)
  }

  private reconnectTimelineOffset(): number {
    const firstQueuedTime =
      this.audioQueue[0]?.startTime ??
      (this.pendingFrameBytes > 0 ? this.pendingFrameStartTime : undefined)
    return Math.max(
      this.lastTimelineEnd,
      this.sessionTimelineBase + (firstQueuedTime ?? this.acceptedAudioSeconds),
    )
  }

  private timelineRange(
    startValue: unknown,
    endValue: unknown,
    offset: number,
  ): { startTime: number; endTime: number } {
    const start = rawTime(startValue)
    const end = Math.max(start, rawTime(endValue))
    return {
      startTime: offset + start,
      endTime: offset + end,
    }
  }

  private isDuplicateFinal(
    kind: 'transcript' | 'translation',
    speaker: string,
    text: string,
    startTime: number,
    endTime: number,
  ): boolean {
    const key = [
      kind,
      speaker,
      startTime.toFixed(3),
      endTime.toFixed(3),
      text.trim(),
    ].join('\u0000')
    if (this.recentFinalKeys.has(key)) return true
    this.recentFinalKeys.add(key)
    this.recentFinalOrder.push(key)
    if (this.recentFinalOrder.length > MAX_RECENT_FINAL_EVENTS) {
      const expired = this.recentFinalOrder.shift()
      if (expired) this.recentFinalKeys.delete(expired)
    }
    return false
  }

  private startRecognitionMessage(): Record<string, unknown> {
    const audioFormat = {
      type: 'raw',
      encoding: 'pcm_f32le',
      sample_rate: this.sampleRate,
      ...(this.channels === 1 ? {} : { channels: this.channels }),
    }
    const transcriptionConfig = {
      language: this.configValue.language || 'en',
      enable_partials: this.configValue.enable_partials ?? true,
      diarization: this.configValue.diarization || 'speaker',
      operating_point: this.configValue.operating_point || 'enhanced',
      ...(this.configValue.max_delay === undefined
        ? {}
        : { max_delay: this.configValue.max_delay }),
    }
    return {
      message: 'StartRecognition',
      audio_format: audioFormat,
      transcription_config: transcriptionConfig,
      ...(this.configValue.translation_config
        ? {
            translation_config: {
              ...this.configValue.translation_config,
              target_languages: [
                ...this.configValue.translation_config.target_languages,
              ],
            },
          }
        : {}),
    }
  }

  private copyConfig(config: SpeechmaticsProxyConfig): SpeechmaticsProxyConfig {
    return {
      ...config,
      ...(config.audio_format ? { audio_format: { ...config.audio_format } } : {}),
      ...(config.translation_config
        ? {
            translation_config: {
              ...config.translation_config,
              target_languages: [...config.translation_config.target_languages],
            },
          }
        : {}),
    }
  }

  private configureAudio(config: SpeechmaticsProxyConfig): void {
    const sampleRate = positiveNumber(
      config.audio_format?.sample_rate,
      this.configuredSampleRate,
      'audio_format.sample_rate',
    )
    const channels = positiveInteger(
      config.audio_format?.channels,
      this.configuredChannels,
      'audio_format.channels',
    )
    if (config.audio_format?.encoding && config.audio_format.encoding !== 'pcm_f32le') {
      throw new Error(`Unsupported audio encoding: ${config.audio_format.encoding}`)
    }

    this.sampleRate = sampleRate
    this.channels = channels
    this.bytesPerSecond = sampleRate * channels * PCM_F32_BYTES_PER_SAMPLE
    const sampleFrameBytes = channels * PCM_F32_BYTES_PER_SAMPLE
    const targetBytes = Math.round(
      (this.bytesPerSecond * this.frameDurationMs) / 1000,
    )
    this.frameBytes = Math.max(
      sampleFrameBytes,
      Math.round(targetBytes / sampleFrameBytes) * sampleFrameBytes,
    )
    this.maxQueuedAudioBytes =
      this.configuredMaxQueuedAudioBytes ??
      Math.round(this.bytesPerSecond * this.maxQueuedAudioSeconds)
    this.pendingFrame = new Uint8Array(this.frameBytes)
    this.pendingFrameBytes = 0
  }

  private createEndWaiter(timeoutMs: number): Promise<void> {
    if (this.endWaiter) return this.endWaiter.promise
    let resolveWaiter!: () => void
    let rejectWaiter!: (error: Error) => void
    const promise = new Promise<void>((resolve, reject) => {
      resolveWaiter = resolve
      rejectWaiter = reject
    })
    const timer = globalThis.setTimeout(() => {
      this.settleEnd(new Error('Timed out waiting for the final transcript'))
    }, positiveNumber(timeoutMs, 1, 'timeoutMs'))
    this.endWaiter = {
      promise,
      resolve: resolveWaiter,
      reject: rejectWaiter,
      timer,
    }
    return promise
  }

  private settleEnd(error?: Error): void {
    const waiter = this.endWaiter
    if (!waiter) return
    this.endWaiter = null
    globalThis.clearTimeout(waiter.timer)
    if (error) waiter.reject(error)
    else waiter.resolve()
  }

  private settleStartup(context: SocketContext, error?: Error): void {
    if (context.startupSettled) return
    context.startupSettled = true
    if (context.startupTimer !== null) {
      globalThis.clearTimeout(context.startupTimer)
      context.startupTimer = null
    }
    if (error) context.rejectStartup(error)
    else context.resolveStartup()
  }

  private closeActiveSocket(code: number, reason: string): void {
    const context = this.context
    if (!context) return
    this.context = null
    this.recognitionReady = false
    context.socket.onopen = null
    context.socket.onmessage = null
    context.socket.onerror = null
    context.socket.onclose = null
    this.settleStartup(context, new Error(reason))
    if (
      context.socket.readyState === SOCKET_CONNECTING ||
      context.socket.readyState === SOCKET_OPEN
    ) {
      try {
        context.socket.close(code, reason)
      } catch {
        // Closing is best-effort during replacement/cleanup.
      }
    }
  }

  private waitForReady(timeoutMs: number): Promise<void> {
    if (this.recognitionReady && this.isSocketOpen()) return Promise.resolve()
    const normalizedTimeout = positiveNumber(timeoutMs, 1, 'timeoutMs')

    return new Promise((resolve, reject) => {
      let settled = false
      const finish = (error?: Error) => {
        if (settled) return
        settled = true
        globalThis.clearTimeout(timer)
        unsubscribe()
        if (error) reject(error)
        else resolve()
      }
      const unsubscribe = this.subscribe(() => {
        if (this.recognitionReady && this.isSocketOpen()) {
          finish()
          return
        }
        if (
          this.statusValue === 'error' ||
          this.statusValue === 'stopped' ||
          this.statusValue === 'destroyed'
        ) {
          finish(new Error(this.errorValue || 'Transcription connection is unavailable'))
        }
      })
      const timer = globalThis.setTimeout(() => {
        finish(new Error('Timed out waiting for the transcription connection'))
      }, normalizedTimeout)
    })
  }

  private wait(timeoutMs: number): Promise<void> {
    return new Promise((resolve) => {
      globalThis.setTimeout(resolve, Math.max(0, timeoutMs))
    })
  }

  private remainingTime(deadline: number): number {
    const remaining = deadline - this.clock()
    if (remaining <= 0) throw new Error('Transcription operation timed out')
    return remaining
  }

  private isSocketOpen(): boolean {
    return this.context?.socket.readyState === SOCKET_OPEN
  }

  private resetRuntime(): void {
    this.connectionGeneration += 1
    this.paymentBlocked = false
    this.paymentResumeInProgress = false
    this.clearAllTimers()
    this.closeActiveSocket(1000, 'Starting a new session')
    this.clearAudioQueue()
    this.pendingTranscriptPartial = null
    this.pendingTranslationPartials.clear()
    this.recentFinalKeys.clear()
    this.recentFinalOrder.length = 0
    this.acceptedAudioSeconds = 0
    this.sessionTimelineBase = 0
    this.lastTimelineEnd = 0
    this.droppedAudioBytes = 0
    this.sentAudioBytes = 0
    this.connectionCount = 0
    this.reconnectAttemptValue = 0
    this.lastPartialAppliedAt = 0
    this.resetResultLatencyStats()
    this.settleEnd(new Error('A new transcription session was started'))
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer === null) return
    globalThis.clearTimeout(this.reconnectTimer)
    this.reconnectTimer = null
  }

  private clearAllTimers(): void {
    this.clearReconnectTimer()
    if (this.drainTimer !== null) globalThis.clearTimeout(this.drainTimer)
    if (this.frameTimer !== null) globalThis.clearTimeout(this.frameTimer)
    if (this.transcriptPartialTimer !== null) {
      globalThis.clearTimeout(this.transcriptPartialTimer)
    }
    if (this.translationPartialTimer !== null) {
      globalThis.clearTimeout(this.translationPartialTimer)
    }
    this.drainTimer = null
    this.frameTimer = null
    this.transcriptPartialTimer = null
    this.translationPartialTimer = null
  }

  private reportError(message: string, fatal: boolean, cause?: unknown): void {
    this.errorValue = message
    this.publishSnapshot()
    this.emit('error', { message, fatal, cause })
  }

  private setStatus(status: SpeechmaticsClientStatus): void {
    this.statusValue = status
    this.publishSnapshot()
  }

  private publishSnapshot(): void {
    this.clientVersion += 1
    this.snapshotValue = this.buildSnapshot()
    for (const listener of [...this.subscribers]) listener()
    this.emit('state', this.snapshotValue)
  }

  private buildSnapshot(): SpeechmaticsClientSnapshot {
    return Object.freeze({
      version: this.clientVersion,
      status: this.statusValue,
      error: this.errorValue,
      reconnectAttempt: this.reconnectAttemptValue,
      maxReconnectAttempts: this.maxReconnectAttempts,
      connected: this.recognitionReady && this.isSocketOpen(),
      acceptingAudio:
        this.desiredSession && !this.capturePaused && !this.stopping && !this.destroyed,
      startedAt: this.startedAtValue,
    })
  }

  private emit<EventName extends keyof SpeechmaticsClientEventMap>(
    eventName: EventName,
    payload: SpeechmaticsClientEventMap[EventName],
  ): void {
    const listeners = this.eventListeners.get(eventName)
    if (!listeners) return
    for (const listener of [...listeners]) listener(payload)
  }

  private assertUsable(): void {
    if (this.destroyed) throw new Error('Transcription client has been destroyed')
  }
}
