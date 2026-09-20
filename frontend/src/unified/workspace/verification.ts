/**
 * Long-session hot-path verification.
 *
 * Run from frontend/:
 *   esbuild src/unified/workspace/verification.ts --bundle --platform=node \
 *     --format=esm --outfile=/tmp/dreamtrans-long-session-verify.mjs
 *   node /tmp/dreamtrans-long-session-verify.mjs
 */
import {
  TranscriptStore,
  type TranscriptSegment,
  type TranslationSegment,
} from '../../core/transcription'
import { VirtualLayout } from '../feed/virtualLayout'
import { AiTranslateClient, type AiTranslateChunk } from './AiTranslateClient'
import {
  endsSentence,
  joinSegmentTexts,
  normalizeTranscriptText,
  textWeight,
} from '../../core/transcription/scriptText'
import { mergeSessionRecords } from './mergeSessionRecords'
import { TranscriptFeedModel } from './TranscriptFeedModel'

const SEGMENT_COUNT = 30_000

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(`Long-session verification failed: ${message}`)
}

const mergeLocalStore = new TranscriptStore()
const localOnly = mergeLocalStore.appendTranscript({
  id: 'local-only',
  speaker: 'S1',
  text: 'This exists only in the durable local copy.',
  startTime: 0,
  endTime: 1,
}).record
const shared = mergeLocalStore.appendTranscript({
  id: 'shared',
  speaker: 'S1',
  text: 'Local content wins.',
  startTime: 2,
  endTime: 3,
}).record
const localTranslation = mergeLocalStore.appendTranslation({
  segmentId: shared.id,
  speaker: shared.speaker,
  language: 'cmn',
  text: '保留本地译文。',
  startTime: shared.startTime,
  endTime: shared.endTime,
}).record
const mergeCloudStore = new TranscriptStore()
const cloudShared = mergeCloudStore.appendTranscript(shared).record
const cloudOnly = mergeCloudStore.appendTranscript({
  id: 'cloud-only',
  speaker: 'S2',
  text: 'This exists only in the cloud copy.',
  startTime: 4,
  endTime: 5,
}).record
const duplicateCloudIdentity = Object.freeze({
  ...cloudOnly,
  id: 'cloud-only-alias',
  sequence: 2,
})
const duplicateCloudScope = mergeCloudStore.appendTranslation({
  id: 'cloud-scope-duplicate',
  segmentId: cloudShared.id,
  speaker: cloudShared.speaker,
  language: 'cmn',
  text: '云端较晚的同范围译文。',
  startTime: cloudShared.startTime,
  endTime: cloudShared.endTime,
}).record
const orphanedCloudTranslation: TranslationSegment = Object.freeze({
  ...duplicateCloudScope,
  id: 'cloud-orphan',
  sequence: 1,
  segmentId: 'missing-cloud-segment',
  text: '缺少原文链接时也要保留。',
})
const mergedRecords = mergeSessionRecords(
  { segments: [localOnly, shared], translations: [localTranslation] },
  {
    segments: [cloudShared, cloudOnly, duplicateCloudIdentity],
    translations: [duplicateCloudScope, orphanedCloudTranslation],
  },
)
assert(mergedRecords.segments.length === 3, 'cloud load retains local-only records')
assert(mergedRecords.segments[1]?.text === 'Local content wins.', 'local identity wins')
assert(
  mergedRecords.segments.every((segment, index) => segment.sequence === index),
  'merged transcript sequences stay dense',
)
assert(
  mergedRecords.translations.length === 2
    && mergedRecords.translations[0]?.id === localTranslation.id,
  'local translation scope wins while unrelated cloud translations survive',
)
assert(
  mergedRecords.translations[1]?.id === orphanedCloudTranslation.id
    && mergedRecords.translations[1]?.segmentId === null,
  'translation text survives when its former transcript link is missing',
)
assert(
  mergedRecords.translations.every((translation, index) => translation.sequence === index),
  'merged translation sequences stay dense',
)

const LARGE_MERGE_SEGMENT_COUNT = 100_000

function syntheticMergeSegment(
  index: number,
  sequence = index,
): TranscriptSegment {
  const startTime = index * 0.75
  return Object.freeze({
    id: `merge-segment-${index}`,
    sequence,
    speaker: `S${index % 4}`,
    text: `Long history segment ${index}.`,
    status: 'final',
    startTime,
    endTime: startTime + 0.5,
    receivedAt: index,
    source: 'verification',
  })
}

function verifyLargeHistoryMerge(): number {
  const localCount = LARGE_MERGE_SEGMENT_COUNT / 2
  const localSegments: TranscriptSegment[] = []
  const incomingSegments: TranscriptSegment[] = []
  for (let index = 0; index < localCount; index += 1) {
    const segment = syntheticMergeSegment(index)
    localSegments.push(segment)
    incomingSegments.push(segment)
  }
  for (let index = localCount; index < LARGE_MERGE_SEGMENT_COUNT; index += 1) {
    incomingSegments.push(syntheticMergeSegment(index, index - localCount))
  }
  const last = incomingSegments.at(-1)
  assert(Boolean(last), 'large merge fixture contains a final segment')
  incomingSegments.push(Object.freeze({
    ...(last as TranscriptSegment),
    id: 'large-history-identity-alias',
    sequence: incomingSegments.length,
  }))

  const startedAt = performance.now()
  const merged = mergeSessionRecords(
    { segments: localSegments, translations: [] },
    { segments: incomingSegments, translations: [] },
  )
  const elapsedMs = performance.now() - startedAt

  assert(
    merged.segments.length === LARGE_MERGE_SEGMENT_COUNT,
    '100k merge drops ID and content-identical duplicates without losing unique rows',
  )
  assert(
    merged.addedSegments === LARGE_MERGE_SEGMENT_COUNT - localCount,
    '100k merge reports only cloud-only additions',
  )
  assert(
    merged.segments[0] === localSegments[0],
    'already-dense frozen local rows are reused instead of cloned',
  )
  for (let index = 0; index < merged.segments.length; index += 1) {
    assert(
      merged.segments[index]?.sequence === index,
      `100k merge sequence ${index} is not dense`,
    )
  }
  assert(elapsedMs < 10_000, `100k merge took ${elapsedMs.toFixed(0)}ms`)
  return elapsedMs
}

const largeMergeElapsedMs = verifyLargeHistoryMerge()

// --- Script-aware text helpers: CJK weight, spacing, sentence ends ---
assert(textWeight('hello world') === 11, 'Latin text weighs one per character')
assert(textWeight('你好') === 6, 'Han characters weigh three Latin characters each')
assert(textWeight('안녕') === 2, 'Hangul counts like Latin because Korean uses spaces')
assert(
  normalizeTranscriptText('我们 今天 讨论 一下 。') === '我们今天讨论一下。',
  `provider word spacing is removed inside Chinese, got ${JSON.stringify(normalizeTranscriptText('我们 今天 讨论 一下 。'))}`,
)
assert(
  normalizeTranscriptText('今日 は いい 天気 です ね 。') === '今日はいい天気ですね。',
  'provider word spacing is removed inside Japanese',
)
assert(
  normalizeTranscriptText('오늘 날씨 좋네요.') === '오늘 날씨 좋네요.',
  'Korean keeps its word spacing',
)
assert(
  normalizeTranscriptText('  The GPT 模型 很 强 today. ') === 'The GPT 模型很强 today.',
  'mixed-script text keeps Latin spacing and drops CJK-internal spacing',
)
assert(joinSegmentTexts('你好。', '今天') === '你好。今天', 'CJK fragments join without a space')
assert(joinSegmentTexts('Hi.', 'Hello.') === 'Hi. Hello.', 'Latin fragments join with a space')
assert(endsSentence('「終わりました。」') && endsSentence('他说：“走吧。”'), 'closing CJK quotes still end a sentence')
assert(!endsSentence('他说，') && !endsSentence('今天'), 'commas and open clauses do not end a sentence')

