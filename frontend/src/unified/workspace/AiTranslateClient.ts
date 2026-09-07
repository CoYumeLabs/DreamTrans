import { endsSentence, joinSegmentTexts, textWeight } from '../../core/transcription/scriptText'
import { messages } from '../../i18n'

/**
 * Context-aware AI translation over the backend /ws/translate endpoint.
 *
 * The client batches provider micro-finals into sentence-sized chunks aligned
 * with the display cards. New servers accept each chunk as an atomic,
 * idempotent request and echo its stable request ID. Older servers are used
 * only after an explicit init acknowledgement without the new capabilities;
 * a missing acknowledgement is treated as a broken connection, never legacy.
 * The server keeps a rolling context window across chunks, so translations see
 * the conversation history the way the old Classic UI did.
 */

export interface AiTranslateSegmentInput {
  id: string
  speaker: string
  text: string
  startTime: number
  endTime: number
}

export interface AiTranslateChunk {
  requestId: string
  segmentIds: readonly string[]
  speaker: string
  text: string
  startTime: number
  endTime: number
}

export interface AiTranslationResult {
  text: string
  model?: string
  latencyMs?: number
}

export interface AiTranslateBalanceEvent {
  /** Charge for the segment just settled, in USD. */
  costUsd: number
  /** Raw account balance object from the server; shape owned by the backend. */
  balance: unknown
}

export interface AiTranslateSessionConfig {
  /** Cloud session UUID for billing/RAG attribution; omit for local sessions. */
  sessionId?: string
  /** Custom translate system prompt; empty uses the server default. */
  translatePrompt?: string
  /** Source (spoken) language code; lets the server pick a default prompt. */
  sourceLanguage?: string
  /** Target language code; lets the server pick a default prompt. */
  targetLanguage?: string
}

interface AiTranslateSocket {
  readyState: number
  send(data: string): void
  close(code?: number, reason?: string): void
  onopen: ((event: unknown) => void) | null
  onmessage: ((event: { data: unknown }) => void) | null
  onclose: ((event: { code: number; reason: string }) => void) | null
  onerror: ((event: unknown) => void) | null
}

export interface AiTranslateClientOptions {
  url: string | (() => string)
  tokenProvider: () => Promise<string>
  protocolFactory: (token: string) => readonly string[]
  socketFactory?: (url: string, protocols: readonly string[]) => AiTranslateSocket
  onTranslation: (chunk: AiTranslateChunk, result: AiTranslationResult) => void
  onError?: (message: string) => void
  onPaymentRequired?: () => void
  onRecovered?: () => void
  onChunkError?: (chunk: AiTranslateChunk, message: string) => void
  /** Per-segment settlement: the charge just made and the account balance. */
  onBalance?: (event: AiTranslateBalanceEvent) => void
  /** Flush an unfinished sentence after this idle period. */
  idleFlushMs?: number
  /**
   * Sentence-punctuation chunks lighter than this wait for more text. Measured
   * in Latin-equivalent characters (`textWeight`), so a short Chinese sentence
   * is not held back the way its raw character count would suggest.
   */
  minChunkChars?: number
  /** Total queued + submitted chunks retained for reliable matching. */
  maxPendingChunks?: number
  /** Maximum requests submitted concurrently to an ID-capable server. */
  maxInFlightChunks?: number
  reconnectDelaysMs?: readonly number[]
  /** Maximum wait for an access token before this connection attempt fails. */
  tokenTimeoutMs?: number
  /** Maximum wait for the WebSocket open event. */
  connectTimeoutMs?: number
  /** Maximum wait for an explicit translator init acknowledgement. */
  handshakeTimeoutMs?: number
  /** Maximum stop/drain wait; must cover the server's end-to-end budget. */
  drainTimeoutMs?: number
  /** Bounded server-processing retries per request (disconnect replay is separate). */
  processingRetryLimit?: number
}

const SOCKET_OPEN = 1
// Above the display model's 3.5s mid-sentence pause tolerance, so a thinking
// pause neither splits the card nor splits the translation paragraph.
const DEFAULT_IDLE_FLUSH_MS = 4_000
const DEFAULT_MIN_CHUNK_CHARS = 12
const DEFAULT_MAX_PENDING = 8
const DEFAULT_MAX_IN_FLIGHT = 8
const DEFAULT_RECONNECT_DELAYS = [1_000, 2_000, 4_000, 8_000, 15_000] as const
const DEFAULT_TOKEN_TIMEOUT_MS = 10_000
const DEFAULT_CONNECT_TIMEOUT_MS = 15_000
const DEFAULT_HANDSHAKE_TIMEOUT_MS = 10_000
const DEFAULT_DRAIN_TIMEOUT_MS = 150_000
const MAX_SERVER_WORKERS = 8
const MIN_RETRY_DELAY_MS = 250
const MAX_RETRY_DELAY_MS = 30_000
const MAX_PROCESSING_RETRIES = 8
const ACTIONABLE_ERROR_TYPES = new Set([
  'quota_exhausted',
  'quota_temporarily_unavailable',
  'insufficient_balance',
  'pricing_unavailable',
  'billing_temporarily_unavailable',
  'accounting_uncertain',
])
const TERMINAL_ACCOUNT_ERROR_TYPES = new Set([
  'quota_exhausted',
  'insufficient_balance',
  'pricing_unavailable',
])

