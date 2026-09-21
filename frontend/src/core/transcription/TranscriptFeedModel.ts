import type {
  TranscriptPartial,
  TranscriptSegment,
  TranslationPartial,
  TranslationSegment,
} from './types'
import type { TranscriptFeedItem, TranscriptFeedTrack } from './feedTypes'
import {
  endsSentence,
  joinSegmentTexts,
  textWeight,
} from './scriptText'

export { joinSegmentTexts }

export interface TranscriptFeedModelSnapshot {
  readonly version: number
  readonly generation: number
  /**
   * Atomic transcript segments covered by a translation. Chunked AI
   * translations cover several segments per record, so this counter — not
   * the translation record count — is the source for progress displays.
   */
  readonly translatedSegmentCount: number
  /**
   * Intentionally stable, append-only backing storage. Rows are mutated only
   * immediately before a new snapshot is published, avoiding a full-array
   * clone for every streaming partial in a multi-hour session.
   */
  readonly items: readonly TranscriptFeedItem[]
}

export interface TranscriptFeedModelOptions {
  sourceLanguage: string
  targetLanguage: string
  translationEnabled: boolean
}

const DEFAULT_SPEAKER = 'Speaker'
const PARTIAL_MATCH_TOLERANCE_SECONDS = 2

/**
 * Display aggregation. Provider finals are often tiny fragments ("Hi." /
 * "Hello." / "How"), which is unreadable during a lecture or meeting. The
 * store keeps every atomic segment; only this view model merges consecutive
 * same-speaker fragments into one card, bounded so cards stay scannable.
 *
 * Cards prefer to break at sentence boundaries: while a sentence is still
 * open, merging continues past the preferred length up to the hard bounds,
 * and a longer mid-sentence thinking pause is tolerated. Only the hard
 * bounds may split mid-sentence (runaway-monologue protection).
 */
const MERGE_GAP_SECONDS = 2
const MIDSENTENCE_MERGE_GAP_SECONDS = 3.5
/**
 * Once a card is this long AND ends a sentence, the next final starts a new
 * card. Lengths are measured with `textWeight` (Latin-equivalent characters),
 * so a Chinese or Japanese card breaks after a comparable amount of speech
 * rather than after three times as much.
 */
const SENTENCE_BREAK_MIN_WEIGHT = 120
const HARD_MAX_CARD_WEIGHT = 420
const HARD_MAX_CARD_SECONDS = 32
const HARD_MAX_CARD_PARTS = 48

interface CardPart {
  segmentId: string
  text: string
  startTime: number
  endTime: number
  translation?: string
}

interface CardState {
  parts: CardPart[]
}

function rangesTouch(
  left: { startTime?: number; endTime?: number; speaker: string },
  right: { startTime: number; endTime: number; speaker: string },
  allowSpeakerRevision = false,
): boolean {
  if (
    left.startTime === undefined
    || left.endTime === undefined
    || (!allowSpeakerRevision && left.speaker !== right.speaker)
  ) {
    return false
  }
  return (
    right.startTime <= left.endTime + PARTIAL_MATCH_TOLERANCE_SECONDS
    && right.endTime + PARTIAL_MATCH_TOLERANCE_SECONDS >= left.startTime
  )
}

function speakerLabel(speaker: string | undefined): string {
  return speaker?.trim() || DEFAULT_SPEAKER
}

/**
 * Incremental view model between the normalized transcript store and React.
 *
 * The final row array is never rebuilt during recording. A streaming row is
 * updated in place and promoted to a final row without changing its UI id,
 * which keeps virtual-list layout work independent of transcript length.
 * Live aggregation and history hydration share the same append path, so a
 * reloaded session renders exactly the same cards as the live view did.
 */