// Chinese cards break after a comparable amount of speech as English cards:
// ~40 Han characters weigh ~120 Latin-equivalents, reaching the sentence-break
// threshold, so the next final starts a new card even though the raw count
// is far below 120.
{
  const cjkStore = new TranscriptStore()
  const cjkFeed = new TranscriptFeedModel({
    sourceLanguage: 'cmn',
    targetLanguage: 'en',
    translationEnabled: true,
  })
  const longSentence = '我们今天要讨论的是产品定价策略以及它对下个季度营收增长目标可能带来的影响和风险。'
  assert(longSentence.length >= 40 && longSentence.length < 120, 'fixture is shorter than the raw threshold')
  const first = cjkStore.appendTranscript({ speaker: 'S1', text: longSentence, startTime: 0, endTime: 8 }).record
  const second = cjkStore.appendTranscript({ speaker: 'S1', text: '接下来看第二点。', startTime: 8.5, endTime: 10 }).record
  cjkFeed.appendSegment(first)
  cjkFeed.appendSegment(second)
  assert(
    cjkFeed.getSnapshot().items.length === 2,
    `a long Chinese sentence must close its card, got ${cjkFeed.getSnapshot().items.length} card(s)`,
  )

  const shortStore = new TranscriptStore()
  const shortFeed = new TranscriptFeedModel({
    sourceLanguage: 'cmn',
    targetLanguage: 'en',
    translationEnabled: true,
  })
  shortFeed.appendSegment(shortStore.appendTranscript({ speaker: 'S1', text: '好的。', startTime: 0, endTime: 0.5 }).record)
  shortFeed.appendSegment(shortStore.appendTranscript({ speaker: 'S1', text: '我们开始吧。', startTime: 0.8, endTime: 1.6 }).record)
  assert(shortFeed.getSnapshot().items.length === 1, 'short Chinese sentences still merge into one card')
  assert(
    shortFeed.getSnapshot().items[0]?.original?.text === '好的。我们开始吧。',
    `Chinese card text joins without spaces, got ${JSON.stringify(shortFeed.getSnapshot().items[0]?.original?.text)}`,
  )
}

// --- Display aggregation: fragments merge into readable utterance cards ---
const aggStore = new TranscriptStore()
const agg = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: true,
})

const fragmentTexts = ['Hi.', 'Hello.', 'How', 'are', 'you', 'doing?']
const fragmentSegments = fragmentTexts.map((text, index) => aggStore.appendTranscript({
  speaker: 'S1',
  text,
  startTime: index * 0.6,
  endTime: index * 0.6 + 0.5,
}).record)
for (const segment of fragmentSegments) agg.appendSegment(segment)
assert(
  agg.getSnapshot().items.length === 1,
  'six same-speaker fragments must render as one card',
)
assert(
  agg.getSnapshot().items[0]?.original?.text === 'Hi. Hello. How are you doing?',
  `punctuation-aware join, got ${JSON.stringify(agg.getSnapshot().items[0]?.original?.text)}`,
)
assert(
  agg.getSnapshot().items[0]?.segmentIds?.length === 6,
  'card must keep the mapping to its atomic segments',
)

// Translation of one fragment attaches to the whole card without leaving the
// remaining fragments stuck on a waiting placeholder forever.
const translationTexts = ['一', '二', '三', '四', '五', '六']
const middleTranslation = aggStore.appendTranslation({
  segmentId: fragmentSegments[2].id,
  speaker: 'S1',
  language: 'cmn',
  text: translationTexts[2],
  startTime: fragmentSegments[2].startTime,
  endTime: fragmentSegments[2].endTime,
}).record
agg.appendTranslation(middleTranslation)
{
  const card = agg.getSnapshot().items[0]
  assert(
    card?.translation?.text === '三' && card.translation.status === 'streaming',
    'a partial set of translations must render on the card immediately',
  )
}
const translationRecords = [middleTranslation]
for (const [index, segment] of fragmentSegments.entries()) {
  if (index === 2) continue
  const record = aggStore.appendTranslation({
    segmentId: segment.id,
    speaker: 'S1',
    language: 'cmn',
    text: translationTexts[index],
    startTime: segment.startTime,
    endTime: segment.endTime,
  }).record
  translationRecords.push(record)
  agg.appendTranslation(record)
}
{
  const card = agg.getSnapshot().items[0]
  assert(
    card?.translation?.text === '一二三四五六' && card.translation.status === 'final',
    `card translation joins in part order, got ${JSON.stringify(card?.translation)}`,
  )
}
const temporaryTranslation = aggStore.setTranslationPartial({
  segmentId: fragmentSegments[5].id,
  speaker: 'S1',
  language: 'cmn',
  text: '临时预览',
  startTime: fragmentSegments[5].startTime,
  endTime: fragmentSegments[5].endTime,
})
assert(temporaryTranslation !== null, 'store must produce a translation partial')
agg.setTranslationPartial(temporaryTranslation)
assert(
  agg.getSnapshot().items[0]?.translation?.partialText === '临时预览',
  'translation partial renders on its card',
)
agg.clearTranslationPartial()
assert(
  agg.getSnapshot().items[0]?.translation?.text === '一二三四五六'
    && agg.getSnapshot().items[0]?.translation?.partialText === undefined
    && agg.getSnapshot().items[0]?.translation?.status === 'final',
  'clearing a translation partial preserves completed translation text',
)

// Speaker changes and >2s pauses start a new card.
const speakerChange = aggStore.appendTranscript({
  speaker: 'S2',
  text: 'Different speaker.',
  startTime: 3.7,
  endTime: 4.2,
}).record
agg.appendSegment(speakerChange)
assert(agg.getSnapshot().items.length === 2, 'a speaker change must start a new card')

const afterLongPause = aggStore.appendTranscript({
  speaker: 'S2',
  text: 'After a long pause.',
  startTime: 4.2 + 2.01,
  endTime: 4.2 + 2.51,
}).record
agg.appendSegment(afterLongPause)
assert(agg.getSnapshot().items.length === 3, 'a 2.01s pause must start a new card')

// A continuing live partial streams inside the newest card instead of
// flapping a separate row for every provider micro-final.
const livePartial = aggStore.setPartial({
  speaker: 'S2',
  text: 'and one more',
  startTime: 6.9,
  endTime: 7.4,
})
assert(livePartial !== null, 'store must produce a live partial')
agg.setPartial(livePartial)
{
  const items = agg.getSnapshot().items
  assert(items.length === 3, 'a continuing partial must not add a separate row')
  assert(
    items[2]?.original?.partialText === 'and one more',
    'the continuing partial streams inside the newest card',
  )
}
const mergedFinal = aggStore.appendTranscript({
  speaker: 'S2',
  text: 'and one more thing.',
  startTime: 6.9,
  endTime: 7.5,
}).record
agg.appendSegment(mergedFinal)
assert(
  agg.getSnapshot().items.length === 3,
  'the merged final keeps the same card count',
)
assert(
  agg.getSnapshot().items[2]?.original?.text === 'After a long pause. and one more thing.',
  'merged final must extend the previous card',
)
assert(
  agg.getSnapshot().items[2]?.original?.partialText === undefined,
  'the merged final consumes the in-card streaming preview',
)