interface BufferedChunk {
  cardId: string
  speaker: string
  segmentIds: string[]
  text: string
  startTime: number
  endTime: number
}

function defaultSocketFactory(
  url: string,
  protocols: readonly string[],
): AiTranslateSocket {
  return new WebSocket(url, [...protocols]) as unknown as AiTranslateSocket
}

function withTimeout<T>(
  promise: Promise<T>,
  timeoutMs: number,
  timeoutMessage: string,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = globalThis.setTimeout(() => {
      reject(new Error(timeoutMessage))
    }, timeoutMs)
    promise.then(
      (value) => {
        globalThis.clearTimeout(timer)
        resolve(value)
      },
      (reason: unknown) => {
        globalThis.clearTimeout(timer)
        reject(reason)
      },
    )
  })
}

export class AiTranslateClient {
  private readonly url: string | (() => string)
  private readonly tokenProvider: () => Promise<string>
  private readonly protocolFactory: (token: string) => readonly string[]
  private readonly socketFactory: (
    url: string,
    protocols: readonly string[],
  ) => AiTranslateSocket
  private readonly onTranslation: (
    chunk: AiTranslateChunk,
    result: AiTranslationResult,
  ) => void
  private readonly onError?: (message: string) => void
  private readonly onPaymentRequired?: () => void
  private readonly onRecovered?: () => void
  private readonly onChunkError?: (chunk: AiTranslateChunk, message: string) => void
  private readonly onBalance?: (event: AiTranslateBalanceEvent) => void
  private readonly idleFlushMs: number
  private readonly minChunkChars: number
  private readonly maxPendingChunks: number
  private readonly maxInFlightChunks: number
  private readonly reconnectDelaysMs: readonly number[]
  private readonly tokenTimeoutMs: number
  private readonly connectTimeoutMs: number
  private readonly handshakeTimeoutMs: number
  private readonly drainTimeoutMs: number
  private readonly processingRetryLimit: number

  private socket: AiTranslateSocket | null = null
  private socketSerial = 0
  private connectInProgress = false
  private socketReady = false
  private protocolReady = false
  private supportsRequestIds = false
  private serverWorkers = 1
  private active = false
  private draining = false
  private destroyed = false
  private sessionConfig: AiTranslateSessionConfig = {}
  private requestNamespace = ''
  private requestSequence = 0
  private buffer: BufferedChunk | null = null
  private readonly pending = new Map<string, AiTranslateChunk>()
  private readonly inFlight = new Set<string>()
  private readonly retryNotBefore = new Map<string, number>()
  private readonly processingRetryCounts = new Map<string, number>()
  private idleTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private closeTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private reconnectTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private connectTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private handshakeTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private retryTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private drainTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private drainPromise: Promise<boolean> | null = null
  private resolveDrain: ((completed: boolean) => void) | null = null
  private reconnectAttempt = 0
  private terminalBlock: { type: string; message: string } | null = null
  private stickyTypedError: {
    message: string
    clearsOnSuccess: boolean
    requestId?: string
  } | null = null

  constructor(options: AiTranslateClientOptions) {
    this.url = options.url
    this.tokenProvider = options.tokenProvider
    this.protocolFactory = options.protocolFactory
    this.socketFactory = options.socketFactory ?? defaultSocketFactory
    this.onTranslation = options.onTranslation
    this.onError = options.onError
    this.onRecovered = options.onRecovered
    this.onPaymentRequired = options.onPaymentRequired
    this.onChunkError = options.onChunkError
    this.onBalance = options.onBalance
    this.idleFlushMs = options.idleFlushMs ?? DEFAULT_IDLE_FLUSH_MS
    this.minChunkChars = options.minChunkChars ?? DEFAULT_MIN_CHUNK_CHARS
    this.maxPendingChunks = options.maxPendingChunks ?? DEFAULT_MAX_PENDING
    this.maxInFlightChunks = Math.max(
      1,
      Math.min(options.maxInFlightChunks ?? DEFAULT_MAX_IN_FLIGHT, this.maxPendingChunks),
    )
    this.reconnectDelaysMs = options.reconnectDelaysMs ?? DEFAULT_RECONNECT_DELAYS
    this.tokenTimeoutMs = Math.max(1, options.tokenTimeoutMs ?? DEFAULT_TOKEN_TIMEOUT_MS)
    this.connectTimeoutMs = Math.max(1, options.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS)
    this.handshakeTimeoutMs = Math.max(
      1,
      options.handshakeTimeoutMs ?? DEFAULT_HANDSHAKE_TIMEOUT_MS,
    )
    this.drainTimeoutMs = Math.max(
      1,
      options.drainTimeoutMs ?? DEFAULT_DRAIN_TIMEOUT_MS,
    )
    this.processingRetryLimit = Math.max(
      1,
      Math.min(10, options.processingRetryLimit ?? MAX_PROCESSING_RETRIES),
    )
  }

