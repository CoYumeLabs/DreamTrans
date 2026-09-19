import assert from 'node:assert/strict'
import { EdgeAudioBuffer, RegionalEdgeSocket, type EdgeAuthorization } from './RegionalEdge'

class FakeNativeSocket {
  static instances: FakeNativeSocket[] = []
  readyState = 1
  bufferedAmount = 0
  binaryType: BinaryType = 'arraybuffer'
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  onclose: ((event: { code: number; reason: string }) => void) | null = null
  sent: (string | Uint8Array)[] = []
  failSend = false
  readonly url: string
  readonly protocols: string
  constructor(url: string, protocols: string) { this.url = url; this.protocols = protocols; FakeNativeSocket.instances.push(this) }
  send(data: string | Uint8Array) { if (this.failSend) throw new Error('disconnected'); this.sent.push(data) }
  close(code = 1000, reason = '') { this.readyState = 3; this.onclose?.({ code, reason }) }
  message(value: unknown) { this.onmessage?.({ data: JSON.stringify(value) }) }
}

const original = globalThis.WebSocket
Object.defineProperty(globalThis, 'WebSocket', { configurable: true, writable: true, value: FakeNativeSocket })
try {
  const authorization = (generation: number, previousAudio = 0): EdgeAuthorization => ({
    endpoint: 'https://edge.example.test', token: 'short-grant',
    grant: { protocol: 2, session_id: 'session', generation, previous_generation: generation - 1,
      previous_audio_sequence: previousAudio, durable_audio_sequence: previousAudio, sample_rate: 44100 },
  })
  const buffer = new EdgeAudioBuffer()
  const first = new RegionalEdgeSocket(authorization(1), buffer)
  const native = FakeNativeSocket.instances.at(-1)!
  native.onopen?.(new Event('open'))
  assert.equal(native.url, 'wss://edge.example.test/ws/edge')
  assert.equal(native.sent[0], '{"token":"short-grant"}')
  native.message({ message: 'RecognitionStarted' })
  first.send(new Float32Array([1, 2]).buffer)
  first.send(new Float32Array([3, 4]).buffer)
  assert.equal(buffer.bytes, 16)
  native.message({ message: 'EdgeReceived', sequence: 2 })
  assert.equal(buffer.bytes, 16, 'receipt must never release replay audio')
  native.message({ message: 'EdgeSaved', generation: 0, audio_sequence: 2 })
  assert.equal(buffer.bytes, 16, 'stale connection must not release audio')
  first.close()
  // A failed provider handshake must retain the unconfirmed tail.
  const retry = new RegionalEdgeSocket(authorization(2, 1), buffer)
  assert.equal(buffer.frames.length, 1)
  assert.equal(buffer.bytes, 8)
  retry.close()
  const third = new RegionalEdgeSocket(authorization(3, 1), buffer)
  const thirdNative = FakeNativeSocket.instances.at(-1)!
  thirdNative.message({ message: 'RecognitionStarted' })
  assert.equal(buffer.frames.length, 1)
  assert.equal(buffer.frames[0].generation, 3)
  assert.equal(buffer.bytes, 8, 'replay must not duplicate retained bytes')
  const replay = thirdNative.sent[0] as Uint8Array
  assert.equal(new DataView(replay.buffer).getBigUint64(0), 2n)
  assert.deepEqual([...new Float32Array(replay.buffer.slice(8))], [3, 4])
  thirdNative.message({ message: 'EdgeSaved', generation: 3, audio_sequence: 2 })
  assert.equal(buffer.bytes, 0)
  third.send(new Float32Array([5]).buffer)
  third.close()
  // A send failure during replay retains both sent and unsent frames for handoff.
  new RegionalEdgeSocket(authorization(4, 2), buffer)
  const fourthNative = FakeNativeSocket.instances.at(-1)!
  fourthNative.failSend = true
  fourthNative.message({ message: 'RecognitionStarted' })
  assert.equal(buffer.bytes, 4)
  assert.equal(buffer.frames[0].generation, 4)
  assert.equal(fourthNative.readyState, 3)
  const missingBuffer = new EdgeAudioBuffer()
  assert.throws(() => new RegionalEdgeSocket(authorization(2, 1), missingBuffer), /恢复缓冲/)
  const legacy = authorization(1)
  legacy.grant.protocol = 1
  const legacyBuffer = new EdgeAudioBuffer()
  const legacySocket = new RegionalEdgeSocket(legacy, legacyBuffer)
  FakeNativeSocket.instances.at(-1)!.message({ message: 'RecognitionStarted' })
  legacySocket.send(new Float32Array([9]).buffer)
  const legacyNext = authorization(2)
  legacyNext.grant.protocol = 1
  new RegionalEdgeSocket(legacyNext, legacyBuffer)
  const legacyNative = FakeNativeSocket.instances.at(-1)!
  legacyNative.message({ message: 'RecognitionStarted' })
  assert.equal(new DataView((legacyNative.sent[0] as Uint8Array).buffer).getBigUint64(0), 1n)
  console.log('Regional Edge replay and credential boundary verification passed')
} finally {
  Object.defineProperty(globalThis, 'WebSocket', { configurable: true, writable: true, value: original })
}
