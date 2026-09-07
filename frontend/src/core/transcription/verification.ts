/**
 * Dependency-free executable invariants for the transcription core.
 *
 * Run from frontend/:
 *   esbuild src/core/transcription/verification.ts --bundle --platform=node \
 *     --format=esm --outfile=/tmp/dreamtrans-transcription-verify.mjs
 *   node /tmp/dreamtrans-transcription-verify.mjs
 */
import {
  SpeechmaticsProxyClient,
  SpeechmaticsPaymentRequiredError,
  TranscriptStore,
  type SpeechmaticsSocket,
} from './index'

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(`Transcription core verification failed: ${message}`)
}

function nextTurn(): Promise<void> {
  return new Promise((resolve) => {
    globalThis.setTimeout(resolve, 0)
  })
}

class FakeSocket implements SpeechmaticsSocket {
  readyState = 0
  bufferedAmount = 0
  binaryType: BinaryType = 'blob'
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: { readonly data: unknown }) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  onclose:
    | ((event: {
        readonly code: number
        readonly reason: string
        readonly wasClean?: boolean
      }) => void)
    | null = null
  readonly sent: Array<string | ArrayBuffer | ArrayBufferView | Blob> = []

  open(): void {
    this.readyState = 1
    this.onopen?.(new Event('open'))
  }

  message(payload: Record<string, unknown>): void {
    this.onmessage?.({ data: JSON.stringify(payload) })
  }

  send(data: string | ArrayBuffer | ArrayBufferView | Blob): void {
    if (this.readyState !== 1) throw new Error('Fake socket is not open')
    this.sent.push(data)
    if (typeof data === 'string') {
      const payload = JSON.parse(data) as { message?: string; last_seq_no?: unknown }
      if (payload.message === 'EndOfStream') {
        // Speechmatics rejects EndOfStream without last_seq_no matching the
        // number of binary AddAudio messages sent on this connection.
        const binaryFrames = this.sent
          .filter((item) => typeof item !== 'string').length
        if (payload.last_seq_no !== binaryFrames) {
          throw new Error(
            `EndOfStream last_seq_no must equal ${binaryFrames} sent audio chunks, `
            + `got ${String(payload.last_seq_no)}`,
          )
        }
        globalThis.queueMicrotask(() => {
          this.message({ message: 'EndOfTranscript' })
        })
      }
    }
  }

  close(code = 1000, reason = ''): void {
    if (this.readyState >= 2) return
    this.readyState = 3
    this.onclose?.({ code, reason, wasClean: code === 1000 })
  }

  fail(code = 1006, reason = 'network failure'): void {
    this.readyState = 3
    this.onclose?.({ code, reason, wasClean: false })
  }
}

async function verifyStore(): Promise<void> {
  let now = 1_000
  const store = new TranscriptStore({ clock: () => now })
  const initialSnapshot = store.getSnapshot()
  assert(initialSnapshot === store.getSnapshot(), 'snapshot identity must be cached')

  const first = store.appendTranscript({
    speaker: 'S1',
    text: 'Hello world.',
    startTime: 0,
    endTime: 1,
  })
  const duplicate = store.appendTranscript({
    speaker: 'S1',
    text: 'Hello world.',
    startTime: 0,
    endTime: 1,
  })
  assert(first.inserted, 'first final transcript must append')
  assert(!duplicate.inserted, 'identical final transcript must deduplicate')
  assert(first.record.id === duplicate.record.id, 'stable ids must survive duplicate delivery')
  assert(store.getSnapshot().stats.transcriptWordCount === 2, 'word stats must be incremental')

  now += 1
  store.setPartial({
    speaker: 'S1',
    text: 'Next',
    startTime: 1.2,
    endTime: 1.5,
  })
  const partialId = store.getSnapshot().activePartial?.id
  now += 1
  store.setPartial({
    speaker: 'S1',
    text: 'Next sentence',
    startTime: 1.2,
    endTime: 1.9,
  })
  assert(store.getSnapshot().activePartial?.id === partialId, 'partial revisions must merge')
  assert(store.getSnapshot().segmentCount === 1, 'partial must not enter final history')

  const second = store.appendTranscript({
    speaker: 'S1',
    text: 'Next sentence.',
    startTime: 1.2,
    endTime: 2,
  })
  assert(store.getSnapshot().activePartial === null, 'matching final must clear partial')
  const translation = store.appendTranslation({
    speaker: 'S1',
    language: 'cmn',
    text: '下一句。',
    startTime: 1.2,
    endTime: 2,
  })
  assert(
    translation.record.segmentId === second.record.id,
    'translation must link through the time index',
  )
  assert(
    store.getLatestTranslationForSegment(second.record.id, 'cmn')?.id ===
      translation.record.id,
    'translation lookup must be normalized by segment and language',
  )
  const earlyTranslation = store.appendTranslation({
    segmentId: null,
    speaker: 'S2',
    language: 'cmn',
    text: '先到的翻译。',
    startTime: 3,
    endTime: 4,
  })
  const third = store.appendTranscript({
    speaker: 'S2',
    text: 'Translation arrived first.',
    startTime: 3,
    endTime: 4,
  })
  const linked = store.relinkTranslation(earlyTranslation.record.id, third.record.id)
  assert(linked?.segmentId === third.record.id, 'orphan translation must be relinkable')
  assert(
    store.getLatestTranslationForSegment(third.record.id, 'cmn')?.id === linked.id,
    'relinked translation must enter the normalized lookup',
  )
  assert(store.getSegments({ offset: 1, limit: 1 })[0]?.id === second.record.id, 'range read')
}