  /** True while a live translation session can accept segments. */
  isSessionActive(): boolean {
    return this.active && !this.destroyed && !this.draining && !this.terminalBlock
  }

  suspendForPayment(): void {
    if (!this.active) return
    this.blockForTerminalError('insufficient_balance', messages().workspace.runtime.insufficientBalance)
  }

  startSession(config: AiTranslateSessionConfig): void {
    if (this.destroyed) return
    if (this.draining) this.finishDrain(false)
    this.terminalBlock = null
    this.stickyTypedError = null
    this.reconnectAttempt = 0
    this.sessionConfig = { ...config }
    this.active = true
    this.draining = false
    this.reconnectAttempt = 0
    this.requestNamespace = crypto.randomUUID().replaceAll('-', '')
    this.requestSequence = 0
    this.clearTimer('close')
    this.clearTimer('drain')
    this.clearTimer('retry')
    // A previous session's unmatched chunks must not consume new results.
    this.buffer = null
    this.pending.clear()
    this.inFlight.clear()
    this.retryNotBefore.clear()
    this.processingRetryCounts.clear()
    if (this.socketReady && this.socket) this.beginHandshake(this.socket)
    else this.connect()
  }

  /**
   * Feed one finalized transcript fragment. Fragments buffer per display card
   * and flush as complete sentences, on card change, or after an idle pause.
   */
  addSegment(segment: AiTranslateSegmentInput, cardId: string): void {
    if (!this.active || this.destroyed || this.terminalBlock) return
    const text = segment.text.trim()
    if (!text) return

    if (this.buffer && this.buffer.cardId !== cardId) this.flushBuffer()

    if (this.buffer) {
      this.buffer.segmentIds.push(segment.id)
      this.buffer.text = joinSegmentTexts(this.buffer.text, text)
      this.buffer.endTime = segment.endTime
    } else {
      this.buffer = {
        cardId,
        speaker: segment.speaker,
        segmentIds: [segment.id],
        text,
        startTime: segment.startTime,
        endTime: segment.endTime,
      }
    }

    if (
      endsSentence(this.buffer.text)
      && textWeight(this.buffer.text) >= this.minChunkChars
    ) {
      this.flushBuffer()
    } else {
      this.restartIdleTimer()
    }
  }

  /** Flush the open sentence buffer immediately (pause/stop). */
  flush(): void {
    this.flushBuffer()
  }

  /**
   * Ends the translation session. The socket lingers briefly so in-flight
   * translations still arrive for the final sentences.
   */
  stopSession(): Promise<boolean> {
    if (this.destroyed) return Promise.resolve(false)
    if (this.drainPromise) return this.drainPromise
    this.flushBuffer()
    this.active = false
    this.draining = true
    this.clearTimer('idle')
    this.drainPromise = new Promise<boolean>((resolve) => {
      this.resolveDrain = resolve
    })
    const drainPromise = this.drainPromise
    if (this.protocolReady) {
      // One final barrier is enough. ID-capable servers submit every chunk
      // atomically; legacy servers use this to release their final buffer.
      this.sendJSON({ type: 'flush' })
    }
    if (this.pending.size === 0) {
      this.finishDrain(this.terminalBlock === null)
    } else {
      if (!this.socketReady) this.connect()
      this.drainTimer = globalThis.setTimeout(() => {
        this.drainTimer = null
        for (const chunk of this.pending.values()) {
          this.reportChunkError(chunk, messages().workspace.runtime.translation.drainTimeout)
        }
        this.pending.clear()
        this.inFlight.clear()
        this.retryNotBefore.clear()
        this.processingRetryCounts.clear()
        this.clearTimer('retry')
        this.finishDrain(false)
      }, this.drainTimeoutMs)
    }
    return drainPromise
  }

  destroy(): void {
    if (this.destroyed) return
    this.destroyed = true
    this.active = false
    this.draining = false
    this.clearTimer('idle')
    this.clearTimer('close')
    this.clearTimer('reconnect')
    this.clearTimer('connect')
    this.clearTimer('handshake')
    this.clearTimer('retry')
    this.clearTimer('drain')
    this.resolveDrain?.(false)
    this.resolveDrain = null
    this.drainPromise = null
    this.closeSocket()
    this.buffer = null
    this.pending.clear()
    this.inFlight.clear()
    this.retryNotBefore.clear()
    this.processingRetryCounts.clear()
  }

  getDiagnostics(): { pendingChunks: number; bufferedChars: number } {
    return {
      pendingChunks: this.pending.size,
      bufferedChars: this.buffer?.text.length ?? 0,
    }
  }