// A partial for a NEW speaker still gets its own live row.
const otherSpeakerPartial = aggStore.setPartial({
  speaker: 'S9',
  text: 'different voice',
  startTime: 7.6,
  endTime: 8.1,
})
assert(otherSpeakerPartial !== null, 'store must produce the second partial')
agg.setPartial(otherSpeakerPartial)
assert(
  agg.getSnapshot().items.length === 4,
  'a new-speaker partial must appear as its own live row',
)
agg.clearPartial()
assert(agg.getSnapshot().items.length === 3, 'clearing removes the live row')

// Sentence-punctuated monologues break at sentence boundaries and stay
// within the hard bounds.
const monologue = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: false,
})
const monologueStore = new TranscriptStore()
for (let index = 0; index < 12; index += 1) {
  monologue.appendSegment(monologueStore.appendTranscript({
    speaker: 'S1',
    text: `${'word '.repeat(11).trim()}.`,
    startTime: index * 1.2,
    endTime: index * 1.2 + 1,
  }).record)
}
{
  const items = monologue.getSnapshot().items
  assert(items.length > 1, 'an unbroken monologue must still break into multiple cards')
  for (const item of items) {
    const length = item.original?.text?.length ?? 0
    assert(length <= 420, `card exceeded the hard char bound: ${length}`)
    assert(
      /[.!?。！？…]$/u.test(item.original?.text ?? ''),
      'sentence-punctuated monologue cards break at sentence boundaries',
    )
  }
}

// An open sentence must NOT split just because the preferred length was
// reached ("...My brothers had" | "three, I have one..." style breaks).
const openSentence = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: false,
})
const openSentenceStore = new TranscriptStore()
for (let index = 0; index < 5; index += 1) {
  openSentence.appendSegment(openSentenceStore.appendTranscript({
    speaker: 'S1',
    text: 'and then we kept going without any pause or punctuation at all',
    startTime: index * 1.4,
    endTime: index * 1.4 + 1.2,
  }).record)
}
openSentence.appendSegment(openSentenceStore.appendTranscript({
  speaker: 'S1',
  text: 'until the very end.',
  startTime: 7.0,
  endTime: 7.8,
}).record)
{
  const items = openSentence.getSnapshot().items
  assert(
    items.length === 1,
    `an unfinished sentence past the soft cap stays one card, got ${items.length}`,
  )
  const text = items[0]?.original?.text ?? ''
  assert(text.length > 240, 'the soft cap no longer splits mid-sentence')
  assert(text.endsWith('until the very end.'), 'the sentence completes in the same card')
}

// A mid-sentence thinking pause (2–3.5s) keeps the card together; the same
// pause after a completed sentence starts a new card.
const pauses = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: false,
})
const pausesStore = new TranscriptStore()
pauses.appendSegment(pausesStore.appendTranscript({
  speaker: 'S1',
  text: 'So by',
  startTime: 0,
  endTime: 0.5,
}).record)
pauses.appendSegment(pausesStore.appendTranscript({
  speaker: 'S1',
  text: '2050 most countries will shrink.',
  startTime: 3.4, // 2.9s thinking pause, sentence still open
  endTime: 5.0,
}).record)
assert(
  pauses.getSnapshot().items.length === 1,
  'a mid-sentence thinking pause must not split the card',
)
pauses.appendSegment(pausesStore.appendTranscript({
  speaker: 'S1',
  text: 'Nigeria is one of them.',
  startTime: 7.9, // same 2.9s pause, but the sentence was complete
  endTime: 9.0,
}).record)
assert(
  pauses.getSnapshot().items.length === 2,
  'the same pause after a finished sentence starts a new card',
)

// A delayed final from far earlier in the timeline must not pass the negative
// gap check and corrupt the newest card. Promotion from an old live partial
// also restores chronological card order.
const delayed = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: false,
})
const delayedStore = new TranscriptStore()
delayed.appendSegment(delayedStore.appendTranscript({
  speaker: 'S1',
  text: 'Newest card.',
  startTime: 105,
  endTime: 106,
}).record)
const delayedPartial = delayedStore.setPartial({
  speaker: 'S1',
  text: 'Old delayed text',
  startTime: 12,
  endTime: 12.4,
})
assert(delayedPartial !== null, 'store must produce the delayed partial')
delayed.setPartial(delayedPartial)
delayed.appendSegment(delayedStore.appendTranscript({
  speaker: 'S1',
  text: 'Old delayed text.',
  startTime: 12,
  endTime: 12.5,
}).record)
{
  const items = delayed.getSnapshot().items
  assert(items.length === 2, 'a far-earlier final must remain a separate card')
  assert(
    items[0]?.startTime === 12 && items[1]?.startTime === 105,
    `delayed finals restore chronological order, got ${items.map((item) => item.startTime)}`,
  )
  assert(
    items[1]?.original?.text === 'Newest card.' && items[1]?.endTime === 106,
    'a delayed final must not change the latest card text or end time',
  )
}

// Reloading a session must produce exactly the same cards as the live view.
const rehydrated = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: true,
})
rehydrated.hydrate(
  [...fragmentSegments, speakerChange, afterLongPause, mergedFinal],
  translationRecords,
)
const cardShape = (items: readonly ReturnType<TranscriptFeedModel['getSnapshot']>['items'][number][]) =>
  items.map((item) => ({
    speaker: item.speaker,
    text: item.original?.text ?? null,
    translation: item.translation?.text ?? null,
    segments: item.segmentIds?.length ?? 0,
  }))
assert(
  JSON.stringify(cardShape(rehydrated.getSnapshot().items))
    === JSON.stringify(cardShape(agg.getSnapshot().items)),
  'hydrated history must render the same cards as the live session',
)

const store = new TranscriptStore()
const feed = new TranscriptFeedModel({
  sourceLanguage: 'en',
  targetLanguage: 'cmn',
  translationEnabled: true,
})
const layout = new VirtualLayout([], [], 'bilingual:1024', '0', 164)
const startedAt = performance.now()

for (let index = 0; index < SEGMENT_COUNT; index += 1) {
  const startTime = index * 1.8
  const speaker = `S${index % 4}`
  const partial = store.setPartial({
    speaker,
    text: `Streaming phrase ${index}`,
    startTime,
    endTime: startTime + 0.8,
  })
  if (partial) feed.setPartial(partial)

  const segment = store.appendTranscript({
    speaker,
    text: `Confirmed phrase number ${index}.`,
    startTime,
    endTime: startTime + 1.5,
  }).record
  feed.appendSegment(segment)

  const translation = store.appendTranslation({
    speaker,
    language: 'cmn',
    text: `第 ${index} 个确认片段。`,
    startTime,
    endTime: startTime + 1.5,
  }).record
  feed.appendTranslation(translation)
  layout.append([feed.getSnapshot().items[index]?.id ?? segment.id], [164 + index % 5])
}

const appendElapsedMs = performance.now() - startedAt
const lookupStartedAt = performance.now()
let checksum = 0
for (let index = 0; index < 100_000; index += 1) {
  const row = layout.indexAtOffset((index * 7919) % layout.totalSize)
  checksum += layout.getOffset(row)
}
const lookupElapsedMs = performance.now() - lookupStartedAt

assert(store.getSnapshot().segmentCount === SEGMENT_COUNT, 'all final segments appended')
assert(store.getSnapshot().translationCount === SEGMENT_COUNT, 'all translations appended')
assert(feed.getSnapshot().items.length === SEGMENT_COUNT, 'streaming rows promoted in place')
assert(layout.length === SEGMENT_COUNT, 'virtual layout appended without a rebuild')
assert(Number.isFinite(checksum) && checksum > 0, 'virtual offsets remain valid')