export class TranscriptFeedModel {
  private readonly listeners = new Set<() => void>()
  private readonly segmentIndex = new Map<string, number>()
  private readonly cards = new Map<string, CardState>()
  private readonly itemsValue: TranscriptFeedItem[] = []
  private options: TranscriptFeedModelOptions
  private activePartialIndex: number | null = null
  private activeTranslationPartialCardIndex: number | null = null
  private translatedSegmentCount = 0
  /**
   * When the live partial continues the newest final card, it renders inside
   * that card as a streaming tail instead of popping a separate row in and
   * out for every provider micro-final. Mutually exclusive with a standalone
   * partial row.
   */
  private attachedPartialCardIndex: number | null = null
  private version = 0
  private generation = 0
  private snapshotValue: TranscriptFeedModelSnapshot

  constructor(options: TranscriptFeedModelOptions) {
    this.options = options
    this.snapshotValue = this.createSnapshot()
  }

  readonly subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  readonly getSnapshot = (): TranscriptFeedModelSnapshot => this.snapshotValue

  configure(options: Partial<TranscriptFeedModelOptions>): void {
    this.options = { ...this.options, ...options }
  }

  reset(options?: Partial<TranscriptFeedModelOptions>): void {
    this.itemsValue.length = 0
    this.segmentIndex.clear()
    this.cards.clear()
    this.activePartialIndex = null
    this.activeTranslationPartialCardIndex = null
    this.attachedPartialCardIndex = null
    this.translatedSegmentCount = 0
    this.generation += 1
    if (options) this.options = { ...this.options, ...options }
    this.publish()
  }

  appendSegment(segment: TranscriptSegment): boolean {
    if (!this.appendSegmentCore(segment)) return false
    this.publish()
    return true
  }

  setPartial(partial: TranscriptPartial): void {
    const speaker = speakerLabel(partial.speaker)

    // While the utterance continues the newest card, stream the preview
    // inside that card instead of flapping a separate live row.
    let attachIndex = this.findAttachTarget(speaker, partial.startTime)
    if (attachIndex !== null) {
      const rowIndex = this.activePartialIndex
      if (rowIndex !== null) {
        this.removeStandalonePartialRow()
        if (rowIndex < attachIndex) attachIndex -= 1
      }
      if (
        this.attachedPartialCardIndex !== null
        && this.attachedPartialCardIndex !== attachIndex
      ) {
        this.clearAttachedPartialPreview()
      }
      const card = this.itemsValue[attachIndex]
      if (card) {
        this.itemsValue[attachIndex] = {
          ...card,
          original: {
            ...(card.original ?? { language: this.options.sourceLanguage }),
            partialText: partial.text,
            status: 'streaming',
          },
        }
        this.attachedPartialCardIndex = attachIndex
        this.publish()
        return
      }
    }

    if (this.attachedPartialCardIndex !== null) this.clearAttachedPartialPreview()
    const existingIndex = this.activePartialIndex
    const existing = existingIndex === null ? undefined : this.itemsValue[existingIndex]
    const item: TranscriptFeedItem = {
      id: existing?.id ?? `live:${partial.id}`,
      speaker,
      speakerId: speaker,
      startTime: partial.startTime,
      endTime: partial.endTime,
      original: {
        partialText: partial.text,
        status: 'streaming',
        language: this.options.sourceLanguage,
      },
      ...(this.options.translationEnabled
        ? {
            translation: {
              status: 'pending' as const,
              language: this.options.targetLanguage,
            },
          }
        : {}),
    }

    if (existingIndex === null) {
      this.activePartialIndex = this.itemsValue.length
      this.itemsValue.push(item)
    } else {
      this.itemsValue[existingIndex] = item
    }
    this.publish()
  }

  clearPartial(): void {
    let changed = false
    if (this.attachedPartialCardIndex !== null) {
      this.clearAttachedPartialPreview()
      changed = true
    }
    if (this.activePartialIndex !== null) {
      this.removeStandalonePartialRow()
      changed = true
    }
    if (changed) this.publish()
  }

  appendTranslation(translation: TranslationSegment): boolean {
    if (!this.appendTranslationCore(translation)) return false
    this.publish()
    return true
  }