  private connect(): void {
    if (this.destroyed || this.terminalBlock || this.socket || this.connectInProgress) return
    this.connectInProgress = true
    const serial = ++this.socketSerial
    void (async () => {
      let socket: AiTranslateSocket
      try {
        const token = (
          await withTimeout(
            this.tokenProvider(),
            this.tokenTimeoutMs,
            messages().workspace.runtime.translation.tokenTimeout,
          )
        ).trim()
        if (this.destroyed || serial !== this.socketSerial) {
          return
        }
        const url = typeof this.url === 'function' ? this.url() : this.url
        socket = this.socketFactory(url, this.protocolFactory(token))
      } catch (reason) {
        if (serial !== this.socketSerial) return
        this.connectInProgress = false
        this.reportError(messages().workspace.runtime.translation.initFailed(
          reason instanceof Error ? reason.message : String(reason),
        ))
        this.scheduleReconnect()
        return
      }

      this.connectInProgress = false
      this.socket = socket
      socket.onopen = () => {
        if (this.socket !== socket) return
        this.clearTimer('connect')
        this.socketReady = true
        this.beginHandshake(socket)
      }
      socket.onmessage = (event) => {
        if (this.socket !== socket) return
        this.handleMessage(event.data)
      }
      socket.onclose = () => {
        if (this.socket !== socket) return
        this.handleSocketDisconnect(socket)
      }
      socket.onerror = () => {
        // onclose follows and owns the retry policy.
      }
      this.connectTimer = globalThis.setTimeout(() => {
        this.connectTimer = null
        if (this.socket !== socket || this.socketReady) return
        this.reportError(messages().workspace.runtime.translation.socketTimeout)
        this.abandonSocket(socket, 'WebSocket open timeout')
      }, this.connectTimeoutMs)
    })()
  }

  private beginHandshake(socket: AiTranslateSocket): void {
    if (this.socket !== socket || !this.socketReady) return
    this.protocolReady = false
    this.supportsRequestIds = false
    this.serverWorkers = 1
    this.inFlight.clear()
    this.clearTimer('handshake')
    if (!this.sendInit()) {
      this.abandonSocket(socket, 'Translator init send failed')
      return
    }
    this.handshakeTimer = globalThis.setTimeout(() => {
      this.handshakeTimer = null
      if (this.socket !== socket || this.protocolReady) return
      this.reportError(messages().workspace.runtime.translation.handshakeTimeout)
      // Never guess that a silent peer is legacy: doing so would submit
      // unidentifiable work that cannot be retried safely.
      this.abandonSocket(socket, 'Translator handshake timeout')
    }, this.handshakeTimeoutMs)
  }

  private sendInit(): boolean {
    const config: Record<string, unknown> = {
      // Chunks arrive pre-batched; the server-side atomic protocol passes them
      // through as-is.
      min_chunk_chars: 1,
      disable_summarization: true,
      disable_embeddings: true,
    }
    if (this.sessionConfig.sessionId) config.session_id = this.sessionConfig.sessionId
    const prompt = this.sessionConfig.translatePrompt?.trim()
    if (prompt) config.translate_prompt = prompt
    const sourceLanguage = this.sessionConfig.sourceLanguage?.trim()
    if (sourceLanguage) config.source_language = sourceLanguage
    const targetLanguage = this.sessionConfig.targetLanguage?.trim()
    if (targetLanguage) config.target_language = targetLanguage
    return this.sendJSON({ type: 'init', mode: 'ai_rolling', config })
  }

  private flushBuffer(): void {
    this.clearTimer('idle')
    const buffered = this.buffer
    if (!buffered) return
    this.buffer = null
    if (this.terminalBlock) return
    const chunk: AiTranslateChunk = {
      requestId: this.nextRequestId(),
      segmentIds: buffered.segmentIds,
      speaker: buffered.speaker,
      text: buffered.text,
      startTime: buffered.startTime,
      endTime: buffered.endTime,
    }
    if (this.pending.size >= this.maxPendingChunks) {
      this.reportChunkError(
        chunk,
        messages().workspace.runtime.translation.backlogLimit,
      )
      return
    }
    this.pending.set(chunk.requestId, chunk)
    if (this.protocolReady) this.sendAvailable()
    else this.connect()
  }

  private sendChunk(chunk: AiTranslateChunk): boolean {
    const sentTranscript = this.sendJSON({
      type: 'transcript',
      payload: {
        ...(this.supportsRequestIds ? { request_id: chunk.requestId } : {}),
        speaker: chunk.speaker,
        transcript: chunk.text,
        start_time: chunk.startTime,
        end_time: chunk.endTime,
      },
    })
    if (!sentTranscript) return false
    // Legacy servers do not understand atomic request IDs. Keep one request in
    // flight and force its paragraph boundary, preserving old deployments.
    if (!this.supportsRequestIds) return this.sendJSON({ type: 'flush' })
    return true
  }