const sizeBeforeTailTruncate = layout.totalSize
const removedTailSize = layout.getSize(layout.length - 1)
layout.truncate(SEGMENT_COUNT - 1)
assert(layout.length === SEGMENT_COUNT - 1, 'virtual layout truncates a transient tail in place')
assert(
  layout.totalSize === sizeBeforeTailTruncate - removedTailSize,
  'tail truncation preserves Fenwick sums',
)
layout.append(['tail-restored'], [removedTailSize])
assert(layout.length === SEGMENT_COUNT, 'virtual layout appends normally after a tail truncation')
assert(layout.totalSize === sizeBeforeTailTruncate, 'tail restoration preserves total size')

assert(appendElapsedMs < 15_000, `append path took ${appendElapsedMs.toFixed(0)}ms`)
assert(lookupElapsedMs < 5_000, `lookup path took ${lookupElapsedMs.toFixed(0)}ms`)

// --- AI context translation client: batching, protocol, and matching ---
class FakeTranslateSocket {
  readyState = 0
  readonly sent: string[] = []
  onopen: ((event: unknown) => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onclose: ((event: { code: number; reason: string }) => void) | null = null
  onerror: ((event: unknown) => void) | null = null

  send(data: string): void {
    if (this.readyState !== 1) throw new Error('Fake translate socket is not open')
    this.sent.push(data)
  }

  close(): void {
    if (this.readyState === 3) return
    this.readyState = 3
    this.onclose?.({ code: 1000, reason: '' })
  }

  open(): void {
    this.readyState = 1
    this.onopen?.({})
  }

  serverClose(code = 1006, reason = 'network lost'): void {
    if (this.readyState === 3) return
    this.readyState = 3
    this.onclose?.({ code, reason })
  }

  messages(): Array<Record<string, unknown>> {
    return this.sent.map((raw) => JSON.parse(raw) as Record<string, unknown>)
  }
}

async function verifyAiTranslateClient(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  let socket: FakeTranslateSocket | null = null
  const sockets: FakeTranslateSocket[] = []
  const received: Array<{ chunk: AiTranslateChunk; text: string }> = []
  const errors: string[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: (token) => ['dreamtrans.v1', `dreamtrans.jwt.${token}`],
    socketFactory: () => {
      socket = new FakeTranslateSocket()
      sockets.push(socket)
      return socket
    },
    onTranslation: (chunk, result) => received.push({ chunk, text: result.text }),
    onError: (message) => errors.push(message),
    idleFlushMs: 20,
    minChunkChars: 4,
    reconnectDelaysMs: [0],
  })

  client.startSession({
    translatePrompt: 'CUSTOM PROMPT',
    sourceLanguage: 'cmn',
    targetLanguage: 'en',
  })
  await sleep(0)
  assert(socket !== null, 'startSession must open a translate socket')
  const activeSocket = socket as FakeTranslateSocket
  activeSocket.open()

  const init = activeSocket.messages()[0] as {
    type?: string
    mode?: string
    config?: {
      translate_prompt?: string
      disable_summarization?: boolean
      source_language?: string
      target_language?: string
    }
  }
  assert(init.type === 'init' && init.mode === 'ai_rolling', 'init announces rolling context mode')
  assert(init.config?.translate_prompt === 'CUSTOM PROMPT', 'custom prompt reaches init')
  assert(
    init.config?.source_language === 'cmn' && init.config?.target_language === 'en',
    'the language pair reaches init so the server can pick a default prompt',
  )
  assert(init.config?.disable_summarization === true, 'summarization stays off by default')
  activeSocket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 8,
      capabilities: {
        request_ids: true,
        atomic_transcripts: true,
        async_flush: true,
      },
    }),
  })

  // Fragments of one card batch into one sentence-complete paragraph.
  client.addSegment({ id: 'f1', speaker: 'S1', text: 'Hi.', startTime: 0, endTime: 0.4 }, 'card-1')
  assert(activeSocket.messages().length === 1, 'a too-short sentence fragment stays buffered')
  client.addSegment({ id: 'f2', speaker: 'S1', text: 'Hello.', startTime: 0.5, endTime: 0.9 }, 'card-1')
  {
    const sentMessages = activeSocket.messages()
    assert(sentMessages.length === 2, 'sentence completion sends one atomic transcript')
    const transcript = sentMessages[1] as {
      type?: string
      payload?: { request_id?: string; transcript?: string }
    }
    assert(
      transcript.type === 'transcript' && Boolean(transcript.payload?.request_id),
      'chunk goes out with a stable request ID',
    )
    assert(
      transcript.payload?.transcript === 'Hi. Hello.',
      `chunk joins fragments, got ${JSON.stringify(transcript.payload?.transcript)}`,
    )
  }

  // A card change flushes the open buffer even without sentence punctuation.
  client.addSegment({ id: 'f3', speaker: 'S1', text: 'Unfinished thought', startTime: 1.2, endTime: 1.8 }, 'card-1')
  client.addSegment({ id: 'f4', speaker: 'S2', text: 'Next speaker', startTime: 2.0, endTime: 2.6 }, 'card-2')
  {
    const sentMessages = activeSocket.messages()
    const transcript = sentMessages[2] as { payload?: { transcript?: string } }
    assert(
      transcript.payload?.transcript === 'Unfinished thought',
      'card change flushes the previous buffer',
    )
  }

  // Idle timeout flushes the trailing buffer.
  await sleep(40)
  {
    const sentMessages = activeSocket.messages()
    const transcript = sentMessages[3] as { payload?: { transcript?: string } }
    assert(
      transcript.payload?.transcript === 'Next speaker',
      'idle timeout flushes the open buffer',
    )
  }

  // minChunkChars is measured in Latin-equivalent weight: a two-character
  // Chinese sentence (weight 6 ≥ 4) flushes at once instead of waiting for
  // four raw characters, and the joined chunk carries no CJK-internal spaces.
  client.addSegment({ id: 'f5', speaker: 'S3', text: '好的', startTime: 3.0, endTime: 3.3 }, 'card-3')
  client.addSegment({ id: 'f6', speaker: 'S3', text: '。', startTime: 3.3, endTime: 3.4 }, 'card-3')
  {
    const sentMessages = activeSocket.messages()
    const transcript = sentMessages[4] as { payload?: { transcript?: string } }
    assert(
      transcript.payload?.transcript === '好的。',
      `short CJK sentence flushes by weight, got ${JSON.stringify(transcript.payload?.transcript)}`,
    )
    // Settle it so the reconnect scenario below sees the original pending set.
    const cjkRequestId = (transcript as { payload?: { request_id?: string } }).payload?.request_id
    activeSocket.onmessage?.({
      data: JSON.stringify({
        message: 'AddTranslation',
        results: [{ request_id: cjkRequestId, content: 'OK.', start_time: 3.0, end_time: 3.4 }],
      }),
    })
    assert(received.length === 1 && received[0]?.text === 'OK.', 'the CJK chunk resolves by request ID')
    received.length = 0
  }

  const sentChunks = activeSocket.messages().slice(1, 4) as Array<{
    payload?: { request_id?: string }
  }>
  const firstRequestId = sentChunks[0]?.payload?.request_id
  const secondRequestId = sentChunks[1]?.payload?.request_id
  const thirdRequestId = sentChunks[2]?.payload?.request_id
  assert(
    Boolean(firstRequestId && secondRequestId && thirdRequestId),
    'all atomic chunks carry request IDs',
  )

  // Results are matched by ID even if delivery is out of order.
  activeSocket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{
        request_id: thirdRequestId,
        content: '下一位。',
        start_time: 2.0,
        end_time: 2.6,
      }],
    }),
  })
  assert(received.length === 1, 'AddTranslation resolves one chunk')
  assert(
    JSON.stringify(received[0]?.chunk.segmentIds) === JSON.stringify(['f4']),
    'out-of-order translation maps to its exact atomic segment',
  )

  // A request-scoped error removes only that chunk, never an older neighbor.
  activeSocket.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      request_id: secondRequestId,
      reason: 'upstream busy',
    }),
  })
  assert(errors.length === 1, 'server errors surface to the caller')
  activeSocket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{
        request_id: firstRequestId,
        content: '你好。',
        start_time: 0,
        end_time: 0.9,
      }],
    }),
  })
  assert(
    JSON.stringify(received[1]?.chunk.segmentIds) === JSON.stringify(['f1', 'f2']),
    'a later error cannot discard an earlier pending chunk',
  )

  // A lost response is retried with the same stable ID after reconnect.
  client.addSegment({
    id: 'f5',
    speaker: 'S1',
    text: 'Reconnect me.',
    startTime: 3,
    endTime: 3.8,
  }, 'card-3')
  const reconnectRequest = activeSocket.messages().at(-1) as {
    payload?: { request_id?: string }
  }
  const reconnectRequestId = reconnectRequest.payload?.request_id
  assert(Boolean(reconnectRequestId), 'reconnect candidate has a request ID')
  activeSocket.serverClose()
  await sleep(5)
  assert(sockets.length === 2 && socket !== null, 'network close schedules a reconnect')
  const reconnectedSocket = socket as FakeTranslateSocket
  reconnectedSocket.open()
  reconnectedSocket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 8,
      capabilities: { request_ids: true, atomic_transcripts: true, async_flush: true },
    }),
  })
  const resent = reconnectedSocket.messages()[1] as {
    payload?: { request_id?: string }
  }
  assert(
    resent.payload?.request_id === reconnectRequestId,
    'reconnect resends the same idempotency ID',
  )
  reconnectedSocket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: reconnectRequestId, content: '已重连。' }],
    }),
  })
  assert(Number(received.length) === 3, 'retried request resolves exactly once')

  const drained = await client.stopSession()
  assert(drained, 'stop drains all submitted translations')
  assert(reconnectedSocket.readyState === 3, 'drained stop closes the socket')
  client.destroy()
}