async function verifyClient(): Promise<void> {
  const sockets: FakeSocket[] = []
  const socketProtocols: string[][] = []
  let reconnectTimelineOffset = 0
  const client = new SpeechmaticsProxyClient({
    tokenProvider: () => 'fresh-token',
    url: 'ws://dreamtrans.test/ws/speechmatics',
    socketFactory: (_url, protocols) => {
      const socket = new FakeSocket()
      sockets.push(socket)
      socketProtocols.push([...protocols])
      return socket
    },
    reconnect: {
      maxAttempts: 2,
      baseDelayMs: 1,
      maxDelayMs: 1,
      jitterMs: 0,
    },
    audio: {
      frameDurationMs: 40,
      maxFrameLatencyMs: 5,
      highWaterMarkBytes: 1_024,
      drainIntervalMs: 1,
    },
    partialUpdateIntervalMs: 0,
  })
  client.on('reconnected', (event) => {
    reconnectTimelineOffset = event.timelineOffset
  })

  const startPromise = client.start({
    language: 'en',
    timeline_offset_seconds: 120,
    translation_config: { target_languages: ['cmn'], enable_partials: true },
  })
  await nextTurn()
  const firstSocket = sockets[0]
  assert(firstSocket, 'start must create a socket')
  assert(
    socketProtocols[0]?.join(',') ===
      'dreamtrans.v1,dreamtrans.jwt.fresh-token',
    'authenticated sockets must offer the stable app protocol before the JWT',
  )
  firstSocket.open()
  const startMessage = JSON.parse(String(firstSocket.sent[0])) as Record<string, unknown>
  assert(
    !('timeline_offset_seconds' in startMessage),
    'client-only timeline offset must not be sent upstream',
  )
  firstSocket.message({ message: 'RecognitionStarted' })
  await startPromise
  assert(client.getSnapshot().status === 'running', 'recognition must enter running state')

  firstSocket.message({
    message: 'AddPartialTranscript',
    metadata: { transcript: 'Testing', start_time: 0, end_time: 0.5 },
    results: [{ alternatives: [{ speaker: 'S1' }] }],
  })
  await nextTurn()
  assert(client.store.getSnapshot().activePartial?.text === 'Testing', 'client partial ingest')

  const finalMessage = {
    message: 'AddTranscript',
    metadata: { transcript: 'Testing one two.', start_time: 0, end_time: 1 },
    results: [{ alternatives: [{ speaker: 'S1' }] }],
  }
  firstSocket.message(finalMessage)
  firstSocket.message(finalMessage)
  await nextTurn()
  assert(client.store.getSnapshot().segmentCount === 1, 'client final deduplication')
  assert(
    client.store.getSegmentAt(0)?.startTime === 120,
    'continued session must offset the initial socket timeline',
  )

  firstSocket.message({
    message: 'AddTranslation',
    results: [
      {
        content: '测试一二。',
        speaker: 'S1',
        language: 'cmn',
        start_time: 0,
        end_time: 1,
      },
    ],
  })
  await nextTurn()
  assert(client.store.getSnapshot().translationCount === 1, 'client translation ingest')

  const workletChunk = new Float32Array(128)
  for (let index = 0; index < 15; index += 1) client.sendAudio(workletChunk)
  const initialBinaryFrames = firstSocket.sent.filter(
    (item) => item instanceof ArrayBuffer,
  )
  assert(initialBinaryFrames.length === 1, 'small worklet chunks must coalesce to one frame')
  const liveDiag = client.getDiagnostics()
  assert(
    typeof liveDiag.outboundQueueMs === 'number' && liveDiag.outboundQueueMs >= 0,
    'diagnostics must report non-negative outbound queue latency',
  )
  assert(liveDiag.bytesPerSecond > 0, 'diagnostics must expose bytesPerSecond')
  assert(
    typeof liveDiag.finalBehindMs === 'number' && liveDiag.finalBehindMs >= 0,
    'diagnostics must report final recognition lag',
  )
  assert(
    'lastPartialLagMs' in liveDiag && 'avgFinalLagMs' in liveDiag,
    'diagnostics must expose partial/final latency samples',
  )
  for (let index = 0; index < 735; index += 1) client.sendAudio(workletChunk)

  firstSocket.fail()
  const reconnectAudio = new Float32Array(1_920)
  client.sendAudio(reconnectAudio)
  await new Promise((resolve) => {
    globalThis.setTimeout(resolve, 5)
  })
  const reconnectSocket = sockets[1]
  assert(reconnectSocket, 'unexpected close must create a reconnect socket')
  assert(
    socketProtocols[1]?.join(',') ===
      'dreamtrans.v1,dreamtrans.jwt.fresh-token',
    'reconnects must preserve authenticated protocol negotiation',
  )
  reconnectSocket.open()
  reconnectSocket.message({ message: 'RecognitionStarted' })
  await nextTurn()
  assert(client.getSnapshot().status === 'running', 'successful reconnect must recover state')
  assert(
    reconnectTimelineOffset >= 121.9,
    'continued-session reconnect must include base offset plus accepted audio',
  )
  assert(
    reconnectSocket.sent.some((item) => item instanceof ArrayBuffer),
    'reconnect must flush framed audio',
  )

  await client.stop()
  assert(client.getSnapshot().status === 'stopped', 'stop must await EndOfTranscript')
  client.destroy()
  assert(client.getSnapshot().status === 'destroyed', 'destroy must be terminal')
}