  private sendAvailable(): void {
    if (!this.protocolReady || !this.socketReady) return
    const limit = this.supportsRequestIds
      ? Math.min(this.maxInFlightChunks, this.serverWorkers)
      : 1
    const now = Date.now()
    for (const chunk of this.pending.values()) {
      if (this.inFlight.size >= limit) break
      if (this.inFlight.has(chunk.requestId)) continue
      const retryAt = this.retryNotBefore.get(chunk.requestId)
      if (retryAt !== undefined && retryAt > now) continue
      this.retryNotBefore.delete(chunk.requestId)
      if (!this.sendChunk(chunk)) break
      this.inFlight.add(chunk.requestId)
    }
    this.scheduleRetryWakeup()
    if (this.draining && this.pending.size === 0) this.finishDrain(true)
  }

  private handleMessage(data: unknown): void {
    if (typeof data !== 'string') return
    let payload: {
      message?: string
      type?: string
      reason?: string
      request_id?: string
      workers?: number
      retry_after_ms?: number
      retryable?: boolean
      connection_terminal?: boolean
      cost?: number
      cost_usd?: number
      balance?: unknown
      capabilities?: {
        request_ids?: boolean
        atomic_transcripts?: boolean
        async_flush?: boolean
      }
      results?: Array<{
        request_id?: string
        content?: string
        start_time?: number
        end_time?: number
        model?: string
        latency_ms?: number
      }>
    }
    try {
      payload = JSON.parse(data) as typeof payload
    } catch {
      return
    }

    switch (payload.message) {
      case 'Info': {
        if (payload.reason !== 'translator initialized') break
        this.clearTimer('handshake')
        this.protocolReady = true
        this.supportsRequestIds = (
          payload.capabilities?.request_ids === true
          && payload.capabilities?.atomic_transcripts === true
        )
        this.serverWorkers = this.supportsRequestIds
          ? Math.max(
              1,
              Math.min(
                MAX_SERVER_WORKERS,
                typeof payload.workers === 'number' && Number.isFinite(payload.workers)
                  ? Math.floor(payload.workers)
                  : 1,
              ),
            )
          : 1
        this.sendAvailable()
        if (this.draining) this.sendJSON({ type: 'flush' })
        break
      }
      case 'AddTranslation': {
        for (const result of payload.results ?? []) {
          const text = (result.content ?? '').trim()
          const chunk = this.takeMatchingChunk(
            result.request_id,
            result.start_time,
            result.end_time,
          )
          if (!chunk) continue
          this.inFlight.delete(chunk.requestId)
          if (!text) {
            this.reportChunkError(chunk, messages().workspace.runtime.translation.empty)
            continue
          }
          this.onTranslation(chunk, {
            text,
            ...(result.model ? { model: result.model } : {}),
            ...(result.latency_ms === undefined ? {} : { latencyMs: result.latency_ms }),
          })
          this.markRequestSucceeded(chunk.requestId)
        }
        this.afterPendingChanged()
        break
      }
      case 'BalanceUpdated': {
        const costUsd = typeof payload.cost_usd === 'number'
          ? payload.cost_usd
          : typeof payload.cost === 'number' ? payload.cost : 0
        this.onBalance?.({ costUsd, balance: payload.balance ?? null })
        break
      }
      case 'Error': {
        if (this.terminalBlock) break
        const rawErrorType = (payload.type ?? '').trim()
        const copy = messages().workspace.runtime.translation
        const rawReasonText = payload.reason ?? copy.unknown
        const legacyDisabledConnection = (
          rawErrorType === ''
          && rawReasonText.trim().toLowerCase()
            === 'paid features are disabled for this connection'
        )
        const legacyBillingError = rawErrorType === 'billing_error' || legacyDisabledConnection
        const legacyQuotaError = rawErrorType === 'quota_error'
        const errorType = legacyBillingError
          ? 'billing_temporarily_unavailable'
          : legacyQuotaError
            ? 'quota_temporarily_unavailable'
            : rawErrorType
        const reasonText = legacyBillingError
          ? copy.billingReconnect(rawReasonText)
          : legacyQuotaError
            ? copy.quotaReconnect(rawReasonText)
            : rawReasonText
        const reason = copy.failed(reasonText)
        const retryable = (
          payload.retryable === true
          || errorType === 'translation_processing'
          || legacyBillingError
          || legacyQuotaError
        )
        const connectionTerminal = (
          payload.connection_terminal === true
          || legacyBillingError
          || legacyQuotaError
        )
        const retryAfterMs = (
          (legacyBillingError || legacyQuotaError)
          && payload.retry_after_ms === undefined
        )
          ? MIN_RETRY_DELAY_MS
          : payload.retry_after_ms
        const reconnectAfterMs = (legacyBillingError || legacyQuotaError)
          ? 0
          : this.normalizedRetryDelay(retryAfterMs, 0)
        const actionable = ACTIONABLE_ERROR_TYPES.has(errorType)
        const terminalAccountError = (
          !retryable
          && (connectionTerminal || TERMINAL_ACCOUNT_ERROR_TYPES.has(errorType))
        )

        if (terminalAccountError) {
          this.blockForTerminalError(errorType || 'connection_terminal', reason)
          if (errorType === 'insufficient_balance') this.onPaymentRequired?.()
          break
        }

        if (retryable) {
          if (actionable) {
            this.reportTypedError(reason, true, payload.request_id)
          }
          if (payload.request_id) {
            const chunk = this.pending.get(payload.request_id)
            if (chunk) {
              this.deferChunkRetry(chunk, reasonText, retryAfterMs)
            } else if (!connectionTerminal) {
              break
            }
          } else if (connectionTerminal) {
            this.deferAllPending(retryAfterMs)
          }
          if (connectionTerminal) {
            const socket = this.socket
            if (socket) {
              this.abandonSocket(
                socket,
                `Retryable ${errorType || 'server'} error`,
                reconnectAfterMs,
              )
            } else {
              this.scheduleReconnect(reconnectAfterMs)
            }
          }
          break
        }

        const chunk = this.takeErrorChunk(payload.request_id)
        if (chunk) {
          this.inFlight.delete(chunk.requestId)
          if (actionable) {
            this.reportChunkOnly(chunk, reason)
            this.reportTypedError(`${reason}（${chunk.requestId}）`, false)
          } else {
            this.reportChunkError(chunk, reason)
          }
          this.afterPendingChanged()
        } else if (payload.request_id) {
          // Late duplicate from a previous socket generation; the request was
          // already resolved locally, so it must not surface as a new failure.
          break
        } else {
          if (actionable) this.reportTypedError(reason, false)
          else this.reportError(reason)
        }
        break
      }
      default:
        break
    }
  }

