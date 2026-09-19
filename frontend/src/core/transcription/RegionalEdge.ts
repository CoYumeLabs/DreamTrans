import type { SpeechmaticsSocket } from './SpeechmaticsProxyClient'

export interface EdgeAuthorization {
  endpoint: string
  token: string
  grant: {
    session_id: string
    generation: number
    previous_generation: number
    previous_audio_sequence: number
    durable_audio_sequence?: number
    protocol?: number
    sample_rate: number
  }
}
interface Frame { generation: number; sequence: number; data: ArrayBuffer }

/** Retains only a bounded, not-yet-main-acknowledged audio window for this tab. */
export class EdgeAudioBuffer {
  frames: Frame[] = []
  session = ''
  bytes = 0
  sequence = 0
  reset(session: string) { this.frames = []; this.bytes = 0; this.sequence = 0; this.session = session }
  acknowledge(sequence: number, generation?: number) {
    this.frames = this.frames.filter(frame => {
      if (frame.sequence <= sequence && (generation === undefined || frame.generation === generation)) {
        this.bytes -= frame.data.byteLength
        return false
      }
      return true
    })
  }
}

/** Grants travel in the first WS message, never in a URL or main login token. */
export class RegionalEdgeSocket implements SpeechmaticsSocket {
  readonly native: WebSocket
  onopen: SpeechmaticsSocket['onopen'] = null
  onmessage: SpeechmaticsSocket['onmessage'] = null
  onerror: SpeechmaticsSocket['onerror'] = null
  onclose: SpeechmaticsSocket['onclose'] = null
  private sequence = 0
  private authorization: EdgeAuthorization
  private buffer: EdgeAudioBuffer
  constructor(authorization: EdgeAuthorization, buffer: EdgeAudioBuffer) {
    this.authorization = authorization
    this.buffer = buffer
    const { grant } = authorization
    if (buffer.session !== grant.session_id) {
      if (grant.protocol === 2 && grant.previous_generation > 0) {
        throw new Error('此页面已没有上一段录音的恢复缓冲，请新建录音；已保存字幕仍在历史记录中')
      }
      buffer.reset(grant.session_id)
    }
    if (grant.protocol === 2) {
      buffer.acknowledge(grant.durable_audio_sequence ?? 0)
      buffer.sequence = Math.max(buffer.sequence, grant.durable_audio_sequence ?? 0)
    } else {
      buffer.acknowledge(grant.previous_audio_sequence, grant.previous_generation)
    }
    this.sequence = buffer.sequence
    this.native = new WebSocket(authorization.endpoint.replace(/^https:/, 'wss:') + '/ws/edge', 'dreamtrans-edge-v1')
    this.native.binaryType = 'arraybuffer'
    this.native.onopen = event => {
      this.native.send(JSON.stringify({ token: authorization.token }))
      this.onopen?.(event)
    }
    this.native.onerror = event => this.onerror?.(event)
    this.native.onclose = event => this.onclose?.(event)
    this.native.onmessage = event => {
      let message: Record<string, unknown>
      try { message = JSON.parse(String(event.data)) as Record<string, unknown> } catch { return }
      if (message.message === 'EdgeSaved') {
        if (Number(message.generation) === grant.generation) {
          buffer.acknowledge(Number(message.audio_sequence), grant.protocol === 2 ? undefined : grant.generation)
        }
      }
      if (message.message === 'RecognitionStarted') {
        // Frame identifiers survive provider reconnection and failed handshakes.
        // Only the main site's final-result checkpoint releases replay bytes.
        buffer.frames = buffer.frames.map((frame, index) => ({
          ...frame, generation: grant.generation,
          sequence: grant.protocol === 2 ? frame.sequence : index + 1,
        }))
        if (grant.protocol !== 2) buffer.sequence = buffer.frames.length
        this.sequence = buffer.sequence
        try {
          for (const frame of buffer.frames) this.sendFrame(frame.sequence, frame.data)
        } catch {
          this.native.close(4009, 'Audio replay interrupted')
          return
        }
      }
      this.onmessage?.(event)
    }
  }
  get readyState() { return this.native.readyState }
  get bufferedAmount() { return this.native.bufferedAmount }
  get binaryType() { return this.native.binaryType }
  set binaryType(value: BinaryType) { this.native.binaryType = value }
  send(data: string | ArrayBuffer | ArrayBufferView | Blob) {
    if (typeof data === 'string') { this.native.send(data); return }
    if (!(data instanceof ArrayBuffer)) throw new Error('Edge requires PCM ArrayBuffer frames')
    const maximum = this.authorization.grant.sample_rate * 4 * 30
    if (this.buffer.bytes + data.byteLength > maximum) {
      this.native.close(4008, 'Unacknowledged audio buffer reached 30 seconds')
      throw new Error('主站未确认的音频已达到 30 秒，请等待恢复连接')
    }
    const sequence = this.sequence + 1
    this.sequence = sequence
    this.buffer.sequence = sequence
    this.buffer.frames.push({ generation: this.authorization.grant.generation, sequence, data })
    this.buffer.bytes += data.byteLength
    this.sendFrame(sequence, data)
  }
  private sendFrame(sequence: number, data: ArrayBuffer) {
    const wire = new Uint8Array(data.byteLength + 8)
    new DataView(wire.buffer).setBigUint64(0, BigInt(sequence))
    wire.set(new Uint8Array(data), 8)
    this.native.send(wire)
  }
  close(code?: number, reason?: string) { this.native.close(code, reason) }
}
