// Framework-independent caption presentation types shared by both products.
export type TranscriptTrackStatus = 'pending' | 'streaming' | 'final' | 'error'

export interface TranscriptFeedTrack {
  /** Committed text. */
  text?: string
  /** Uncommitted tail. Keep this separate from text so it can update cheaply. */
  partialText?: string
  status?: TranscriptTrackStatus
  language?: string
  errorMessage?: string
}

export interface TranscriptFeedItem {
  /** Stable and unique for the lifetime of the feed. */
  id: string
  speaker: string
  speakerId?: string
  startTime?: number
  endTime?: number
  /**
   * Atomic transcript segments aggregated into this display card. The
   * underlying store keeps one record per provider final; the feed merges
   * short same-speaker fragments into readable utterances.
   */
  segmentIds?: readonly string[]
  original?: TranscriptFeedTrack
  translation?: TranscriptFeedTrack
}