  /**
   * Results arrive in send order; the oldest pending chunk is the expected
   * match. Time bounds guard against drift after an error was dropped by an
   * intermediary.
   */
  private takeMatchingChunk(
    requestId?: string,
    startTime?: number,
    endTime?: number,
  ): AiTranslateChunk | null {
    if (this.pending.size === 0) return null
    if (requestId) {
      const exact = this.pending.get(requestId)
      if (!exact) return null
      this.pending.delete(requestId)
      this.retryNotBefore.delete(requestId)
      this.processingRetryCounts.delete(requestId)
      return exact
    }
    if (typeof startTime === 'number' && typeof endTime === 'number') {
      for (const candidate of this.pending.values()) {
        if (
          Math.abs(candidate.startTime - startTime) < 0.75
          && Math.abs(candidate.endTime - endTime) < 0.75
        ) {
          this.pending.delete(candidate.requestId)
          this.retryNotBefore.delete(candidate.requestId)
          this.processingRetryCounts.delete(candidate.requestId)
          return candidate
        }
      }
    }
    // A result without a request ID is necessarily from a legacy server. That
    // mode deliberately permits only one request in flight.
    const legacyRequestId = this.inFlight.values().next().value as string | undefined
    if (!legacyRequestId) return null
    const legacyChunk = this.pending.get(legacyRequestId) ?? null
    if (legacyChunk) {
      this.pending.delete(legacyRequestId)
      this.retryNotBefore.delete(legacyRequestId)
      this.processingRetryCounts.delete(legacyRequestId)
    }
    return legacyChunk
  }

  private takeErrorChunk(requestId?: string): AiTranslateChunk | null {
    if (requestId) {
      const exact = this.pending.get(requestId) ?? null
      if (exact) {
        this.pending.delete(requestId)
        this.retryNotBefore.delete(requestId)
        this.processingRetryCounts.delete(requestId)
      }
      return exact
    }
    if (this.supportsRequestIds) return null
    const legacyRequestId = this.inFlight.values().next().value as string | undefined
    if (!legacyRequestId) return null
    const legacyChunk = this.pending.get(legacyRequestId) ?? null
    if (legacyChunk) {
      this.pending.delete(legacyRequestId)
      this.retryNotBefore.delete(legacyRequestId)
      this.processingRetryCounts.delete(legacyRequestId)
    }
    return legacyChunk
  }

  private afterPendingChanged(): void {
    if (this.pending.size === 0) {
      this.clearTimer('retry')
      if (this.draining) this.finishDrain(true)
      return
    }
    this.sendAvailable()
  }

  private normalizedRetryDelay(value: number | undefined, fallbackMs: number): number {
    const candidate = typeof value === 'number' && Number.isFinite(value)
      ? Math.floor(value)
      : fallbackMs
    return Math.max(0, Math.min(MAX_RETRY_DELAY_MS, candidate))
  }