async function verifyCancelledStart(): Promise<void> {
  let releaseToken!: (token: string) => void
  const token = new Promise<string>((resolve) => {
    releaseToken = resolve
  })
  const sockets: FakeSocket[] = []
  const client = new SpeechmaticsProxyClient({
    tokenProvider: () => token,
    url: 'ws://dreamtrans.test/ws/speechmatics',
    socketFactory: () => {
      const socket = new FakeSocket()
      sockets.push(socket)
      return socket
    },
  })

  const cancelledStart = client.start().then(
    () => false,
    () => true,
  )
  await nextTurn()
  await client.stop()
  assert(client.getSnapshot().status === 'stopped', 'stop must cancel a pending start')
  releaseToken('late-token')
  assert(await cancelledStart, 'cancelled start must reject its original caller')
  await nextTurn()
  assert(sockets.length === 0, 'late authentication must not resurrect a socket')
  assert(client.getSnapshot().status === 'stopped', 'late start result must not overwrite stop')
  client.destroy()
}

await verifyStore()
await verifyClient()
await verifyCancelledStart()

async function verifyPaymentPause(): Promise<void> {
  for (const legacy of [false, true]) {
    const sockets: FakeSocket[] = []
    let affordable = true
    let paymentEvents = 0
    const client = new SpeechmaticsProxyClient({
      url: 'ws://dreamtrans.test/ws/speechmatics',
      tokenProvider: () => 'token',
      beforeReconnect: async () => {
        if (!affordable) throw new SpeechmaticsPaymentRequiredError('insufficient balance')
      },
      socketFactory: () => {
        const socket = new FakeSocket()
        sockets.push(socket)
        return socket
      },
      reconnect: { baseDelayMs: 1, maxDelayMs: 1, jitterMs: 0 },
      audio: { sampleRate: 48_000, maxQueuedAudioSeconds: 30 },
    })
    client.on('paymentRequired', () => { paymentEvents += 1 })
    const start = client.start()
    await nextTurn()
    sockets[0].open()
    sockets[0].message({ message: 'RecognitionStarted' })
    await start
    client.store.appendTranscript({ text: 'Keep this final.', startTime: 0, endTime: 1 })
    // Hold unsent PCM behind network backpressure when the balance runs out.
    sockets[0].bufferedAmount = 1_000_000
    client.sendAudio(new Float32Array(48_000))
    sockets[0].message({
      message: 'Error',
      type: legacy ? 'proxy_error' : 'insufficient_balance',
      reason: legacy ? 'usage charge failed or balance is insufficient' : 'insufficient balance',
    })
    await nextTurn()
    affordable = false
    assert(paymentEvents === 1, 'balance failure must emit one payment pause')
    assert(client.getSnapshot().status === 'paused', 'balance failure must pause')
    assert(!client.sendAudio(new Float32Array(48_000)), 'payment pause must reject new PCM')
    const buffered = client.getDiagnostics().queuedAudioBytes
    assert(buffered > 0, 'payment pause must retain unsent audio')
    await new Promise(resolve => setTimeout(resolve, 20))
    assert(sockets.length === 1, 'payment pause must not reconnect automatically')
    await client.resume().then(
      () => { throw new Error('unpaid resume must fail') },
      () => {},
    )
    assert(client.getSnapshot().status === 'paused', 'unpaid resume must stay paused')
    assert(sockets.length === 1, 'failed balance preflight must not open an upstream socket')
    affordable = true
    const resume = client.resume()
    await nextTurn()
    sockets[1].open()
    sockets[1].message({ message: 'RecognitionStarted' })
    await resume
    assert(client.getSnapshot().status === 'running', 'explicit paid resume must work')
    assert(client.store.getSnapshot().segmentCount === 1, 'resume must retain prior finals')
    assert(client.getDiagnostics().queuedAudioBytes === 0, 'resume must drain retained PCM')
    // A network loss followed by HTTP 402 must also hold, even without a WS error.
    affordable = false
    sockets[1].fail()
    await new Promise(resolve => setTimeout(resolve, 20))
    assert(client.getSnapshot().status === 'paused', 'reconnect preflight 402 must pause')
    const countBeforeStop = sockets.length
    await client.stop()
    assert(sockets.length === countBeforeStop, 'stopping while unpaid must not reconnect')
    client.destroy()
  }
}