  setTranslationPartial(translation: TranslationPartial): boolean {
    if (!translation.segmentId) return false
    const index = this.segmentIndex.get(translation.segmentId)
    if (index === undefined) return false
    if (
      this.activeTranslationPartialCardIndex !== null
      && this.activeTranslationPartialCardIndex !== index
    ) {
      this.clearTranslationPartialCore()
    }
    const current = this.itemsValue[index]
    if (!current) return false
    const state = this.cards.get(current.id)
    const track = state
      ? this.buildTranslationTrack(state, translation.language, translation.text)
      : {
          partialText: translation.text,
          status: 'streaming' as const,
          language: translation.language,
        }
    this.itemsValue[index] = { ...current, translation: track }
    this.activeTranslationPartialCardIndex = index
    this.publish()
    return true
  }

  clearTranslationPartial(): void {
    if (this.clearTranslationPartialCore()) this.publish()
  }

  markTranslationError(
    segmentIds: readonly string[],
    message: string,
  ): boolean {
    const affected = new Set<number>()
    for (const segmentId of segmentIds) {
      const index = this.segmentIndex.get(segmentId)
      if (index !== undefined) affected.add(index)
    }
    if (affected.size === 0) return false
    for (const index of affected) {
      const current = this.itemsValue[index]
      if (!current) continue
      this.itemsValue[index] = {
        ...current,
        translation: {
          ...current.translation,
          status: 'error',
          errorMessage: message,
          language: current.translation?.language || this.options.targetLanguage,
        },
      }
    }
    this.publish()
    return true
  }

  hydrate(
    segments: readonly TranscriptSegment[],
    translations: readonly TranslationSegment[],
  ): void {
    this.reset()
    for (const segment of segments) this.appendSegmentCore(segment)
    for (const translation of translations) this.appendTranslationCore(translation)
    this.publish()
  }

  /** The display card currently containing this transcript segment. */
  cardIdOf(segmentId: string): string | undefined {
    const index = this.segmentIndex.get(segmentId)
    if (index === undefined) return undefined
    return this.itemsValue[index]?.id
  }

  /**
   * The newest final card this speaker could still be continuing. Attachment
   * intentionally ignores the char/sentence caps: a preview briefly riding a
   * full card beats a live row that pops in and out per micro-final.
   */
  private findAttachTarget(speaker: string, startTime: number): number | null {
    let candidateIndex = this.itemsValue.length - 1
    if (candidateIndex === this.activePartialIndex) candidateIndex -= 1
    if (candidateIndex < 0) return null
    const candidate = this.itemsValue[candidateIndex]
    if (!candidate || !this.cards.has(candidate.id)) return null
    if (candidate.speaker !== speaker) return null
    if (candidate.startTime === undefined || candidate.endTime === undefined) return null
    // A delayed provider event must not preview inside a much newer card just
    // because a negative gap also satisfies the upper-bound comparison.
    if (startTime + PARTIAL_MATCH_TOLERANCE_SECONDS < candidate.startTime) return null
    const gapLimit = endsSentence(candidate.original?.text ?? '')
      ? MERGE_GAP_SECONDS
      : MIDSENTENCE_MERGE_GAP_SECONDS
    if (startTime - candidate.endTime > gapLimit) return null
    return candidateIndex
  }

  /** Restore the attached card to its parts-only presentation. */
  private clearAttachedPartialPreview(): void {
    const index = this.attachedPartialCardIndex
    this.attachedPartialCardIndex = null
    if (index === null) return
    const card = this.itemsValue[index]
    if (!card) return
    const state = this.cards.get(card.id)
    if (!state) return
    this.itemsValue[index] = this.buildCardItem(card, state)
  }

  private clearTranslationPartialCore(): boolean {
    const index = this.activeTranslationPartialCardIndex
    this.activeTranslationPartialCardIndex = null
    if (index === null) return false
    const card = this.itemsValue[index]
    if (!card?.translation?.partialText) return false
    const state = this.cards.get(card.id)
    if (!state) {
      const translation = { ...card.translation }
      delete translation.partialText
      this.itemsValue[index] = { ...card, translation }
      return true
    }
    this.itemsValue[index] = {
      ...card,
      translation: this.buildTranslationTrack(
        state,
        card.translation.language || this.options.targetLanguage,
      ),
    }
    return true
  }