  private deferChunkRetry(
    chunk: AiTranslateChunk,
    reason: string,
    retryAfterMs?: number,
  ): void {
    this.inFlight.delete(chunk.requestId)
    const retryCount = (this.processingRetryCounts.get(chunk.requestId) ?? 0) + 1
    if (retryCount > this.processingRetryLimit) {
      this.pending.delete(chunk.requestId)
      this.retryNotBefore.delete(chunk.requestId)
      this.processingRetryCounts.delete(chunk.requestId)
      this.clearRetryableError(chunk.requestId)
      this.reportChunkError(
        chunk,
        messages().workspace.runtime.translation.retryExhausted(
          reason || messages().workspace.runtime.translation.unavailable,
        ),
      )
      this.afterPendingChanged()
      return
    }
    this.processingRetryCounts.set(chunk.requestId, retryCount)
    const baseRetryDelay = Math.max(
      MIN_RETRY_DELAY_MS,
      this.normalizedRetryDelay(retryAfterMs, 1_500),
    )
    const retryDelay = Math.min(
      MAX_RETRY_DELAY_MS,
      baseRetryDelay * 2 ** (retryCount - 1),
    )
    this.retryNotBefore.set(chunk.requestId, Date.now() + retryDelay)
    this.scheduleRetryWakeup()
  }

  private deferAllPending(retryAfterMs?: number): void {
    const retryAt = Date.now() + Math.max(
      MIN_RETRY_DELAY_MS,
      this.normalizedRetryDelay(retryAfterMs, 1_500),
    )
    for (const requestId of this.pending.keys()) {
      this.retryNotBefore.set(
        requestId,
        Math.max(this.retryNotBefore.get(requestId) ?? 0, retryAt),
      )
    }
    this.scheduleRetryWakeup()
  }

  private blockForTerminalError(type: string, message: string): void {
    if (this.terminalBlock || this.destroyed) return
    this.terminalBlock = { type, message }
    this.clearTimer('idle')
    this.clearTimer('retry')
    this.clearTimer('reconnect')
    this.buffer = null
    const pending = [...this.pending.values()]
    this.pending.clear()
    this.inFlight.clear()
    this.retryNotBefore.clear()
    this.processingRetryCounts.clear()
    for (const chunk of pending) this.reportChunkOnly(chunk, message)
    const wasDraining = this.draining
    this.closeSocket()
    this.reportTypedError(message, false, undefined, true)
    if (wasDraining) this.finishDrain(false)
  }

  private markRequestSucceeded(requestId: string): void {
    this.reconnectAttempt = 0
    this.clearRetryableError(requestId)
  }

  private clearRetryableError(requestId: string): void {
    const sticky = this.stickyTypedError
    if (
      sticky?.clearsOnSuccess
      && (sticky.requestId === undefined || sticky.requestId === requestId)
    ) {
      this.stickyTypedError = null
      this.onRecovered?.()
    }
  }

  private scheduleRetryWakeup(): void {
    this.clearTimer('retry')
    const now = Date.now()
    let earliest = Number.POSITIVE_INFINITY
    for (const [requestId, retryAt] of this.retryNotBefore) {
      if (!this.pending.has(requestId)) {
        this.retryNotBefore.delete(requestId)
        continue
      }
      if (retryAt > now) earliest = Math.min(earliest, retryAt)
    }
    if (!Number.isFinite(earliest)) return
    this.retryTimer = globalThis.setTimeout(() => {
      this.retryTimer = null
      this.sendAvailable()
    }, Math.max(0, earliest - Date.now()))
  }

  private nextRequestId(): string {
    this.requestSequence += 1
    return `tr_${this.requestNamespace}_${this.requestSequence.toString(36)}`
  }

  private restartIdleTimer(): void {
    this.clearTimer('idle')
    this.idleTimer = globalThis.setTimeout(() => {
      this.idleTimer = null
      this.flushBuffer()
    }, this.idleFlushMs)
  }

  private scheduleReconnect(minimumDelayMs = 0): void {
    if (
      this.reconnectTimer !== null
      || this.destroyed
      || !this.shouldMaintainConnection()
    ) return
    const delay = this.reconnectDelaysMs[
      Math.min(this.reconnectAttempt, this.reconnectDelaysMs.length - 1)
    ] ?? 15_000
    if (this.reconnectAttempt === this.reconnectDelaysMs.length + 2) {
      this.reportError(messages().workspace.runtime.translation.unstable)
    }
    this.reconnectAttempt += 1
    const jitter = delay <= 0 ? 0 : Math.round(delay * (Math.random() * 0.4 - 0.2))
    const reconnectDelay = Math.max(
      this.normalizedRetryDelay(minimumDelayMs, 0),
      Math.max(0, delay + jitter),
    )
    this.reconnectTimer = globalThis.setTimeout(() => {
      this.reconnectTimer = null
      if (this.shouldMaintainConnection()) this.connect()
    }, reconnectDelay)
  }

  private scheduleClose(delayMs: number): void {
    this.clearTimer('close')
    if (delayMs <= 0) {
      this.closeSocket()
      return
    }
    this.closeTimer = globalThis.setTimeout(() => {
      this.closeTimer = null
      if (!this.active) this.closeSocket()
    }, delayMs)
  }