await verifyPaymentPause()

async function verifyCancelledPaymentResume(): Promise<void> {
  const sockets: FakeSocket[] = []
  let rejectPreflight!: (error: Error) => void
  const client = new SpeechmaticsProxyClient({
    url: 'ws://dreamtrans.test/ws/speechmatics',
    tokenProvider: () => 'token',
    beforeReconnect: () => new Promise<void>((_resolve, reject) => { rejectPreflight = reject }),
    socketFactory: () => {
      const socket = new FakeSocket()
      sockets.push(socket)
      return socket
    },
  })
  const first = client.start()
  await nextTurn()
  sockets[0].open()
  sockets[0].message({ message: 'RecognitionStarted' })
  await first
  client.suspendForPayment()
  const resume = client.resume().catch(() => undefined)
  await nextTurn()
  await client.stop()
  const next = client.start()
  await nextTurn()
  sockets[1].open()
  sockets[1].message({ message: 'RecognitionStarted' })
  await next
  rejectPreflight(new SpeechmaticsPaymentRequiredError('late balance rejection'))
  await resume
  assert(client.getSnapshot().status === 'running', 'a cancelled payment check must not pause a new recording')
  assert(sockets.length === 2, 'a cancelled resume must not resurrect an old connection')
  client.destroy()
}

await verifyCancelledPaymentResume()

async function verifyStartupPaymentRejection(): Promise<void> {
  const socket = new FakeSocket()
  const client = new SpeechmaticsProxyClient({
    url: 'ws://dreamtrans.test/ws/speechmatics',
    tokenProvider: () => 'token',
    socketFactory: () => socket,
  })
  const result = client.start().then(() => null, (error: unknown) => error)
  await nextTurn()
  socket.open()
  socket.message({ message: 'Error', type: 'insufficient_balance', reason: 'insufficient balance' })
  const error = await result
  assert(error instanceof Error && error.message === 'insufficient balance', 'a startup balance race must preserve the actionable error')
  client.destroy()
}

await verifyStartupPaymentRejection()
