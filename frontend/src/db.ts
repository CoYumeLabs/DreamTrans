import {
  IndexedDbSessionRepository,
  type LegacyMigrationResult,
  type LegacySessionImport,
} from './core/session'

interface ConfirmedSegment {
  text: string
  startTime: number
  endTime: number
}

interface TranscriptLine {
  id: number
  speaker: string
  confirmedSegments: ConfirmedSegment[]
  partialText: string
  lastSegmentEndTime: number
}

interface TranslationLine {
  id: string
  speaker: string
  startTime: number
  content: string
  isPartial: boolean
  original?: string
}

const repository = new IndexedDbSessionRepository<TranscriptLine, TranslationLine>()
let legacyMigrationPromise: Promise<LegacyMigrationResult> | undefined

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function finiteTimestamp(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : fallback
}

function mapLegacySession(
  value: unknown,
  key: string,
): LegacySessionImport<TranscriptLine, TranslationLine> {
  const now = Date.now()
  const raw = isObject(value) ? value : {}
  const id = typeof raw.id === 'string' && raw.id ? raw.id : key
  const timestamp = finiteTimestamp(raw.timestamp, now)
  const lines = Array.isArray(raw.lines)
    ? (raw.lines as TranscriptLine[])
    : []
  const translations = Array.isArray(raw.translations)
    ? (raw.translations as TranslationLine[])
    : []
  const audioBlob = raw.audioBlob instanceof Blob ? raw.audioBlob : null

  return {
    id,
    createdAt: timestamp,
    updatedAt: timestamp,
    status: 'completed',
    completedAt: timestamp,
    transcripts: lines,
    translations,
    ...(audioBlob ? { audioChunks: [audioBlob] } : {}),
    ...(audioBlob?.type ? { audioMimeType: audioBlob.type } : {}),
    ...(typeof raw.title === 'string' ? { title: raw.title } : {}),
    ...(typeof raw.summary === 'string' ? { summary: raw.summary } : {}),
  }
}

async function ensureLegacySessionsMigrated(): Promise<LegacyMigrationResult> {
  if (!legacyMigrationPromise) {
    const migration = repository.migrateLegacySessions(mapLegacySession)
    legacyMigrationPromise = migration.catch((error: unknown) => {
      legacyMigrationPromise = undefined
      throw error
    })
  }
  return legacyMigrationPromise
}

/**
 * Moves sessions stored by the retired Classic UI into the session repository.
 */
export function migrateLegacySessionStorage(): Promise<LegacyMigrationResult> {
  return ensureLegacySessionsMigrated()
}
