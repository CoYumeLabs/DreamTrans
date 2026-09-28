// Shared shapes between the Moodle content script, the background worker and
// the popup. Nothing here ever carries a Moodle credential.

export interface MoodleConfig {
  wwwroot: string
  sesskey: string
  courseId: number
  userId?: number
  theme?: string
  release?: string
  version?: string
}

export type SesskeySource = 'M.cfg' | 'input' | 'link' | 'none'

export interface MoodleContext {
  host: string
  wwwroot: string
  pageUrl: string
  courseId: number
  courseName: string
  shortname: string
  sesskey: string
  sesskeySource: SesskeySource
  userId?: number
  theme?: string
  release?: string
  /** Whether the current page is a course view (course/view.php?id=). */
  onCoursePage: boolean
}

export type RecordingProvider = 'echo360' | 'panopto' | 'youtube' | 'kaltura' | 'other'

export interface CourseFile {
  fileurl: string
  filename: string
  filesize?: number
  mimetype?: string
  timemodified?: number
}

export interface CourseModule {
  cmid: number
  modtype: string
  name: string
  url?: string
  timemodified?: number
  description?: string
  dueAt?: number
  contents: CourseFile[]
  /** url / lti modules pointing at a recording provider. */
  recording?: { provider: RecordingProvider; url: string }
  /** Library reading lists are recognised and never fetched. */
  skipped?: 'library' | 'unsupported' | 'private'
}

export interface CourseSection {
  id: number
  name: string
  order: number
  modules: CourseModule[]
}

export interface CourseTree {
  source: 'state' | 'html' | 'links'
  courseId: number
  sections: CourseSection[]
}

export interface FetchedFile {
  filename: string
  mimetype: string
  bytes: ArrayBuffer
  url: string
  timemodified?: number
}

export interface DerivedFigure {
  png_base64: string
  bbox?: number[]
}

export interface DerivedPage {
  n: number
  text: string
  figures?: DerivedFigure[]
}

export interface DerivedLMS {
  host: string
  course_id: number
  course_shortname: string
  course_name?: string
  section: string
  section_order: number
  cmid: number
  modtype: string
  module_name: string
  url?: string
  timemodified: number
  extractor?: string
}

export interface DerivedDocument {
  sha256: string
  filename: string
  media_type: string
  size_bytes: number
  page_count: number
  pages: DerivedPage[]
  lms: DerivedLMS
}

export interface SyncModuleState {
  timemodified: number
  sha256s: string[]
  uploadedAt: number
  /** Every file of this module that can keep an original has one. */
  originalsKept?: boolean
}

export interface SyncState {
  modules: Record<string, SyncModuleState>
  lastSyncedAt?: number
}

export interface SyncOptions {
  projectId: string
  /** Ignore local state and re-check every module against the server. */
  full: boolean
  /** Upload renders of figure pages (OCR'd server-side, then discarded). */
  uploadFigures: boolean
  /** Opt-in: forum posts (announcements and discussions), post bodies only. */
  includeForums?: boolean
  /** Opt-in: also upload PDF/PPTX/DOCX/images themselves, kept for download. */
  keepOriginals?: boolean
}

export interface SyncProgress {
  phase: 'discover' | 'fetch' | 'extract' | 'upload' | 'done' | 'error'
  message: string
  done?: number
  total?: number
}

export interface SyncSummary {
  scanned: number
  uploaded: number
  duplicates: number
  unchanged: number
  skipped: number
  failed: number
  recordings: Array<{ provider: RecordingProvider; name: string; url: string; section: string }>
  requests: number
  durationMs: number
  errors: string[]
  /** Originals uploaded in this run (keepOriginals only). */
  originals?: number
  /** The user pressed 停止; counts cover what finished before that. */
  stopped?: boolean
}

export interface DiagnosticCheck {
  key: string
  label: string
  ok: boolean | null
  detail: string
}

export interface DiagnosticsReport {
  generatedAt: string
  host: string
  courseId: number
  courseName: string
  checks: DiagnosticCheck[]
  modtypes: Record<string, number>
  recordings: Array<{ provider: RecordingProvider; url: string }>
  libraryLinks: number
  discoverySource: CourseTree['source'] | null
  discoveryMs: number
  requestCount: number
  markdown: string
}

export interface DreamTransProject {
  id: string
  name: string
  description?: string
}

export interface DreamTransStatus {
  connected: boolean
  server: string
  email?: string
  name?: string
}

export interface ServerDerivedRef {
  id: string
  name: string
  sha256: string
  size_bytes: number
  lms: Partial<DerivedLMS>
  created_at: string
  /** The server keeps the file itself (opt-in 保存原文件). */
  has_original?: boolean
}