  private removeStandalonePartialRow(): void {
    const index = this.activePartialIndex
    if (index === null) return
    const removedTail = index === this.itemsValue.length - 1
    if (removedTail) this.itemsValue.pop()
    else this.itemsValue.splice(index, 1)
    this.activePartialIndex = null
    if (
      this.attachedPartialCardIndex !== null
      && this.attachedPartialCardIndex > index
    ) {
      this.attachedPartialCardIndex -= 1
    }
    if (
      this.activeTranslationPartialCardIndex !== null
      && this.activeTranslationPartialCardIndex > index
    ) {
      this.activeTranslationPartialCardIndex -= 1
    }
    if (!removedTail) this.rebuildSegmentIndex(index)
  }

  private appendSegmentCore(segment: TranscriptSegment): boolean {
    if (this.segmentIndex.has(segment.id)) return false
    const speaker = speakerLabel(segment.speaker)
    const part: CardPart = {
      segmentId: segment.id,
      text: segment.text.trim(),
      startTime: segment.startTime,
      endTime: segment.endTime,
    }

    const mergeIndex = this.findMergeTarget(speaker, segment)
    if (mergeIndex !== null) {
      const card = this.itemsValue[mergeIndex] as TranscriptFeedItem
      const state = this.cards.get(card.id) as CardState
      this.insertCardPart(state, part)
      this.itemsValue[mergeIndex] = this.buildCardItem(card, state)
      this.segmentIndex.set(segment.id, mergeIndex)
      // The rebuilt card already dropped any attached preview; a preview on a
      // different card is stale once the utterance advanced.
      if (this.attachedPartialCardIndex === mergeIndex) {
        this.attachedPartialCardIndex = null
      } else if (this.attachedPartialCardIndex !== null) {
        this.clearAttachedPartialPreview()
      }
      this.consumePartialFor(segment, speaker)
      return true
    }

    if (this.attachedPartialCardIndex !== null) this.clearAttachedPartialPreview()
    const partialIndex = this.activePartialIndex
    const partialItem = partialIndex === null ? undefined : this.itemsValue[partialIndex]
    const canPromote = partialItem !== undefined && rangesTouch(
      partialItem,
      { speaker, startTime: segment.startTime, endTime: segment.endTime },
      true,
    )

    const id = canPromote && partialItem ? partialItem.id : segment.id
    const state: CardState = { parts: [part] }
    this.cards.set(id, state)
    const item = this.buildCardItem(
      { id, speaker, speakerId: speaker },
      state,
    )

    let index: number
    if (canPromote && partialIndex !== null) {
      const previousStart = this.itemsValue[partialIndex - 1]?.startTime
      if (previousStart === undefined || previousStart <= segment.startTime) {
        // Normal hot path: promote the live tail in place without scanning or
        // rebuilding any long-session index.
        index = partialIndex
        this.itemsValue[index] = item
        this.activePartialIndex = null
      } else {
        // A delayed partial was appended after a much newer final. Relocate
        // this rare event so the permanent cards remain chronological.
        this.removeStandalonePartialRow()
        index = this.findChronologicalInsertionIndex(segment.startTime)
        this.insertItem(index, item)
      }
    } else {
      index = this.findChronologicalInsertionIndex(segment.startTime)
      this.insertItem(index, item)
    }
    this.segmentIndex.set(segment.id, index)
    return true
  }

  private insertCardPart(state: CardState, part: CardPart): void {
    const last = state.parts[state.parts.length - 1]
    if (!last || last.startTime <= part.startTime) {
      state.parts.push(part)
      return
    }
    let low = 0
    let high = state.parts.length
    while (low < high) {
      const middle = (low + high) >>> 1
      if ((state.parts[middle]?.startTime ?? 0) <= part.startTime) low = middle + 1
      else high = middle
    }
    state.parts.splice(low, 0, part)
  }