async function verifyAiTranslateBackpressure(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0))
  const socket = new FakeTranslateSocket()
  const translatedIds: string[] = []
  const errors: string[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => socket,
    onTranslation: (chunk) => translatedIds.push(chunk.segmentIds[0] ?? ''),
    onError: (message) => errors.push(message),
    minChunkChars: 1,
    maxPendingChunks: 2,
    maxInFlightChunks: 2,
  })
  client.startSession({})
  await new Promise((resolve) => setTimeout(resolve, 0))
  socket.open()
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 2,
      capabilities: { request_ids: true, atomic_transcripts: true },
    }),
  })
  client.addSegment({ id: 'bp-1', speaker: 'S1', text: 'One.', startTime: 0, endTime: 1 }, 'bp-1')
  client.addSegment({ id: 'bp-2', speaker: 'S1', text: 'Two.', startTime: 1, endTime: 2 }, 'bp-2')
  client.addSegment({ id: 'bp-3', speaker: 'S1', text: 'Three.', startTime: 2, endTime: 3 }, 'bp-3')
  assert(errors.length === 1, 'backpressure rejects the newest chunk explicitly')
  const requests = socket.messages().slice(1) as Array<{
    payload?: { request_id?: string }
  }>
  assert(requests.length === 2, 'backpressure never evicts an already submitted chunk')
  for (const request of [...requests].reverse()) {
    socket.onmessage?.({
      data: JSON.stringify({
        message: 'AddTranslation',
        results: [{ request_id: request.payload?.request_id, content: 'ok' }],
      }),
    })
  }
  assert(
    translatedIds.includes('bp-1') && translatedIds.includes('bp-2'),
    'both accepted chunks remain correctly addressable after backpressure',
  )
  assert(await client.stopSession(), 'backpressure verification drains accepted work')
  client.destroy()
}

async function verifyAiTranslateWorkerNegotiation(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0))
  const socket = new FakeTranslateSocket()
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => socket,
    onTranslation: () => undefined,
    minChunkChars: 1,
    maxPendingChunks: 8,
    maxInFlightChunks: 8,
  })
  client.startSession({})
  await new Promise((resolve) => setTimeout(resolve, 0))
  socket.open()
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 2,
      capabilities: { request_ids: true, atomic_transcripts: true },
    }),
  })
  for (let index = 0; index < 4; index += 1) {
    client.addSegment({
      id: `worker-${index}`,
      speaker: 'S1',
      text: `Sentence ${index}.`,
      startTime: index,
      endTime: index + 0.5,
    }, `worker-card-${index}`)
  }
  let transcripts = socket.messages().filter((message) => message.type === 'transcript')
  assert(transcripts.length === 2, 'client in-flight count never exceeds negotiated workers')
  const firstRequest = (transcripts[0]?.payload as { request_id?: string } | undefined)?.request_id
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: firstRequest, content: 'ok' }],
    }),
  })
  transcripts = socket.messages().filter((message) => message.type === 'transcript')
  assert(transcripts.length === 3, 'one completed worker slot releases exactly one queued chunk')
  for (const message of transcripts.slice(1)) {
    const requestId = (message.payload as { request_id?: string } | undefined)?.request_id
    socket.onmessage?.({
      data: JSON.stringify({
        message: 'AddTranslation',
        results: [{ request_id: requestId, content: 'ok' }],
      }),
    })
  }
  const finalTranscript = socket.messages().filter(
    (message) => message.type === 'transcript',
  ).at(-1)
  const finalRequestId = (
    finalTranscript?.payload as { request_id?: string } | undefined
  )?.request_id
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: finalRequestId, content: 'ok' }],
    }),
  })
  assert(await client.stopSession(), 'worker-negotiated queue drains normally')
  client.destroy()
}

async function verifyAiTranslateHandshakeAndLegacySafety(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  const sockets: FakeTranslateSocket[] = []
  const chunkErrors: string[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => {
      const socket = new FakeTranslateSocket()
      sockets.push(socket)
      return socket
    },
    onTranslation: () => undefined,
    onChunkError: (_chunk, message) => chunkErrors.push(message),
    minChunkChars: 1,
    reconnectDelaysMs: [0],
    handshakeTimeoutMs: 10,
    connectTimeoutMs: 100,
  })
  client.startSession({})
  await sleep(0)
  const silentSocket = sockets[0]
  assert(Boolean(silentSocket), 'silent-handshake socket was created')
  silentSocket?.open()
  client.addSegment({
    id: 'legacy-risk',
    speaker: 'S1',
    text: 'Do not send without an acknowledgement.',
    startTime: 0,
    endTime: 1,
  }, 'legacy-risk')
  await sleep(30)
  assert(
    silentSocket?.messages().length === 1
      && silentSocket.messages()[0]?.type === 'init',
    'handshake timeout reconnects without silently sending a no-ID transcript',
  )
  assert(sockets.length >= 2, 'handshake timeout schedules a fresh connection')

  const legacySocket = sockets.at(-1)
  legacySocket?.open()
  legacySocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
    }),
  })
  assert(
    legacySocket?.messages().some((message) => message.type === 'transcript'),
    'legacy mode is entered only after an explicit capability-free Info',
  )
  legacySocket?.serverClose()
  await sleep(10)
  assert(chunkErrors.length === 1, 'legacy disconnect reports its unidentifiable in-flight chunk')
  const postDisconnectSocket = sockets.at(-1)
  assert(postDisconnectSocket !== legacySocket, 'legacy disconnect reconnects for future work')
  postDisconnectSocket?.open()
  postDisconnectSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
    }),
  })
  assert(
    !postDisconnectSocket?.messages().some((message) => message.type === 'transcript'),
    'legacy disconnect never resends the in-flight transcript',
  )
  assert(await client.stopSession(), 'legacy safety verification stops cleanly')
  client.destroy()
}