  private abandonSocket(
    socket: AiTranslateSocket,
    reason: string,
    minimumReconnectDelayMs = 0,
  ): void {
    if (this.socket !== socket) return
    this.socket = null
    this.socketSerial += 1
    this.connectInProgress = false
    this.socketReady = false
    this.protocolReady = false
    this.supportsRequestIds = false
    this.serverWorkers = 1
    this.inFlight.clear()
    this.clearTimer('connect')
    this.clearTimer('handshake')
    socket.onopen = null
    socket.onmessage = null
    socket.onclose = null
    socket.onerror = null
    try {
      socket.close(4000, reason.slice(0, 120))
    } catch {
      // Socket may still be constructing or already closed.
    }
    if (this.shouldMaintainConnection()) this.scheduleReconnect(minimumReconnectDelayMs)
  }

  private handleSocketDisconnect(socket: AiTranslateSocket): void {
    if (this.socket !== socket) return
    const wasLegacy = this.protocolReady && !this.supportsRequestIds
    this.socket = null
    this.socketSerial += 1
    this.connectInProgress = false
    this.socketReady = false
    this.protocolReady = false
    this.supportsRequestIds = false
    this.serverWorkers = 1
    this.clearTimer('connect')
    this.clearTimer('handshake')

    let lostLegacyWork = false
    if (wasLegacy) {
      // A legacy request has no stable server-side ID. Resending it after a
      // disconnect can duplicate provider work and misattribute a later result.
      for (const requestId of this.inFlight) {
        const chunk = this.pending.get(requestId)
        if (!chunk) continue
        lostLegacyWork = true
        this.pending.delete(requestId)
        this.retryNotBefore.delete(requestId)
        this.processingRetryCounts.delete(requestId)
        this.reportChunkError(
          chunk,
          messages().workspace.runtime.translation.legacyInterrupted,
        )
      }
    }
    this.inFlight.clear()
    if (lostLegacyWork && this.draining && this.pending.size === 0) {
      this.finishDrain(false)
      return
    }
    if (this.shouldMaintainConnection()) this.scheduleReconnect()
  }

  private closeSocket(): void {
    const socket = this.socket
    this.socket = null
    this.socketSerial += 1
    this.connectInProgress = false
    this.socketReady = false
    this.protocolReady = false
    this.supportsRequestIds = false
    this.serverWorkers = 1
    this.inFlight.clear()
    this.clearTimer('connect')
    this.clearTimer('handshake')
    this.clearTimer('reconnect')
    if (!socket) return
    socket.onopen = null
    socket.onmessage = null
    socket.onclose = null
    socket.onerror = null
    try {
      socket.close(1000, 'Session complete')
    } catch {
      // Socket may already be closed.
    }
  }

  private sendJSON(value: unknown): boolean {
    const socket = this.socket
    if (!socket || socket.readyState !== SOCKET_OPEN) return false
    try {
      socket.send(JSON.stringify(value))
      return true
    } catch (reason) {
      this.reportError(messages().workspace.runtime.translation.sendFailed(
        reason instanceof Error ? reason.message : String(reason),
      ))
      return false
    }
  }

  private shouldMaintainConnection(): boolean {
    return (
      !this.destroyed
      && this.terminalBlock === null
      && (this.active || (this.draining && this.pending.size > 0))
    )
  }

  private finishDrain(completed: boolean): void {
    this.draining = false
    this.clearTimer('drain')
    const resolve = this.resolveDrain
    this.resolveDrain = null
    this.drainPromise = null
    this.scheduleClose(0)
    resolve?.(completed)
  }

  private reportError(message: string): void {
    if (!this.destroyed && !this.stickyTypedError) this.onError?.(message)
  }

  private reportTypedError(
    message: string,
    clearsOnSuccess: boolean,
    requestId?: string,
    replaceRetryable = false,
  ): void {
    if (this.destroyed) return
    if (
      this.stickyTypedError
      && (!replaceRetryable || !this.stickyTypedError.clearsOnSuccess)
    ) return
    this.stickyTypedError = {
      message,
      clearsOnSuccess,
      ...(requestId ? { requestId } : {}),
    }
    this.onError?.(message)
  }

  private reportChunkOnly(chunk: AiTranslateChunk, message: string): void {
    if (!this.destroyed) this.onChunkError?.(chunk, message)
  }

  private reportChunkError(chunk: AiTranslateChunk, message: string): void {
    if (this.destroyed) return
    this.reportChunkOnly(chunk, message)
    this.reportError(`${message}（${chunk.requestId}）`)
  }

  private clearTimer(
    kind: 'idle' | 'close' | 'reconnect' | 'connect' | 'handshake' | 'retry' | 'drain',
  ): void {
    const key = kind === 'idle'
      ? 'idleTimer' as const
      : kind === 'close'
        ? 'closeTimer' as const
        : kind === 'reconnect'
          ? 'reconnectTimer' as const
          : kind === 'connect'
            ? 'connectTimer' as const
            : kind === 'handshake'
              ? 'handshakeTimer' as const
              : kind === 'retry'
                ? 'retryTimer' as const
                : 'drainTimer' as const
    const timer = this[key]
    if (timer !== null) {
      globalThis.clearTimeout(timer)
      this[key] = null
    }
  }
}