  private findChronologicalInsertionIndex(startTime: number): number {
    // A standalone live row is always the transient tail and is not part of
    // the ordered final-card range.
    const orderedLength = this.activePartialIndex === this.itemsValue.length - 1
      ? this.itemsValue.length - 1
      : this.itemsValue.length
    const last = this.itemsValue[orderedLength - 1]
    if (!last || (last.startTime ?? Number.POSITIVE_INFINITY) <= startTime) {
      return orderedLength
    }
    let low = 0
    let high = orderedLength
    while (low < high) {
      const middle = (low + high) >>> 1
      const middleStart = this.itemsValue[middle]?.startTime ?? Number.POSITIVE_INFINITY
      if (middleStart <= startTime) low = middle + 1
      else high = middle
    }
    return low
  }

  private insertItem(index: number, item: TranscriptFeedItem): void {
    if (index >= this.itemsValue.length) {
      this.itemsValue.push(item)
      return
    }
    this.itemsValue.splice(index, 0, item)
    for (const [segmentId, currentIndex] of this.segmentIndex) {
      if (currentIndex >= index) this.segmentIndex.set(segmentId, currentIndex + 1)
    }
    if (this.activePartialIndex !== null && this.activePartialIndex >= index) {
      this.activePartialIndex += 1
    }
    if (
      this.attachedPartialCardIndex !== null
      && this.attachedPartialCardIndex >= index
    ) {
      this.attachedPartialCardIndex += 1
    }
    if (
      this.activeTranslationPartialCardIndex !== null
      && this.activeTranslationPartialCardIndex >= index
    ) {
      this.activeTranslationPartialCardIndex += 1
    }
  }

  private appendTranslationCore(translation: TranslationSegment): boolean {
    if (!translation.segmentId) return false
    const index = this.segmentIndex.get(translation.segmentId)
    if (index === undefined) return false
    const current = this.itemsValue[index]
    if (!current) return false
    const state = this.cards.get(current.id)
    if (!state) return false
    const target = state.parts.find((part) => part.segmentId === translation.segmentId)
    if (!target) return false
    if (target.translation === undefined) this.translatedSegmentCount += 1
    target.translation = translation.text.trim()
    // An AI paragraph translation is stored once on its first segment but
    // covers the record's whole time range: sibling fragments inside that
    // range are translated by the same text, not still waiting.
    const rangeStart = translation.startTime - 0.3
    const rangeEnd = translation.endTime + 0.3
    for (const part of state.parts) {
      if (part.translation !== undefined) continue
      if (part.startTime >= rangeStart && part.endTime <= rangeEnd) {
        part.translation = ''
        this.translatedSegmentCount += 1
      }
    }
    this.itemsValue[index] = {
      ...current,
      translation: this.buildTranslationTrack(state, translation.language),
    }
    if (this.activeTranslationPartialCardIndex === index) {
      this.activeTranslationPartialCardIndex = null
    }
    return true
  }

  /**
   * A new final merges into the newest final card when it continues the same
   * speaker within a short pause and none of the readability bounds are hit.
   */
  private findMergeTarget(
    speaker: string,
    segment: TranscriptSegment,
  ): number | null {
    let candidateIndex = this.itemsValue.length - 1
    if (candidateIndex === this.activePartialIndex) candidateIndex -= 1
    if (candidateIndex < 0) return null
    const candidate = this.itemsValue[candidateIndex]
    if (!candidate) return null
    const state = this.cards.get(candidate.id)
    if (!state || state.parts.length === 0) return null
    if (candidate.speaker !== speaker) return null
    if (candidate.endTime === undefined || candidate.startTime === undefined) return null

    // Provider reconnects can deliver an old final after a much newer one.
    // Without a lower bound the negative gap passes every upper-bound check,
    // corrupting the latest card's text and making its end time move backwards.
    if (segment.endTime + PARTIAL_MATCH_TOLERANCE_SECONDS < candidate.startTime) {
      return null
    }
    const cardText = candidate.original?.text ?? ''
    const sentenceComplete = endsSentence(cardText)
    const gap = segment.startTime - candidate.endTime
    // A thinking pause in the middle of a sentence should not split the card.
    if (gap > (sentenceComplete ? MERGE_GAP_SECONDS : MIDSENTENCE_MERGE_GAP_SECONDS)) {
      return null
    }
    if (state.parts.length >= HARD_MAX_CARD_PARTS) return null
    if (segment.endTime - candidate.startTime > HARD_MAX_CARD_SECONDS) return null
    const cardWeight = textWeight(cardText)
    if (cardWeight + textWeight(segment.text.trim()) + 1 > HARD_MAX_CARD_WEIGHT) return null
    if (sentenceComplete && cardWeight >= SENTENCE_BREAK_MIN_WEIGHT) return null
    return candidateIndex
  }