async function verifyAiTranslateConnectionTimeouts(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  let resolveToken: ((token: string) => void) | null = null
  let socketCreations = 0
  const lateTokenClient = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: () => new Promise<string>((resolve) => {
      resolveToken = resolve
    }),
    protocolFactory: () => [],
    socketFactory: () => {
      socketCreations += 1
      return new FakeTranslateSocket()
    },
    onTranslation: () => undefined,
    tokenTimeoutMs: 100,
  })
  lateTokenClient.startSession({})
  assert(await lateTokenClient.stopSession(), 'empty session stops while token acquisition is pending')
  if (resolveToken) (resolveToken as (token: string) => void)('late-token')
  await sleep(0)
  assert(socketCreations === 0, 'a late token cannot create an idle socket after stop')
  lateTokenClient.destroy()

  const sockets: FakeTranslateSocket[] = []
  const connectionErrors: string[] = []
  const connectTimeoutClient = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => {
      const socket = new FakeTranslateSocket()
      sockets.push(socket)
      return socket
    },
    onTranslation: () => undefined,
    onError: (message) => connectionErrors.push(message),
    connectTimeoutMs: 10,
    reconnectDelaysMs: [0],
  })
  connectTimeoutClient.startSession({})
  await sleep(30)
  assert(sockets.length >= 2, 'WebSocket open timeout triggers reconnect')
  assert(
    connectionErrors.some((message) => message.includes('连接超时')),
    'WebSocket open timeout is observable',
  )
  connectTimeoutClient.destroy()
}

async function verifyAiTranslateProcessingRetry(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  const socket = new FakeTranslateSocket()
  const translations: string[] = []
  const chunkErrors: string[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => socket,
    onTranslation: (_chunk, result) => translations.push(result.text),
    onChunkError: (_chunk, message) => chunkErrors.push(message),
    minChunkChars: 1,
  })
  client.startSession({})
  await sleep(0)
  socket.open()
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 1,
      capabilities: { request_ids: true, atomic_transcripts: true },
    }),
  })
  client.addSegment({
    id: 'processing-retry',
    speaker: 'S1',
    text: 'Retry the same durable request.',
    startTime: 0,
    endTime: 1,
  }, 'processing-retry')
  const firstTranscript = socket.messages().find((message) => message.type === 'transcript')
  const requestId = (
    firstTranscript?.payload as { request_id?: string } | undefined
  )?.request_id
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'translation_processing',
      request_id: requestId,
      retry_after_ms: 1,
      reason: 'another owner is still processing',
    }),
  })
  await sleep(275)
  const attempts = socket.messages().filter((message) => message.type === 'transcript')
  assert(attempts.length === 2, 'processing response retains and retries the request')
  assert(
    (attempts[1]?.payload as { request_id?: string } | undefined)?.request_id === requestId,
    'processing retry preserves the exact request ID',
  )
  assert(chunkErrors.length === 0, 'processing response is not surfaced as a terminal chunk error')
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: requestId, content: 'retried' }],
    }),
  })
  assert(translations.length === 1, 'processing retry resolves exactly once')
  assert(await client.stopSession(), 'processing retry drains normally')
  client.destroy()

  const boundedSocket = new FakeTranslateSocket()
  const boundedErrors: string[] = []
  const boundedClient = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => boundedSocket,
    onTranslation: () => undefined,
    onChunkError: (_chunk, message) => boundedErrors.push(message),
    minChunkChars: 1,
    processingRetryLimit: 1,
  })
  boundedClient.startSession({})
  await sleep(0)
  boundedSocket.open()
  boundedSocket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 1,
      capabilities: { request_ids: true, atomic_transcripts: true },
    }),
  })
  boundedClient.addSegment({
    id: 'bounded-processing',
    speaker: 'S1',
    text: 'Do not retry forever.',
    startTime: 1,
    endTime: 2,
  }, 'bounded-processing')
  const boundedRequest = boundedSocket.messages().find(
    (message) => message.type === 'transcript',
  )
  const boundedRequestId = (
    boundedRequest?.payload as { request_id?: string } | undefined
  )?.request_id
  const processingError = JSON.stringify({
    message: 'Error',
    type: 'translation_processing',
    request_id: boundedRequestId,
    retry_after_ms: 1,
    reason: 'provider temporarily unavailable',
  })
  boundedSocket.onmessage?.({ data: processingError })
  await sleep(275)
  boundedSocket.onmessage?.({ data: processingError })
  assert(
    boundedErrors.length === 1
      && boundedClient.getDiagnostics().pendingChunks === 0,
    'processing retry budget terminates instead of creating an infinite retry loop',
  )
  assert(await boundedClient.stopSession(), 'bounded processing retry stops cleanly')
  boundedClient.destroy()
}

async function verifyAiTranslateTypedRequestRetry(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  const socket = new FakeTranslateSocket()
  const translations: string[] = []
  const errors: string[] = []
  const chunkErrors: string[] = []
  let recoveries = 0
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => socket,
    onTranslation: (_chunk, result) => translations.push(result.text),
    onError: (message) => errors.push(message),
    onRecovered: () => { recoveries += 1 },
    onChunkError: (_chunk, message) => chunkErrors.push(message),
    minChunkChars: 1,
  })
  client.startSession({})
  await sleep(0)
  socket.open()
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Info',
      reason: 'translator initialized',
      workers: 1,
      capabilities: { request_ids: true, atomic_transcripts: true },
    }),
  })
  client.addSegment({
    id: 'typed-request-retry',
    speaker: 'S1',
    text: 'Retry this temporary quota failure.',
    startTime: 0,
    endTime: 1,
  }, 'typed-request-retry')
  const firstAttempt = socket.messages().find((message) => message.type === 'transcript')
  const requestId = (
    firstAttempt?.payload as { request_id?: string } | undefined
  )?.request_id
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'quota_temporarily_unavailable',
      reason: 'quota store timed out',
      request_id: requestId,
      retryable: true,
      retry_after_ms: 1,
      connection_terminal: false,
    }),
  })
  assert(
    client.getDiagnostics().pendingChunks === 1 && chunkErrors.length === 0,
    'retryable typed request errors retain the original pending chunk',
  )
  assert(
    errors.length === 1 && errors[0]?.includes('quota store timed out'),
    'the first typed retry reason is surfaced once',
  )
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      reason: 'generic follow-up must not replace the typed cause',
    }),
  })
  assert(errors.length === 1, 'a later generic error cannot overwrite the first typed reason')
  await sleep(275)
  const attempts = socket.messages().filter((message) => message.type === 'transcript')
  assert(attempts.length === 2, 'retryable typed request errors are retried')
  assert(
    (attempts[1]?.payload as { request_id?: string } | undefined)?.request_id === requestId,
    'typed request retry preserves the exact request ID',
  )
  socket.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: requestId, content: 'typed retry recovered' }],
    }),
  })
  assert(translations.length === 1, 'typed request retry resolves exactly once')
  assert(recoveries === 1, 'a successful typed retry clears the visible transient error')
  assert(await client.stopSession(), 'recovered typed request drains normally')
  client.destroy()
}

