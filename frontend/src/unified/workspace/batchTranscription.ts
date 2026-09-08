import { authFetch, createSession, getStoredUser, saveTranscriptsBatch, updateSession, type TranscriptInput } from '../../pro/api/auth'

export const MAX_BATCH_BYTES = 100 * 1024 * 1024
export interface BatchQuote { reservation_usd: number; affordable: boolean }
export interface BatchTranscript {
  metadata: { duration: number; language?: string }
  results: Array<{ type: string; start_time: number; end_time: number; alternatives: Array<{ content: string; speaker?: string }> }>
}
export interface BatchResponse { server_managed?: boolean; session_id?: string; job_id: string; status: string; error?: string; transcript?: BatchTranscript }

export function quoteBatch(seconds: number): Promise<BatchQuote> {
  return authFetch(`/api/transcribe/batch/quote?duration_seconds=${seconds}`)
}
export function submitBatch(audio: Blob, language: string, requestId: string, title: string): Promise<BatchResponse> {
  const form = new FormData()
  form.append('request_id', requestId)
  form.append('title', title)
  form.append('audio', audio, 'audio.wav')
  form.append('config', JSON.stringify({ language, diarization: 'speaker', operating_point: 'enhanced' }))
  return authFetch('/api/transcribe/batch/submit?audio_format=pcm16', { method: 'POST', body: form }, [], 300_000)
}
export function batchStatus(jobId: string): Promise<BatchResponse> {
  return authFetch(`/api/transcribe/batch/status?job_id=${encodeURIComponent(jobId)}`)
}

// Decode locally, mixing to mono and resampling before upload. The server can
// derive the upper billing duration from this exact canonical PCM container.
export async function prepareBatchAudio(file: File): Promise<{ audio: Blob; seconds: number }> {
  if (file.size === 0 || file.size > MAX_BATCH_BYTES) throw new Error('size')
  const context = new AudioContext({ sampleRate: 16000 })
  try {
    const decoded = await context.decodeAudioData(await file.arrayBuffer())
    const samples = decoded.length
    if (!samples || samples * 2 + 44 > MAX_BATCH_BYTES) throw new Error('duration')
    const bytes = new ArrayBuffer(44 + samples * 2)
    const view = new DataView(bytes)
    const text = (offset: number, value: string) => [...value].forEach((char, i) => view.setUint8(offset + i, char.charCodeAt(0)))
    text(0, 'RIFF'); view.setUint32(4, bytes.byteLength - 8, true); text(8, 'WAVEfmt ')
    view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true)
    view.setUint32(24, 16000, true); view.setUint32(28, 32000, true); view.setUint16(32, 2, true); view.setUint16(34, 16, true)
    text(36, 'data'); view.setUint32(40, samples * 2, true)
    const channels = Array.from({ length: decoded.numberOfChannels }, (_, i) => decoded.getChannelData(i))
    for (let i = 0; i < samples; i++) {
      let sample = 0
      for (const channel of channels) sample += channel[i] / channels.length
      sample = Math.max(-1, Math.min(1, sample))
      view.setInt16(44 + i * 2, Math.round(sample * (sample < 0 ? 32768 : 32767)), true)
    }
    return { audio: new Blob([bytes], { type: 'audio/wav' }), seconds: samples / 16000 }
  } finally { await context.close() }
}

export function batchSegments(transcript: BatchTranscript, sessionId: string, language: string): TranscriptInput[] {
  const segments: TranscriptInput[] = []
  const separator = ['cmn', 'ja', 'yue'].includes(language) ? '' : ' '
  for (const word of transcript.results ?? []) {
    const alternative = word.alternatives?.[0]
    if (!alternative?.content) continue
    const last = segments.at(-1)
    if (last && (word.type === 'punctuation' || (last.speaker === (alternative.speaker ?? '') && word.start_time - (last.end_time ?? 0) < 1.5 && last.text.length < 500 && !/[.!?。！？]$/.test(last.text)))) {
      last.text += (word.type === 'punctuation' ? '' : separator) + alternative.content
      last.end_time = Math.max(last.end_time ?? 0, word.end_time)
    } else {
      segments.push({ client_segment_id: `${sessionId.slice(0, 24)}${segments.length.toString(16).padStart(12, '0')}`, speaker: alternative.speaker ?? '', text: alternative.content, start_time: word.start_time, end_time: word.end_time, status: 'confirmed', is_partial: false })
    }
  }
  return segments
}

export async function saveBatchResult(ownerId: string, sessionId: string, name: string, language: string, transcript: BatchTranscript): Promise<void> {
  const assertOwner = () => { if (getStoredUser()?.id !== ownerId) throw new Error('Session expired') }
  assertOwner()
  await createSession({ client_session_id: sessionId, title: name.slice(0, 200), source_language: language })
  const segments = batchSegments(transcript, sessionId, language)
  for (let offset = 0; offset < segments.length; offset += 200) {
    assertOwner()
    await saveTranscriptsBatch(sessionId, segments.slice(offset, offset + 200))
  }
  assertOwner()
  await updateSession(sessionId, { status: 'completed', duration_seconds: Math.ceil(transcript.metadata.duration) })
}

export interface ServerBatchJob { id: string; name: string; language: string; seconds: number; job_id: string; status: string; error?: string }
export function listBatchJobs(): Promise<ServerBatchJob[]> { return authFetch('/api/transcribe/batch/jobs') }

export function retryBatchJob(id: string): Promise<void> { return authFetch('/api/transcribe/batch/jobs/retry', { method: 'POST', body: JSON.stringify({ id }) }) }