  /**
   * When the final that a live partial was previewing merges into an earlier
   * card, drop the now-redundant streaming row so the text does not show twice.
   */
  private consumePartialFor(segment: TranscriptSegment, speaker: string): void {
    const partialIndex = this.activePartialIndex
    if (partialIndex === null) return
    const partialItem = this.itemsValue[partialIndex]
    if (!partialItem) {
      this.activePartialIndex = null
      return
    }
    const touches = rangesTouch(
      partialItem,
      { speaker, startTime: segment.startTime, endTime: segment.endTime },
      true,
    )
    if (!touches) return
    this.removeStandalonePartialRow()
  }

  private buildCardItem(
    base: Pick<TranscriptFeedItem, 'id' | 'speaker' | 'speakerId'>,
    state: CardState,
  ): TranscriptFeedItem {
    let text = ''
    let startTime = Number.POSITIVE_INFINITY
    let endTime = Number.NEGATIVE_INFINITY
    for (const part of state.parts) {
      text = joinSegmentTexts(text, part.text)
      startTime = Math.min(startTime, part.startTime)
      endTime = Math.max(endTime, part.endTime)
    }
    return {
      id: base.id,
      speaker: base.speaker,
      speakerId: base.speakerId ?? base.speaker,
      startTime,
      endTime,
      segmentIds: state.parts.map((part) => part.segmentId),
      original: {
        text,
        status: 'final',
        language: this.options.sourceLanguage,
      },
      ...(this.options.translationEnabled || state.parts.some((part) => part.translation !== undefined)
        ? { translation: this.buildTranslationTrack(state, this.options.targetLanguage) }
        : {}),
    }
  }

  /**
   * The card's translation is the ordered join of every translated part, so a
   * translation attaches to the whole utterance instead of a single fragment
   * while its siblings stay stuck on a "waiting" placeholder.
   */
  private buildTranslationTrack(
    state: CardState,
    language: string,
    partialText?: string,
  ): TranscriptFeedTrack {
    let text = ''
    let translatedCount = 0
    for (const part of state.parts) {
      if (part.translation === undefined) continue
      translatedCount += 1
      text = joinSegmentTexts(text, part.translation)
    }
    const complete = translatedCount === state.parts.length && translatedCount > 0
    return {
      ...(text ? { text } : {}),
      ...(partialText !== undefined ? { partialText } : {}),
      status: partialText !== undefined
        ? 'streaming'
        : complete
          ? 'final'
          : translatedCount > 0
            ? 'streaming'
            : 'pending',
      language: language || this.options.targetLanguage,
    }
  }

  private rebuildSegmentIndex(removedIndex: number): void {
    for (const [segmentId, index] of this.segmentIndex) {
      if (index > removedIndex) {
        this.segmentIndex.set(segmentId, index - 1)
      }
    }
  }

  private publish(): void {
    this.version += 1
    this.snapshotValue = this.createSnapshot()
    for (const listener of [...this.listeners]) listener()
  }

  private createSnapshot(): TranscriptFeedModelSnapshot {
    return Object.freeze({
      version: this.version,
      generation: this.generation,
      translatedSegmentCount: this.translatedSegmentCount,
      items: this.itemsValue,
    })
  }
}