async function verifyAiTranslateTerminalReconnectAndBackoff(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  const sockets: FakeTranslateSocket[] = []
  const errors: string[] = []
  const translations: string[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => {
      const socket = new FakeTranslateSocket()
      sockets.push(socket)
      return socket
    },
    onTranslation: (_chunk, result) => translations.push(result.text),
    onError: (message) => errors.push(message),
    minChunkChars: 1,
    reconnectDelaysMs: [0, 80],
  })
  const initialize = (socket: FakeTranslateSocket) => {
    socket.open()
    socket.onmessage?.({
      data: JSON.stringify({
        message: 'Info',
        reason: 'translator initialized',
        workers: 1,
        capabilities: { request_ids: true, atomic_transcripts: true },
      }),
    })
  }

  client.startSession({})
  await sleep(0)
  const firstSocket = sockets[0]
  assert(Boolean(firstSocket), 'terminal reconnect verification opens its first socket')
  initialize(firstSocket!)
  client.addSegment({
    id: 'terminal-reconnect',
    speaker: 'S1',
    text: 'Replay this request on a fresh connection.',
    startTime: 0,
    endTime: 1,
  }, 'terminal-reconnect')
  const firstRequest = firstSocket?.messages().find((message) => message.type === 'transcript')
  const requestId = (
    firstRequest?.payload as { request_id?: string } | undefined
  )?.request_id
  firstSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'billing_temporarily_unavailable',
      reason: 'billing connection must be replaced',
      request_id: requestId,
      retryable: true,
      retry_after_ms: 0,
      connection_terminal: true,
    }),
  })
  await sleep(10)
  assert(sockets.length === 2, 'connection-terminal retry abandons the current socket')
  const secondSocket = sockets[1]
  initialize(secondSocket!)
  secondSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'billing_temporarily_unavailable',
      reason: 'billing is still recovering',
      retryable: true,
      retry_after_ms: 0,
      connection_terminal: true,
    }),
  })
  await sleep(30)
  assert(
    sockets.length === 2,
    'a successful handshake alone does not reset the reconnect backoff',
  )
  await sleep(90)
  assert(
    Number(sockets.length) === 3,
    'the next backoff attempt eventually opens a new socket',
  )
  const thirdSocket = sockets[2]
  initialize(thirdSocket!)
  await sleep(280)
  const replay = thirdSocket?.messages().find((message) => message.type === 'transcript')
  assert(
    (replay?.payload as { request_id?: string } | undefined)?.request_id === requestId,
    'connection-terminal retry replays the same durable request ID',
  )
  thirdSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: requestId, content: 'connection recovered' }],
    }),
  })
  assert(translations.length === 1, 'connection-terminal replay resolves exactly once')
  thirdSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'billing_temporarily_unavailable',
      reason: 'post-success reconnect',
      retryable: true,
      retry_after_ms: 0,
      connection_terminal: true,
    }),
  })
  await sleep(10)
  assert(
    Number(sockets.length) === 4,
    'a successful translation resets reconnect backoff for the next incident',
  )
  assert(
    Number(errors.length) === 2
      && errors[0]?.includes('billing connection must be replaced')
      && errors[1]?.includes('post-success reconnect'),
    'typed connection errors remain sticky until the affected request recovers',
  )
  client.destroy()
}

async function verifyAiTranslateLegacyPoisonedConnections(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  const sockets: FakeTranslateSocket[] = []
  const errors: string[] = []
  const translations: string[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => {
      const socket = new FakeTranslateSocket()
      sockets.push(socket)
      return socket
    },
    onTranslation: (_chunk, result) => translations.push(result.text),
    onError: (message) => errors.push(message),
    minChunkChars: 1,
    reconnectDelaysMs: [0],
  })
  const initialize = (socket: FakeTranslateSocket) => {
    socket.open()
    socket.onmessage?.({
      data: JSON.stringify({
        message: 'Info',
        reason: 'translator initialized',
        workers: 1,
        capabilities: { request_ids: true, atomic_transcripts: true },
      }),
    })
  }

  client.startSession({})
  await sleep(0)
  const firstSocket = sockets[0]
  initialize(firstSocket!)
  client.addSegment({
    id: 'legacy-billing-error',
    speaker: 'S1',
    text: 'Retry after a legacy billing error.',
    startTime: 0,
    endTime: 1,
  }, 'legacy-billing-error')
  const firstRequest = firstSocket?.messages().find((message) => message.type === 'transcript')
  const firstRequestId = (
    firstRequest?.payload as { request_id?: string } | undefined
  )?.request_id
  const staleMessageHandler = firstSocket?.onmessage
  firstSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'billing_error',
      reason: 'paid features disabled because usage could not be charged',
    }),
  })
  staleMessageHandler?.({
    data: JSON.stringify({
      message: 'Error',
      request_id: firstRequestId,
      reason: 'paid features are disabled for this connection',
    }),
  })
  await sleep(10)
  assert(sockets.length === 2, 'legacy billing_error abandons its poisoned socket')
  assert(
    errors.length === 1 && errors[0]?.includes('计费连接已停用'),
    'legacy billing_error surfaces one clear sticky reason without an error storm',
  )
  const secondSocket = sockets[1]
  initialize(secondSocket!)
  await sleep(275)
  const firstReplay = secondSocket?.messages().find((message) => message.type === 'transcript')
  assert(
    (firstReplay?.payload as { request_id?: string } | undefined)?.request_id === firstRequestId,
    'legacy billing_error reconnect replays the same request ID',
  )
  secondSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: firstRequestId, content: 'legacy billing recovered' }],
    }),
  })

  client.addSegment({
    id: 'legacy-generic-disabled',
    speaker: 'S1',
    text: 'Retry the exact legacy disabled reason.',
    startTime: 2,
    endTime: 3,
  }, 'legacy-generic-disabled')
  const secondRequest = secondSocket?.messages()
    .filter((message) => message.type === 'transcript')
    .at(-1)
  const secondRequestId = (
    secondRequest?.payload as { request_id?: string } | undefined
  )?.request_id
  secondSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      request_id: secondRequestId,
      reason: 'paid features are disabled for this connection',
    }),
  })
  await sleep(10)
  assert(
    Number(sockets.length) === 3 && Number(errors.length) === 2,
    'the exact legacy disabled reason also replaces the poisoned socket once',
  )
  const thirdSocket = sockets[2]
  initialize(thirdSocket!)
  await sleep(275)
  const secondReplay = thirdSocket?.messages().find((message) => message.type === 'transcript')
  assert(
    (secondReplay?.payload as { request_id?: string } | undefined)?.request_id === secondRequestId,
    'the exact legacy disabled reason preserves its request ID across reconnect',
  )
  thirdSocket?.onmessage?.({
    data: JSON.stringify({
      message: 'AddTranslation',
      results: [{ request_id: secondRequestId, content: 'legacy generic recovered' }],
    }),
  })
  assert(translations.length === 2, 'both legacy poisoned-connection requests resolve once')
  client.destroy()

  const quotaSockets: FakeTranslateSocket[] = []
  const quotaErrors: string[] = []
  const quotaClient = new AiTranslateClient({
    url: 'ws://verify/ws/translate',
    tokenProvider: async () => 'verify-token',
    protocolFactory: () => [],
    socketFactory: () => {
      const socket = new FakeTranslateSocket()
      quotaSockets.push(socket)
      return socket
    },
    onTranslation: () => undefined,
    onError: (message) => quotaErrors.push(message),
    reconnectDelaysMs: [0],
  })
  quotaClient.startSession({})
  await sleep(0)
  initialize(quotaSockets[0]!)
  quotaSockets[0]?.onmessage?.({
    data: JSON.stringify({
      message: 'Error',
      type: 'quota_error',
      reason: 'provider-backed features disabled because quota is unavailable',
    }),
  })
  await sleep(10)
  assert(quotaSockets.length === 2, 'legacy quota_error replaces its poisoned socket')
  assert(
    quotaErrors.length === 1 && quotaErrors[0]?.includes('Provider API 配额暂时不可用'),
    'legacy quota_error receives a clear, non-storming compatibility message',
  )
  quotaClient.destroy()
}

