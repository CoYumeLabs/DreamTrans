import type { CSSProperties, ReactNode } from 'react'
import type { LearningLevel, TermDomain } from '../../learning'

/**
 * The feed always keeps one logical item per utterance. Switching modes only
 * changes which tracks are rendered; it never filters partial or pending rows.
 */
export type TranscriptFeedMode = 'original' | 'bilingual' | 'translation'

/** Toolbar chrome includes learning assist as a fourth display path. */
export type TranscriptChromeMode = TranscriptFeedMode | 'learn'

export type { TranscriptTrackStatus, TranscriptFeedTrack, TranscriptFeedItem } from '../../core/transcription/feedTypes'
import type { TranscriptFeedItem } from '../../core/transcription/feedTypes'

export interface TranscriptFeedLabels {
  originalTrack: string
  translationTrack: string
  originalPending: string
  translationPending: string
  originalUnavailable: string
  translationUnavailable: string
  streaming: string
  failed: string
  unknownSpeaker: string
  empty: string
  scrollRegion: string
  keyboardHelp: string
  returnToLive: string
  newItems: (count: number) => string
}

export interface TranscriptFeedProps {
  items: readonly TranscriptFeedItem[]
  mode: TranscriptFeedMode
  className?: string
  style?: CSSProperties
  ariaLabel?: string
  labels?: Partial<TranscriptFeedLabels>
  emptyState?: ReactNode
  /**
   * Extra rows mounted before and after the visible range. The DOM still only
   * contains the visible range plus this overscan.
   */
  overscan?: number
  /** Initial row-height estimate, replaced by ResizeObserver measurements. */
  estimatedItemHeight?: number
  /** Distance from the bottom that counts as being live, in CSS pixels. */
  bottomThreshold?: number
  initialFollow?: boolean
  /**
   * Increment this for a non-append reorder/replacement. It is required when
   * the array length and its first/last IDs stay unchanged. Append/remove needs
   * no revision.
   */
  layoutRevision?: string | number
  formatTime?: (seconds: number) => string
  onFollowChange?: (followingLive: boolean) => void
  /**
   * When true, finalized original text shows local CEFR hard-word glosses
   * instead of (or in addition to) full translation tracks.
   */
  learningMode?: boolean
  learningLevel?: LearningLevel
  learningDomains?: readonly TermDomain[]
}

export interface TranscriptFeedHandle {
  focus: () => void
  isFollowingLive: () => boolean
  scrollToLive: (behavior?: ScrollBehavior) => void
}

export interface TranscriptFeedModeSwitchProps {
  value: TranscriptChromeMode
  onChange: (mode: TranscriptChromeMode) => void
  className?: string
  /** Disable 双语/译文 when live translation is off. 原文/学习 always available. */
  translationDisabled?: boolean
  ariaLabel?: string
  labels?: Partial<Record<TranscriptChromeMode, string>>
  /** Tooltip for the learn option. */
  learnTitle?: string
}