async function verifyAiTranslateTerminalAccountBlocks(): Promise<void> {
  const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))
  for (const errorType of ['quota_exhausted', 'insufficient_balance', 'pricing_unavailable']) {
    const sockets: FakeTranslateSocket[] = []
    const errors: string[] = []
    const chunkErrors: string[] = []
    const client = new AiTranslateClient({
      url: 'ws://verify/ws/translate',
      tokenProvider: async () => 'verify-token',
      protocolFactory: () => [],
      socketFactory: () => {
        const socket = new FakeTranslateSocket()
        sockets.push(socket)
        return socket
      },
      onTranslation: () => undefined,
      onError: (message) => errors.push(message),
      onChunkError: (_chunk, message) => chunkErrors.push(message),
      minChunkChars: 1,
      reconnectDelaysMs: [0],
    })
    client.startSession({})
    await sleep(0)
    const socket = sockets[0]
    assert(Boolean(socket), `${errorType} verification opens a socket`)
    socket?.open()
    socket?.onmessage?.({
      data: JSON.stringify({
        message: 'Info',
        reason: 'translator initialized',
        workers: 1,
        capabilities: { request_ids: true, atomic_transcripts: true },
      }),
    })
    client.addSegment({
      id: `terminal-${errorType}`,
      speaker: 'S1',
      text: 'Preserve this original text.',
      startTime: 0,
      endTime: 1,
    }, `terminal-${errorType}`)
    const request = socket?.messages().find((message) => message.type === 'transcript')
    const requestId = (
      request?.payload as { request_id?: string } | undefined
    )?.request_id
    const lateMessageHandler = socket?.onmessage
    if (errorType === 'insufficient_balance') {
      socket?.onmessage?.({
        data: JSON.stringify({
          message: 'Error',
          type: 'billing_temporarily_unavailable',
          reason: 'temporary billing failure',
          request_id: requestId,
          retryable: true,
          connection_terminal: false,
          retry_after_ms: 5_000,
        }),
      })
      assert(
        errors.length === 1 && errors[0]?.includes('temporary billing failure'),
        'a retryable typed reason is sticky before a terminal account failure',
      )
    }
    socket?.onmessage?.({
      data: JSON.stringify({
        message: 'Error',
        type: errorType,
        reason: `${errorType} terminal reason`,
        request_id: requestId,
        retryable: false,
        connection_terminal: errorType === 'insufficient_balance',
      }),
    })
    lateMessageHandler?.({
      data: JSON.stringify({
        message: 'Error',
        request_id: requestId,
        reason: 'generic late failure',
      }),
    })
    const expectedErrorCount = errorType === 'insufficient_balance' ? 2 : 1
    assert(
      errors.length === expectedErrorCount
        && errors.at(-1)?.includes(`${errorType} terminal reason`),
      `${errorType} reports its typed terminal cause exactly once`,
    )
    if (errorType === 'insufficient_balance') {
      assert(
        !errors.at(-1)?.includes('temporary billing failure'),
        'a terminal account error replaces an earlier retryable sticky reason',
      )
    }
    assert(
      chunkErrors.length === 1 && client.getDiagnostics().pendingChunks === 0,
      `${errorType} preserves original text and clears unsafe pending work`,
    )
    const sentBeforeBlockedInput = socket?.messages().length ?? 0
    client.addSegment({
      id: `blocked-${errorType}`,
      speaker: 'S1',
      text: 'Do not submit while blocked.',
      startTime: 2,
      endTime: 3,
    }, `blocked-${errorType}`)
    await sleep(10)
    assert(
      sockets.length === 1 && socket?.messages().length === sentBeforeBlockedInput,
      `${errorType} stops submission and reconnect for the current session`,
    )
    assert(!(await client.stopSession()), `${errorType} reports an incomplete translation drain`)
    client.startSession({})
    await sleep(0)
    assert(
      Number(sockets.length) === 2,
      `${errorType} block is cleared by a new session`,
    )
    client.destroy()
  }
}

await verifyAiTranslateClient()
await verifyAiTranslateBackpressure()
await verifyAiTranslateWorkerNegotiation()
await verifyAiTranslateHandshakeAndLegacySafety()
await verifyAiTranslateConnectionTimeouts()
await verifyAiTranslateProcessingRetry()
await verifyAiTranslateTypedRequestRetry()
await verifyAiTranslateTerminalReconnectAndBackoff()
await verifyAiTranslateLegacyPoisonedConnections()
await verifyAiTranslateTerminalAccountBlocks()

async function verifyTranslationDeploymentHandoff(): Promise<void> {
  const sockets: FakeTranslateSocket[] = []
  const client = new AiTranslateClient({
    url: 'ws://verify/ws/translate', tokenProvider: async () => 'token', protocolFactory: () => [],
    socketFactory: () => { const socket = new FakeTranslateSocket(); sockets.push(socket); return socket },
    onTranslation: () => {}, minChunkChars: 1, reconnectDelaysMs: [0],
  })
  const tick = () => new Promise(resolve => setTimeout(resolve, 0))
  client.startSession({ sessionId: 'same-session' })
  await tick()
  const old = sockets[0]
  old.open()
  const message = (socket: FakeTranslateSocket, value: unknown) => socket.onmessage?.({ data: JSON.stringify(value) })
  message(old, { message: 'Info', reason: 'translator initialized', capabilities: { request_ids: true, atomic_transcripts: true } })
  client.addSegment({ id: 'old', speaker: 'S1', text: 'First sentence.', startTime: 0, endTime: 1 }, 'old')
  const request = old.messages().find(v => v.type === 'transcript')?.payload as { request_id: string }
  message(old, { message: 'DeploymentHandoff', version: 1 })
  client.addSegment({ id: 'new', speaker: 'S1', text: 'Second sentence.', startTime: 1, endTime: 2 }, 'new')
  assert(old.readyState === 1 && old.messages().filter(v => v.type === 'transcript').length === 1, 'translation handoff must wait for inflight results and queue new work')
  message(old, { message: 'AddTranslation', results: [{ request_id: request.request_id, content: 'First result.' }] })
  for (let i = 0; i < 20 && sockets.length < 2; i++) await tick()
  assert(sockets.length === 2, 'translation connection should move automatically')
  const fresh = sockets[1]
  fresh.open()
  message(fresh, { message: 'Info', reason: 'translator initialized', capabilities: { request_ids: true, atomic_transcripts: true } })
  assert(fresh.messages().filter(v => v.type === 'transcript').length === 1, 'only queued work should be submitted on replacement')
  client.destroy()
}
await verifyTranslationDeploymentHandoff()

console.log(JSON.stringify({
  segments: SEGMENT_COUNT,
  appendElapsedMs: Math.round(appendElapsedMs),
  largeMergeElapsedMs: Math.round(largeMergeElapsedMs),
  largeMergeSegments: LARGE_MERGE_SEGMENT_COUNT,
  lookupElapsedMs: Math.round(lookupElapsedMs),
  aiTranslateClient: 'batching, typed failures, backoff, weak-network retry, and matching verified',
  mountedRowsAtTypicalViewport: 'visible rows + overscan only',
}))
