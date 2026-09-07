import {
  authFetch,
  ensureValidAccessToken,
  getAccessToken,
} from './pro/api/auth'

// In production, use relative URLs to work with the same origin
import { intlLocale, messages } from './i18n'

const BACKEND_URL = import.meta.env.VITE_BACKEND_URL || 'http://localhost:8080';
const isProduction = BACKEND_URL === '/';

export interface SystemAccessCapabilities {
  anonymousAPIEnabled: boolean
  authenticationEnabled: boolean
  registrationEnabled: boolean
  /** New sign-ups must click an emailed link before they can log in. */
  emailVerificationRequired: boolean
  ragEnabled: boolean
  trainingProgram: TrainingProgramInfo
}

/**
 * Whether this deployment offers the Speechmatics training programme, and the
 * transcription discount a user earns by joining it.
 */
export interface TrainingProgramInfo {
  available: boolean
  discountPercent: number
}

export const TRAINING_PROGRAM_UNAVAILABLE: TrainingProgramInfo = { available: false, discountPercent: 0 }

export async function getSystemAccess(): Promise<SystemAccessCapabilities> {
  const base = isProduction ? '' : BACKEND_URL
  try {
    const response = await fetch(`${base}/api/system/access`, { cache: 'no-store' })
    if (!response.ok) throw new Error(`System access request failed: ${response.status}`)
    const access = await response.json() as {
      anonymous_api_enabled?: boolean
      authentication_enabled?: boolean
      registration_enabled?: boolean
      email_verification_required?: boolean
      rag_enabled?: boolean
      training_program_available?: boolean
      training_discount_percent?: number
    }
    const discount = Number(access.training_discount_percent)
    return {
      anonymousAPIEnabled: access.anonymous_api_enabled === true,
      authenticationEnabled: access.authentication_enabled === true,
      registrationEnabled: access.registration_enabled === true,
      emailVerificationRequired: access.email_verification_required === true,
      ragEnabled: access.rag_enabled === true,
      trainingProgram: access.training_program_available === true
        ? { available: true, discountPercent: Number.isFinite(discount) ? discount : 0 }
        : TRAINING_PROGRAM_UNAVAILABLE,
    }
  } catch {
    return {
      anonymousAPIEnabled: false,
      authenticationEnabled: false,
      registrationEnabled: false,
      emailVerificationRequired: false,
      ragEnabled: false,
      trainingProgram: TRAINING_PROGRAM_UNAVAILABLE,
    }
  }
}

export async function getOptionalAuthHeaders(): Promise<Record<string, string>> {
  if (!getAccessToken()) return {}
  const token = await ensureValidAccessToken()
  return { Authorization: `Bearer ${token}` }
}

export async function canUseAnonymousAPI(): Promise<boolean> {
  if (getAccessToken()) return false
  return (await getSystemAccess()).anonymousAPIEnabled
}

export async function getJwt(): Promise<string> {
  try {
    const url = isProduction ? '/api/token/rt' : `${BACKEND_URL}/api/token/rt`;
    const authHeaders = await getOptionalAuthHeaders()
    const response = await fetch(url, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...authHeaders,
      },
    });

    if (!response.ok) {
      throw new Error(`Failed to get JWT: ${response.statusText}`);
    }

    const data = await response.json();
    return data.token;
  } catch (error) {
    console.error('Error fetching JWT:', error);
    throw error;
  }
}

export type RagConfig = {
  api_key?: string
  api_base?: string
  model?: string
  prompt?: string
}

export type RagAskResponse = {
  answer: string
  usage?: {
    prompt_tokens: number
    completion_tokens: number
    total_tokens: number
    model?: string
    cached_tokens?: number
    cache_write_tokens?: number
  }
  latency_ms?: number
  context?: RagContextMetadata
}

export type RagContextMode = 'smart' | 'full' | 'retrieval'
export type AIReasoningEffort = 'low' | 'medium' | 'high'

export interface RagContextPolicy {
  mode: RagContextMode
  max_tokens: number
}

export interface RagTranscriptSegment {
  id?: string
  speaker?: string
  text: string
  start_time?: number
  end_time?: number
}

export interface RagHistoryMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface RagContextMetadata {
  effective_mode: RagContextMode
  rag_used: boolean
  retrieval_mode?: AIRetrievalMode
  index_status: AIIndexStatus
  estimated_tokens: number
  truncated: boolean
  index_targets?: Array<{
    target_type: 'project' | 'session'
    target_id: string
    index_status: AIIndexStatus
  }>
  sources?: Array<{
    kind: string
    id?: string
    label?: string
    start_time?: number
    end_time?: number
  }>
}

export interface RagAskOptions {
  history?: RagHistoryMessage[]
  clientTranscript?: RagTranscriptSegment[]
  contextPolicy?: RagContextPolicy
  projectId?: string
  retrievalPreference?: AIRetrievalPreference
  clientRequestId?: string
  reasoningEffort?: AIReasoningEffort
}

export async function askRag(
  sessionId: string,
  query: string,
  topK: number = 5,
  config?: RagConfig,
  timeoutMs?: number,
  options?: RagAskOptions,
): Promise<RagAskResponse> {
  const base = isProduction ? '' : BACKEND_URL
  const controller = new AbortController()
  const timeout = timeoutMs && timeoutMs > 0
    ? globalThis.setTimeout(() => controller.abort(), timeoutMs)
    : undefined
  try {
    const authHeaders = await getOptionalAuthHeaders()
    const res = await fetch(`${base}/api/rag/ask`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeaders },
      body: JSON.stringify({
        session_id: sessionId,
        question: query,
        top_k: topK,
        config,
        history: options?.history,
        client_transcript: options?.clientTranscript,
        context_policy: options?.contextPolicy,
        project_id: options?.projectId,
        retrieval_preference: options?.retrievalPreference ?? 'auto',
        client_request_id: options?.clientRequestId,
        reasoning_effort: options?.reasoningEffort,
      }),
      signal: controller.signal,
    })
    if (!res.ok) throw new Error(await res.text())
    return await res.json()
  } catch (reason) {
    if (reason instanceof DOMException && reason.name === 'AbortError') {
      throw new Error(messages().common.errors.aiTimeout, { cause: reason })
    }
    throw reason
  } finally {
    if (timeout) globalThis.clearTimeout(timeout)
  }
}

/**
 * Ask the server to name a session from a transcript excerpt. The server
 * always regenerates on POST, so this doubles as the explicit "regenerate"
 * action; the returned title is already trimmed and bounded (≤12 chars).
 */
export async function generateSessionTitle(
  sessionId: string,
  text: string,
  timeoutMs = 30_000,
): Promise<string> {
  const result = await aiFetchJSON<{ title?: string }>('/api/rag/title', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session_id: sessionId, text }),
  }, timeoutMs)
  return (result.title ?? '').trim()
}

export interface AIArtifact {
  id: string
  artifact_type: 'summary' | 'notes' | 'action_items' | 'concept_map'
  title: string
  content: string
  context_tokens: number
  model?: string
  created_at: string
}

export type AIRetrievalPreference = 'auto' | 'lexical_only'
export type AIRetrievalMode =
  | 'none'
  | 'hybrid'
  | 'semantic'
  | 'lexical_fallback'
  | 'legacy'
export type AIIndexStatus =
  | 'unindexed'
  | 'queued'
  | 'processing'
  | 'ready'
  | 'stale'
  | 'error'
export type AIIndexJobStatus =
  | 'queued'
  | 'processing'
  | 'ready'
  | 'error'
  | 'cancelled'

export async function generateAIArtifact(
  sessionId: string,
  artifactType: AIArtifact['artifact_type'],
  clientTranscript: RagTranscriptSegment[],
  contextPolicy: RagContextPolicy,
  config?: RagConfig,
  projectId?: string,
  retrievalPreference: AIRetrievalPreference = 'auto',
  clientRequestId?: string,
  reasoningEffort: AIReasoningEffort = 'medium',
  timeoutMs = 120_000,
): Promise<{ artifact: AIArtifact; context?: RagContextMetadata; latency_ms?: number }> {
  return aiFetchJSON('/api/ai/artifacts', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      session_id: sessionId,
      artifact_type: artifactType,
      client_transcript: clientTranscript,
      context_policy: contextPolicy,
      config,
      project_id: projectId,
      retrieval_preference: retrievalPreference,
      client_request_id: clientRequestId,
      reasoning_effort: reasoningEffort,
    }),
  }, timeoutMs, Boolean(clientRequestId && getAccessToken()))
}

export interface AIContextPreview extends RagContextMetadata {
  segment_count: number
  preview: string
  requested_retrieval_preference?: AIRetrievalPreference
  preview_retrieval_preference?: AIRetrievalPreference
  semantic_query_executed?: boolean
  semantic_skipped?: boolean
  preview_truncated?: boolean
}

export async function previewAIContext(
  sessionId: string,
  clientTranscript: RagTranscriptSegment[],
  contextPolicy: RagContextPolicy,
  options?: {
    question?: string
    history?: RagHistoryMessage[]
    projectId?: string
    retrievalPreference?: AIRetrievalPreference
    artifactType?: 'summary' | 'notes' | 'action_items'
    topK?: number
    config?: RagConfig
    executeSemantic?: boolean
  },
): Promise<AIContextPreview> {
  const body = await aiFetchJSON<AIContextPreview | {
    context: AIContextPreview
    preview?: string
    segment_count?: number
  }>('/api/ai/context/preview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      session_id: sessionId,
      client_transcript: clientTranscript,
      context_policy: contextPolicy,
      question: options?.question,
      history: options?.history,
      project_id: options?.projectId,
      retrieval_preference: options?.retrievalPreference ?? 'auto',
      artifact_type: options?.artifactType,
      top_k: options?.topK,
      config: options?.config,
      execute_semantic: options?.executeSemantic ?? false,
    }),
  }, 30_000)
  if ('context' in body) {
    return {
      ...body.context,
      preview: body.preview ?? body.context.preview ?? '',
      segment_count: body.segment_count ?? body.context.segment_count ?? 0,
    }
  }
  return body
}

export async function listAIArtifacts(sessionId: string): Promise<AIArtifact[]> {
  const body = await aiFetchJSON<{ artifacts?: AIArtifact[] }>(
    `/api/ai/artifacts?session_id=${encodeURIComponent(sessionId)}`,
  )
  return body.artifacts ?? []
}

export async function deleteAIArtifact(artifactId: string): Promise<void> {
  await aiFetchJSON(`/api/ai/artifacts/${encodeURIComponent(artifactId)}`, {
    method: 'DELETE',
  })
}

export interface AIProject {
  id: string
  name: string
  description: string
  context_mode: RagContextMode
  max_context_tokens: number
  /** Monday of teaching week 1 (YYYY-MM-DD); absent = inferred from sessions. */
  week_start?: string
}

export interface AIProjectListResponse {
  projects: AIProject[]
  linked_project_id: string | null
}

export interface KnowledgeSource {
  id: string
  name: string
  source_type: 'file' | 'memory' | 'lms'
  status: 'queued' | 'processing' | 'ready' | 'error'
  /** Provenance of a material synced from an LMS by the browser extension. */
  lms?: { host?: string; course_shortname?: string; section?: string; modtype?: string; cmid?: number; timemodified?: number }
  error_message?: string
  chunk_count: number
  content?: string
  media_type?: string
  size_bytes?: number
  extracted_text_bytes?: number
  vector_bytes?: number
  created_at?: string
  updated_at?: string
  ocr_languages?: OCRLanguage[]
  index_status?: AIIndexStatus
  embedding_model?: string
  embedding_dimensions?: number
  embedded_chunk_count?: number
  index_error_message?: string
}

const DEFAULT_AI_TIMEOUT_MS = 30_000
const AI_GATEWAY_RETRY_DELAYS_MS = [250, 1_000] as const
const AI_TRANSIENT_GATEWAY_STATUSES = new Set([502, 503, 504])

export class AIRequestError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'AIRequestError'
    this.status = status
  }
}

function waitForAIRetry(delayMs: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) {
    return Promise.reject(signal.reason ?? new DOMException('Request aborted', 'AbortError'))
  }
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      globalThis.clearTimeout(timer)
      reject(signal.reason ?? new DOMException('Request aborted', 'AbortError'))
    }
    const timer = globalThis.setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, delayMs)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

function retryAfterMilliseconds(response: Response): number | null {
  const raw = response.headers.get('Retry-After')?.trim()
  if (!raw) return null
  const seconds = Number(raw)
  if (Number.isFinite(seconds) && seconds >= 0) {
    return Math.min(Math.max(seconds * 1_000, 250), 10_000)
  }
  const retryAt = Date.parse(raw)
  if (!Number.isFinite(retryAt)) return null
  return Math.min(Math.max(retryAt - Date.now(), 250), 10_000)
}

async function aiFetchJSON<T = unknown>(
  path: string,
  init?: RequestInit,
  timeoutMs = DEFAULT_AI_TIMEOUT_MS,
  retryTransientWrite = false,
): Promise<T> {
  const base = isProduction ? '' : BACKEND_URL
  const controller = new AbortController()
  const timeout = globalThis.setTimeout(() => controller.abort(), timeoutMs)
  const authHeaders = await getOptionalAuthHeaders()
  try {
    const method = (init?.method ?? 'GET').toUpperCase()
    const retryable = method === 'GET' || method === 'HEAD' || retryTransientWrite
    let response: Response
    let gatewayRetries = 0
    for (;;) {
      try {
        response = await fetch(`${base}${path}`, {
          ...init,
          headers: { ...authHeaders, ...(init?.headers ?? {}) },
          signal: controller.signal,
        })
      } catch (reason) {
        if (
          !retryable
          || gatewayRetries >= AI_GATEWAY_RETRY_DELAYS_MS.length
          || controller.signal.aborted
          || !(reason instanceof TypeError)
        ) {
          throw reason
        }
        await waitForAIRetry(
          AI_GATEWAY_RETRY_DELAYS_MS[gatewayRetries],
          controller.signal,
        )
        gatewayRetries += 1
        continue
      }
      if (
        retryable
        && AI_TRANSIENT_GATEWAY_STATUSES.has(response.status)
        && gatewayRetries < AI_GATEWAY_RETRY_DELAYS_MS.length
      ) {
        await response.body?.cancel().catch(() => undefined)
        await waitForAIRetry(
          AI_GATEWAY_RETRY_DELAYS_MS[gatewayRetries],
          controller.signal,
        )
        gatewayRetries += 1
        continue
      }
      const retryAfterMs = retryTransientWrite && response.status === 409
        ? retryAfterMilliseconds(response)
        : null
      if (retryAfterMs !== null) {
        await response.body?.cancel().catch(() => undefined)
        await waitForAIRetry(retryAfterMs, controller.signal)
        continue
      }
      break
    }
    if (!response.ok) {
      const responseText = await response.text()
      let message = responseText
      try {
        const parsed = JSON.parse(responseText) as { error?: string; message?: string }
        message = parsed.error ?? parsed.message ?? responseText
      } catch {
        // Plain-text API errors remain useful to the user.
      }
      throw new AIRequestError(
        response.status,
        message || `AI request failed: ${response.status}`,
      )
    }
    if (response.status === httpNoContent) return undefined as T
    const responseText = await response.text()
    return (responseText ? JSON.parse(responseText) : undefined) as T
  } catch (reason) {
    if (reason instanceof DOMException && reason.name === 'AbortError') {
      throw new Error(messages().common.errors.requestTimeout, { cause: reason })
    }
    throw reason
  } finally {
    globalThis.clearTimeout(timeout)
  }
}

const httpNoContent = 204

export async function listAIProjects(sessionId?: string): Promise<AIProjectListResponse> {
  const query = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : ''
  const body = await aiFetchJSON<{
    projects?: AIProject[]
    linked_project_id?: string | null
  }>(`/api/ai/projects${query}`)
  return {
    projects: body.projects ?? [],
    linked_project_id: body.linked_project_id ?? null,
  }
}

export async function createAIProject(
  name: string,
  options?: Partial<Pick<
    AIProject,
    'description' | 'context_mode' | 'max_context_tokens'
  >>,
): Promise<AIProject> {
  const body = await aiFetchJSON<{ project: AIProject }>('/api/ai/projects', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      name,
      description: options?.description ?? '',
      context_mode: options?.context_mode ?? 'smart',
      max_context_tokens: options?.max_context_tokens ?? 64000,
    }),
  })
  return body.project
}

export async function updateAIProject(
  projectId: string,
  update: Partial<Pick<
    AIProject,
    'name' | 'description' | 'context_mode' | 'max_context_tokens' | 'week_start'
  >>,
): Promise<AIProject> {
  const body = await aiFetchJSON<{ project: AIProject }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(update),
    },
  )
  return body.project
}

export async function deleteAIProject(projectId: string): Promise<void> {
  await aiFetchJSON(`/api/ai/projects/${encodeURIComponent(projectId)}`, {
    method: 'DELETE',
  })
}

export interface ProjectSession {
  id: string
  title: string
  source_language: string
  target_language: string
  duration_seconds: number
  status: string
  project_id?: string
  /** How the link was made: by a person, or by the course timetable. */
  assigned_by?: 'manual' | 'timetable'
  started_at: string
  created_at: string
  updated_at: string
}

// 学习模式: course skill maps. The document shape mirrors the server-validated
// skillMapDocument in backend/internal/handlers; the server assigns skill ids
// and guarantees prerequisites only reference earlier skills.
export interface SkillMapEvidence {
  session_id?: string
  session_title?: string
  /** Set instead of session_id when the quote comes from an uploaded material. */
  source_id?: string
  source_title?: string
  quote: string
}

export interface SkillMapSkill {
  id: string
  label: string
  summary?: string
  /** Observable "能……" behavior that shows the skill is held. */
  outcome?: string
  prerequisites?: string[]
  new?: boolean
  evidence?: SkillMapEvidence[]
}

export interface SkillMapDocument {
  version: number
  generated_at: string
  session_count: number
  source_count?: number
  truncated?: boolean
  skills: SkillMapSkill[]
}

export type SkillMapJobStatus = 'queued' | 'processing' | 'ready' | 'error' | 'cancelled'

export interface SkillMapJob {
  id: string
  project_id: string
  status: SkillMapJobStatus
  chunk_count: number
  processed_chunks: number
  /** Actual charge for this generation, set once the job is ready. */
  cost_usd?: number
  error_message?: string
  created_at?: string
  updated_at?: string
}

export interface SkillMapResponse {
  stale?: boolean
  materials_pending?: boolean
  artifact: AIArtifact | null
  map: SkillMapDocument | null
  job?: SkillMapJob | null
  replayed?: boolean
}

export async function getProjectSkillMap(projectId: string): Promise<SkillMapResponse> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/skill-map`,
  )
}

export async function generateProjectSkillMap(
  projectId: string,
  clientRequestId: string,
  reasoningEffort: AIReasoningEffort = 'low',
): Promise<SkillMapResponse> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/skill-map`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        client_request_id: clientRequestId,
        reasoning_effort: reasoningEffort,
      }),
    },
    30_000,
    Boolean(clientRequestId && getAccessToken()),
  )
}

/** Stops an in-flight skill-map generation; the stored map is untouched. */
export async function cancelProjectSkillMap(projectId: string): Promise<SkillMapResponse> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/skill-map`,
    { method: 'DELETE' },
  )
}

// 学习模式 practice loop. Grading, combo, scaffolding, and next-item choice
// run server-side against a frozen per-skill rubric.
export type StudyLevel = 'learner' | 'supervised' | 'hazard' | 'independent' | 'mastered'
export type StudyGrade = 'F' | 'P' | 'C' | 'D' | 'HD'

export interface StudySkillState {
  skill_key: string
  skill_label: string
  level: StudyLevel
  xp_total: number
  attempts_count: number
  clean_streak: number
  last_grade?: string
  last_error_pattern?: string
  updated_at?: string
}

export interface StudyContinue {
  skill_label: string
  level: StudyLevel | 'learner'
  /** Why this skill is next, in the learner's terms. */
  reason?: string
}

export type StudyFormat = 'open' | 'single' | 'multi' | 'cloze' | 'tf'

export interface StudyGlossaryEntry {
  term: string
  gloss: string
}

export interface StudyScenarioContent {
  situation: string
  question: string
  question_zh?: string
  hint?: string
  /** Defaults to open. Answer keys never reach the client. */
  format?: StudyFormat
  options?: string[]
  glossary?: StudyGlossaryEntry[]
  starters?: string[]
  /** Language tier label, e.g. 中文框架 · EN 术语. Display only. */
  lang?: string
}

/** The teaching layer, handed back after an answer (or on request). */
export interface StudyReveal {
  format: StudyFormat
  answer_indexes?: number[]
  answer_text?: string
  answer_bool?: boolean
  model_answer: string
  explanation: string
  gap_to_c?: string
  targets?: string[]
  option_notes?: string[]
}

export interface StudyLessonConcept {
  term: string
  gloss: string
  quote?: string
}

export interface StudyLessonMisconception {
  label: string
  how_to_tell: string
}

export interface StudyLessonDocument {
  rule: string
  concepts: StudyLessonConcept[]
  misconceptions?: StudyLessonMisconception[]
  example: {
    situation: string
    question?: string
    answer: string
    walkthrough?: string
  }
}

export interface StudyLesson {
  id: string
  project_id: string
  skill_key: string
  skill_label: string
  content: StudyLessonDocument
  model?: string
  created_at: string
}

export interface StudyScaffold {
  offer_zh: boolean
  show_zh: boolean
  offer_hint: boolean
  offer_glossary?: boolean
  offer_starters?: boolean
}

export interface StudyServe {
  scenario_id: string
  difficulty: number
  level: StudyLevel
  generated?: boolean
  scenario: StudyScenarioContent
  scaffold?: StudyScaffold
  coach_line?: string
  /** What generating this item cost; 0 when served from the bank. */
  cost_usd?: number
}

export interface StudyGradeResult {
  grade: StudyGrade
  feedback: string
  next_step: string
  bonuses: string[]
  xp: number
  combo?: number
  events?: string[]
  used_hint: boolean
  used_zh?: boolean
  leveled_up: boolean
  state: StudySkillState
  format?: StudyFormat
  answer_correct?: boolean
  language_tip?: string
  /** What grading this answer cost. */
  cost_usd?: number
  difficulty?: number
  difficulty_multiplier?: number
  reveal?: StudyReveal
  /** True below C: the learner may retry the same item after reading the reveal. */
  retry_allowed?: boolean
  /** This pass came right after a miss on the same item. */
  self_corrected?: boolean
}

export interface StudyCostSummary {
  project_id: string
  total_usd: number
  by_feature: Record<string, number>
  operations: number
}

export interface StudyCosts {
  billing_enabled: boolean
  summary: StudyCostSummary
  items: UserUsageItem[]
}

// 按周学习: sessions, materials and skills grouped by teaching week.
export type StudyWeekStatus = 'done' | 'current' | 'behind' | 'upcoming' | 'empty'

export interface StudyWeekSession {
  id: string
  title: string
  started_at: string
}

export interface StudyWeekSource {
  id: string
  name: string
  source_type: KnowledgeSource['source_type']
  section?: string
}

export interface StudyWeekSkill {
  id: string
  label: string
  level: StudyLevel | 'unlit'
  xp_total: number
}

export interface StudyWeek {
  week: number
  label: string
  start?: string
  end?: string
  status: StudyWeekStatus
  sessions: StudyWeekSession[]
  sources: StudyWeekSource[]
  skills: StudyWeekSkill[]
}

export interface StudyWeeks {
  week_start?: string
  week_start_inferred: boolean
  current_week: number
  weeks: StudyWeek[]
  behind_weeks: number[]
  focus?: { week: number; skill_label?: string; reason: string } | null
  unassigned: { sessions: StudyWeekSession[]; sources: StudyWeekSource[]; skills: StudyWeekSkill[] }
}

export async function getStudyWeeks(projectId: string): Promise<StudyWeeks> {
  return aiFetchJSON(`/api/ai/projects/${encodeURIComponent(projectId)}/study/weeks`)
}

/** What one course has cost so far, by feature, plus its latest charges. */
export async function getStudyCosts(projectId: string): Promise<StudyCosts> {
  return aiFetchJSON(`/api/ai/projects/${encodeURIComponent(projectId)}/study/costs`)
}

export async function listStudyStates(projectId: string): Promise<{
  states: StudySkillState[]
  continue: StudyContinue | null
}> {
  const body = await aiFetchJSON<{
    states?: StudySkillState[]
    continue?: StudyContinue | null
  }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/study/state`,
  )
  return { states: body.states ?? [], continue: body.continue ?? null }
}

export async function nextStudyScenario(
  projectId: string,
  skillLabel: string,
  clientRequestId: string,
  practiceSessionId: string,
): Promise<StudyServe> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/study/next`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        skill_label: skillLabel,
        client_request_id: clientRequestId,
        practice_session_id: practiceSessionId,
      }),
    },
    // First call per skill may generate the rubric and bank.
    150_000,
    Boolean(clientRequestId && getAccessToken()),
  )
}

/** The skill's frozen 讲解卡; generated (and charged) on the first call. */
export async function getStudyLesson(
  projectId: string,
  skillLabel: string,
): Promise<{ lesson: StudyLesson; generated: boolean; cost_usd: number }> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/study/lesson?skill_label=${encodeURIComponent(skillLabel)}`,
    undefined,
    120_000,
  )
}

/** "不会，直接看解析": the answer and explanation without grading. */
export async function revealStudyScenario(
  projectId: string,
  scenarioId: string,
): Promise<{ scenario_id: string; reveal: StudyReveal; cost_usd: number }> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/study/reveal`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ scenario_id: scenarioId }),
    },
    90_000,
  )
}

export async function submitStudyAttempt(
  projectId: string,
  input: {
    scenario_id: string
    /** Open: the answer. Cloze: the term for the blank. */
    answer: string
    /** Single / multi: selected option indexes. */
    choices?: number[]
    /** True/false: the judgment. */
    answer_bool?: boolean
    /** Choice, tf and cloze formats: the explanation. */
    reason?: string
    used_hint: boolean
    used_zh: boolean
    practice_session_id: string
  },
  clientRequestId: string,
): Promise<StudyGradeResult> {
  return aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/study/attempts`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...input, client_request_id: clientRequestId }),
    },
    90_000,
    Boolean(clientRequestId && getAccessToken()),
  )
}

export interface StudyHistoryItem {
  id: string
  skill_label: string
  answer: string
  grade?: string
  xp: number
  feedback?: string
  next_step?: string
  created_at: string
  scenario: StudyScenarioContent | null
  reveal?: StudyReveal
}

export async function getStudyHistory(projectId: string, before = ''): Promise<{
  items: StudyHistoryItem[]
  next_cursor: string
}> {
  return aiFetchJSON(`/api/ai/projects/${encodeURIComponent(projectId)}/study/attempts?before=${encodeURIComponent(before)}`)
}

/** Sessions linked to a project, oldest first (course order). */
export async function listProjectSessions(projectId: string): Promise<ProjectSession[]> {
  const body = await aiFetchJSON<{ sessions?: ProjectSession[] }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sessions`,
  )
  return body.sessions ?? []
}

export async function linkProjectSession(projectId: string, sessionId: string): Promise<void> {
  await aiFetchJSON(`/api/ai/projects/${encodeURIComponent(projectId)}/sessions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session_id: sessionId }),
  })
}

export async function unlinkProjectSession(
  projectId: string,
  sessionId: string,
): Promise<void> {
  await aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sessions/${encodeURIComponent(sessionId)}`,
    { method: 'DELETE' },
  )
}

// 课表归类: weekly class times per course, and filing sessions by them.

export interface CourseSlot {
  id: string
  project_id: string
  /** Only the user-wide listing names the course. */
  project_name?: string
  /** ISO weekday: 1 = Monday … 7 = Sunday. */
  weekday: number
  /** Wall-clock "HH:MM" in `timezone`. */
  start: string
  end: string
  timezone: string
  label: string
  created_at: string
}

export type CourseSlotInput = Pick<CourseSlot, 'weekday' | 'start' | 'end' | 'timezone' | 'label'>

export interface TimetableAssignment {
  session_id: string
  title: string
  started_at: string
  duration_seconds: number
  /** Course the session was filed under before, when the run moves it. */
  from_project_id?: string
  project_id: string
  slot_id: string
  overlap_minutes: number
  change: 'assign' | 'move'
}

export interface TimetableClassifyResult {
  assignments: TimetableAssignment[]
  kept: number
  unmatched: number
  scanned: number
  applied: number
  preview: boolean
}

export async function listCourseSlots(projectId: string): Promise<CourseSlot[]> {
  const body = await aiFetchJSON<{ slots?: CourseSlot[] }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/timetable`,
  )
  return body.slots ?? []
}

export async function addCourseSlot(projectId: string, slot: CourseSlotInput): Promise<CourseSlot> {
  const body = await aiFetchJSON<{ slot: CourseSlot }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/timetable`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(slot),
    },
  )
  return body.slot
}

export async function deleteCourseSlot(projectId: string, slotId: string): Promise<void> {
  await aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/timetable/${encodeURIComponent(slotId)}`,
    { method: 'DELETE' },
  )
}

/** Every class time across the user's courses, in week order. */
export async function listTimetable(): Promise<CourseSlot[]> {
  const body = await aiFetchJSON<{ slots?: CourseSlot[] }>('/api/ai/timetable')
  return body.slots ?? []
}

/**
 * Files sessions by the timetable. `apply: false` only reports what would
 * change; manual links are never moved either way.
 */
export async function classifySessionsByTimetable(apply: boolean): Promise<TimetableClassifyResult> {
  return aiFetchJSON<TimetableClassifyResult>('/api/ai/timetable/classify', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ apply }),
  })
}

export async function listKnowledgeSources(projectId: string): Promise<KnowledgeSource[]> {
  const body = await aiFetchJSON<{ sources: KnowledgeSource[] }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources`,
  )
  return body.sources
}

export async function addProjectMemory(
  projectId: string, name: string, content: string,
): Promise<KnowledgeSource> {
  const body = await aiFetchJSON<{ source: KnowledgeSource }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, content }),
    },
  )
  return body.source
}

export async function updateProjectMemory(
  projectId: string,
  sourceId: string,
  update: { name?: string; content?: string },
): Promise<KnowledgeSource> {
  const body = await aiFetchJSON<{ source: KnowledgeSource }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources/${encodeURIComponent(sourceId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(update),
    },
  )
  return body.source
}

export type OCRLanguage = 'eng' | 'chi_sim' | 'jpn' | 'kor'

export async function uploadKnowledgeFile(
  projectId: string,
  file: File,
  ocrLanguages?: OCRLanguage[],
  sessionId?: string,
): Promise<KnowledgeSource> {
  const form = new FormData()
  form.append('file', file)
  for (const language of ocrLanguages ?? []) form.append('ocr_language', language)
  if (sessionId?.trim()) form.append('session_id', sessionId.trim())
  const body = await aiFetchJSON<{ source: KnowledgeSource }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources`,
    { method: 'POST', body: form },
    120_000,
  )
  return body.source
}

export async function retryKnowledgeSource(
  projectId: string,
  sourceId: string,
): Promise<KnowledgeSource> {
  const body = await aiFetchJSON<{ source: KnowledgeSource }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources/${encodeURIComponent(sourceId)}/retry`,
    { method: 'POST' },
  )
  return body.source
}

export async function deleteKnowledgeSource(
  projectId: string, sourceId: string,
): Promise<void> {
  await aiFetchJSON(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources/${encodeURIComponent(sourceId)}`,
    { method: 'DELETE' },
  )
}

export interface AIIndexJob {
  id: string
  target_type: 'project' | 'session'
  target_id: string
  model: string
  dimensions: number
  status: AIIndexJobStatus
  chunk_count: number
  processed_chunks: number
  estimated_tokens: number
  actual_tokens?: number
  /** Estimated charge in USD (wire key kept for backend compatibility). */
  estimated_dp: number
  content_digest?: string
  client_request_id?: string
  error_message?: string
  lease_expires_at?: string
  attempt_count: number
  max_attempts: number
  cancel_requested_at?: string
  started_at?: string
  finished_at?: string
  created_at: string
  updated_at: string
}

export type AIIndexTarget =
  | { targetType: 'project'; targetId: string; projectId: string; sessionId?: string }
  | { targetType: 'session'; targetId: string; sessionId: string; projectId?: undefined }

export interface AIIndexPreview {
  target_type: 'project' | 'session'
  target_id: string
  model: string
  dimensions: number
  source_count: number
  chunk_count: number
  indexed_chunks: number
  pending_chunks: number
  estimated_tokens: number
  /** Estimated charge in USD (wire key kept for backend compatibility). */
  estimated_dp: number
  content_digest?: string
  confirmation_token?: string
  index_status: AIIndexStatus
  current_model?: string
  requires_indexing: boolean
  active_job?: AIIndexJob
}

export async function previewAIIndex(
  target: AIIndexTarget,
): Promise<AIIndexPreview> {
  return aiFetchJSON('/api/ai/index/preview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      target_type: target.targetType,
      target_id: target.targetId,
      project_id: target.projectId,
      session_id: target.sessionId,
    }),
  })
}

export async function createAIIndexJob(request: AIIndexTarget & {
  clientRequestId: string
  confirmationToken: string
}): Promise<AIIndexJob> {
  const body = await aiFetchJSON<{ job: AIIndexJob }>('/api/ai/index/jobs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      target_type: request.targetType,
      target_id: request.targetId,
      project_id: request.projectId,
      session_id: request.sessionId,
      client_request_id: request.clientRequestId,
      confirmation_token: request.confirmationToken,
      confirmed: true,
    }),
  })
  return body.job
}

export async function getAIIndexJob(jobId: string): Promise<AIIndexJob> {
  const body = await aiFetchJSON<{ job: AIIndexJob }>(
    `/api/ai/index/jobs/${encodeURIComponent(jobId)}`,
  )
  return body.job
}

export async function retryAIIndexJob(jobId: string): Promise<AIIndexJob> {
  const body = await aiFetchJSON<{ job: AIIndexJob }>(
    `/api/ai/index/jobs/${encodeURIComponent(jobId)}/retry`,
    { method: 'POST' },
  )
  return body.job
}

export async function cancelAIIndexJob(jobId: string): Promise<AIIndexJob> {
  const body = await aiFetchJSON<{ job: AIIndexJob }>(
    `/api/ai/index/jobs/${encodeURIComponent(jobId)}`,
    { method: 'DELETE' },
  )
  return body.job
}

// Ingest transcript for RAG vector memory
export async function ingestRag(
  sessionId: string,
  speaker: string,
  text: string,
  startTime: number,
  endTime: number,
  timeoutMs = 15_000,
): Promise<void> {
  const base = isProduction ? '' : BACKEND_URL
  const controller = new AbortController()
  const timeout = globalThis.setTimeout(() => controller.abort(), timeoutMs)
  try {
    const authHeaders = await getOptionalAuthHeaders()
    const res = await fetch(`${base}/api/rag/ingest`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeaders },
      body: JSON.stringify({
        session_id: sessionId,
        speaker,
        text,
        start_time: startTime,
        end_time: endTime,
      }),
      signal: controller.signal,
    })
    if (!res.ok) {
      throw new Error(await res.text())
    }
  } finally {
    globalThis.clearTimeout(timeout)
  }
}

// Reset server-side API metrics (overall counters/logs). Useful to start a fresh session view.
export async function resetMetrics(): Promise<void> {
  const base = isProduction ? '' : BACKEND_URL
  try {
    const authHeaders = await getOptionalAuthHeaders()
    await fetch(`${base}/api/metrics/reset`, { method: 'POST', headers: authHeaders })
  } catch {
    // ignore best-effort errors
  }
}

// Dictionary APIs
// Dictionary API removed: we'll integrate with cloud API directly outside this project when needed

// Billing API (USD wallet + expiring grants + membership)
export interface AccountBalance {
  user_id: string
  account_id: string
  wallet_usd: number
  grant_usd: number
  available_usd: number
  lifetime_charged_usd: number
  plan_code: string
  member_active: boolean
  member_until?: string
  auto_topup_enabled: boolean
}

export type PlanFeature =
  | 'premium_models'
  | 'byok'
  | 'batch'
  | 'custom_prompt'
  | 'auto_topup'
  | 'export_ledger'
  | 'api_access'

export interface Plan {
  code: string
  name: string
  is_public: boolean
  active: boolean
  sort: number
  price_usd_month: number
  price_usd_year: number
  usage_discount_percent: number
  storage_gb: number
  retention_days: number
  max_concurrent_sessions: number
  seats: number
  features: Partial<Record<PlanFeature, boolean>> & Record<string, boolean | undefined>
  created_at?: string
  updated_at?: string
}

export type GrantKind = 'trial' | 'topup_bonus' | 'promo' | 'adjustment' | 'settle_return'

export interface GrantItem {
  id: string
  kind: GrantKind
  amount_usd: number
  remaining_usd: number
  expires_at?: string
  note?: string
  created_at: string
}

export interface Membership {
  id: string
  plan_code: string
  interval: 'month' | 'year'
  stripe_subscription_id?: string
  status: string
  current_period_start?: string
  current_period_end?: string
  cancel_at_period_end: boolean
}

export interface AccountSummary extends AccountBalance {
  signup_reward_status?: 'legacy' | 'allowed' | 'review' | 'approved' | 'denied' | 'budget_hold'
  email: string
  name: string
  status: 'active' | 'past_due' | 'suspended'
  plan: Plan
  effective_plan: Plan
  discount_percent: number
  grants: GrantItem[]
  has_payment_method: boolean
  auto_topup_threshold_usd?: number
  auto_topup_amount_usd?: number
  storage_bytes: number
  realtime_hour_usd: number
  estimated_realtime_hours: number
  custom_discount_percent?: number
  membership?: Membership
  training_opt_in: boolean | null
  training_program_available: boolean
  training_discount_percent: number
  promotion_discount_percent?: number
  promotion_discount_until?: string
}

/** The signed-in user's own referral link and how it has performed. */
export interface ReferralSummary {
  code: string
  path: string
  visits: number
  registered: number
  verified: number
}

export async function getUserReferral(): Promise<ReferralSummary> {
  return authFetch<ReferralSummary>('/api/user/referral')
}

export interface TopupTier {
  amount_usd: number
  bonus_percent: number
  bonus_expiry_days: number
  active: boolean
  sort: number
}

export interface PlanHourlyExample {
  plan_code: string
  plan_name: string
  discount_percent: number
  realtime_hour_usd: number
  realtime_upstream_usd: number
  realtime_gross_margin_percent: number
}

export interface BalanceTransaction {
  id: string
  user_id: string
  bucket: 'wallet' | 'grant'
  grant_id?: string | null
  amount_usd: number
  balance_after_usd: number
  transaction_type: 'credit' | 'debit' | 'refund' | 'adjustment'
  reference_type: string | null
  reference_id: string | null
  description: string
  created_by: string | null
  created_at: string
}

export interface PaymentRow {
  id: string
  kind: 'topup' | 'membership' | 'refund'
  amount_usd: number
  bonus_usd: number
  stripe_object_id?: string
  status: string
  description: string
  created_at: string
}

export interface UserBillingAccount {
  account: AccountSummary
  payments_enabled: boolean
}

export interface UserBillingPlans {
  plans: Plan[]
  topup_tiers: TopupTier[]
  hourly: PlanHourlyExample[]
  payments_enabled: boolean
  /** ISO 4217 code Stripe charges in; the ledger is always USD. */
  checkout_currency: string
  /** Units of checkout_currency per 1 USD. */
  checkout_usd_rate: number
}

export interface UserBillingLedger {
  ledger: BalanceTransaction[]
  payments: PaymentRow[]
}

export type BillingCheckoutInput =
  | { kind: 'topup'; amount_usd: number }
  | { kind: 'membership'; plan_code: string; interval: 'month' | 'year' }

export interface AutoTopupInput {
  enabled: boolean
  threshold_usd?: number
  amount_usd?: number
}

/** Cheap balance read used after WebSocket BalanceUpdated messages. */
export async function getUserBalance(): Promise<AccountBalance> {
  return authFetch<AccountBalance>('/api/user/balance')
}

export async function getUserBillingAccount(): Promise<UserBillingAccount> {
  return authFetch<UserBillingAccount>('/api/user/billing/account')
}

export async function getUserBillingPlans(): Promise<UserBillingPlans> {
  const result = await authFetch<Partial<UserBillingPlans>>('/api/user/billing/plans')
  return {
    plans: result.plans ?? [],
    topup_tiers: result.topup_tiers ?? [],
    hourly: result.hourly ?? [],
    payments_enabled: result.payments_enabled === true,
    checkout_currency: (result.checkout_currency ?? 'usd').toLowerCase(),
    checkout_usd_rate: typeof result.checkout_usd_rate === 'number' && result.checkout_usd_rate > 0
      ? result.checkout_usd_rate
      : 1,
  }
}

/**
 * Describes how a USD amount is actually charged when checkout settles in
 * another currency, e.g. "≈ A$31.00". Returns null for USD checkout.
 */
export function formatCheckoutCharge(amountUSD: number, currency: string, usdRate: number): string | null {
  if (!currency || currency === 'usd' || !(usdRate > 0)) return null
  const charged = Math.round(amountUSD * usdRate * 100) / 100
  try {
    return `≈ ${new Intl.NumberFormat('en-US', { style: 'currency', currency: currency.toUpperCase() }).format(charged)}`
  } catch {
    return `≈ ${charged.toFixed(2)} ${currency.toUpperCase()}`
  }
}

export async function getUserBillingLedger(): Promise<UserBillingLedger> {
  const result = await authFetch<Partial<UserBillingLedger>>('/api/user/billing/ledger')
  return { ledger: result.ledger ?? [], payments: result.payments ?? [] }
}

/** Spend for one statement window, split the way a session's cost line is. */
export interface StatementTotals {
  transcription_usd: number
  transcription_seconds: number
  translation_usd: number
  ai_usd: number
  charged_usd: number
  refunded_usd: number
  from_grant_usd: number
  from_wallet_usd: number
  topup_usd: number
  membership_usd: number
}

/** Everything needed to reconcile one period: charges, balance moves, payments. */
export interface UserStatement {
  from: string
  to: string
  usage: UserUsageItem[]
  ledger: BalanceTransaction[]
  payments: PaymentRow[]
  totals: StatementTotals
  truncated: boolean
}

/** `month` takes YYYY-MM; `from`/`to` take YYYY-MM-DD and include both days. */
export interface StatementQuery {
  month?: string
  from?: string
  to?: string
}

function statementSearch(query: StatementQuery): string {
  const params = new URLSearchParams()
  if (query.month) params.set('month', query.month)
  if (query.from) params.set('from', query.from)
  if (query.to) params.set('to', query.to)
  const search = params.toString()
  return search ? `?${search}` : ''
}

export async function getUserStatement(query: StatementQuery = {}): Promise<UserStatement> {
  const result = await authFetch<Partial<UserStatement>>(
    `/api/user/billing/statement${statementSearch(query)}`,
  )
  return {
    from: result.from ?? '',
    to: result.to ?? '',
    usage: result.usage ?? [],
    ledger: result.ledger ?? [],
    payments: result.payments ?? [],
    totals: result.totals ?? {
      transcription_usd: 0, transcription_seconds: 0, translation_usd: 0, ai_usd: 0,
      charged_usd: 0, refunded_usd: 0, from_grant_usd: 0, from_wallet_usd: 0,
      topup_usd: 0, membership_usd: 0,
    },
    truncated: result.truncated === true,
  }
}

/**
 * Fetch the statement as CSV and hand it to the browser. The endpoint needs an
 * Authorization header, so a plain download link cannot reach it — the body is
 * read into a blob and saved from an object URL instead.
 */
export async function downloadUserStatementCSV(query: StatementQuery = {}): Promise<void> {
  const token = await ensureValidAccessToken()
  const base = isProduction ? '' : BACKEND_URL
  const search = statementSearch(query)
  const url = `${base}/api/user/billing/statement${search}${search ? '&' : '?'}format=csv`
  const response = await fetch(url, {
    headers: { Authorization: `Bearer ${token}` },
    cache: 'no-store',
  })
  if (!response.ok) throw new Error(`Statement export failed: ${response.status}`)
  const blob = await response.blob()
  const disposition = response.headers.get('Content-Disposition') ?? ''
  const encoded = /filename\*=UTF-8''([^;]+)/i.exec(disposition)?.[1]
  const filename = encoded ? decodeURIComponent(encoded) : 'yufolo-statement.csv'
  const objectURL = URL.createObjectURL(blob)
  try {
    const link = document.createElement('a')
    link.href = objectURL
    link.download = filename
    document.body.appendChild(link)
    link.click()
    link.remove()
  } finally {
    // Revoking synchronously can race the download in some browsers.
    setTimeout(() => URL.revokeObjectURL(objectURL), 10_000)
  }
}

export async function setUserAutoTopup(input: AutoTopupInput): Promise<AccountSummary> {
  const result = await authFetch<{ account: AccountSummary }>('/api/user/billing/auto-topup', {
    method: 'PUT',
    body: JSON.stringify(input),
  })
  return result.account
}

export async function createBillingCheckout(input: BillingCheckoutInput): Promise<string> {
  const result = await authFetch<{ url: string }>('/api/user/billing/checkout', {
    method: 'POST',
    body: JSON.stringify(input),
  })
  return result.url
}

export async function createBillingPortal(): Promise<string> {
  const result = await authFetch<{ url: string }>('/api/user/billing/portal', {
    method: 'POST',
  })
  return result.url
}

/** Reads the AccountBalance carried by a WebSocket BalanceUpdated message. */
export function parseAccountBalance(value: unknown): AccountBalance | null {
  if (!value || typeof value !== 'object') return null
  const record = value as Record<string, unknown>
  if (typeof record.available_usd !== 'number' || typeof record.user_id !== 'string') return null
  return {
    user_id: record.user_id,
    account_id: typeof record.account_id === 'string' ? record.account_id : '',
    wallet_usd: typeof record.wallet_usd === 'number' ? record.wallet_usd : 0,
    grant_usd: typeof record.grant_usd === 'number' ? record.grant_usd : 0,
    available_usd: record.available_usd,
    lifetime_charged_usd: typeof record.lifetime_charged_usd === 'number'
      ? record.lifetime_charged_usd
      : 0,
    plan_code: typeof record.plan_code === 'string' ? record.plan_code : 'free',
    member_active: record.member_active === true,
    ...(typeof record.member_until === 'string' ? { member_until: record.member_until } : {}),
    auto_topup_enabled: record.auto_topup_enabled === true,
  }
}

const usdFormatters = new Map<number, Intl.NumberFormat>()

/**
 * `US$1.23`; pass 4 digits for tiny per-usage charges. Negative values keep
 * the sign: `-US$0.50`. The prefix is deliberate: the ledger is in US dollars
 * while Stripe may settle in another currency, so a bare `$` would be
 * ambiguous for Australian or Canadian payers.
 */
export function formatUSD(amount: number, digits = 2): string {
  const safe = Number.isFinite(amount) ? amount : 0
  let formatter = usdFormatters.get(digits)
  if (!formatter) {
    formatter = new Intl.NumberFormat('en-US', {
      minimumFractionDigits: digits,
      maximumFractionDigits: digits,
    })
    usdFormatters.set(digits, formatter)
  }
  const text = formatter.format(Math.abs(safe))
  return `${safe < 0 && text !== formatter.format(0) ? '-' : ''}US$${text}`
}

/** Usage lines: 2 decimals normally, 4 decimals once the charge drops below one cent. */
export function formatUsageUSD(amount: number): string {
  return formatUSD(amount, Math.abs(amount) > 0 && Math.abs(amount) < 0.01 ? 4 : 2)
}

/** `≈ 12.5 小时` / `≈ 45 分钟` in the current interface language. */
export function formatHours(hours: number): string {
  const safe = Number.isFinite(hours) ? Math.max(0, hours) : 0
  const { format } = messages()
  if (safe >= 1) {
    return format.approxHours(new Intl.NumberFormat(intlLocale(), { maximumFractionDigits: 1 }).format(safe))
  }
  return format.approxMinutes(Math.floor(safe * 60))
}

export interface UserUsageItem {
  id: string
  session_id: string | null
  action: string
  model: string
  quantity: number
  input_tokens: number
  cached_input_tokens: number
  cache_write_tokens: number
  output_tokens: number
  cost_usd: number
  grant_usd: number
  wallet_usd: number
  attribution: string
  feature?: string
  project_id?: string | null
  settled: boolean
  refunded: boolean
  created_at: string
}

export async function getUserUsage(sessionId?: string): Promise<UserUsageItem[]> {
  const suffix = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : ''
  const result = await authFetch<{ usage: UserUsageItem[] }>(`/api/user/billing/usage${suffix}`)
  return result.usage || []
}

/** One session's charges: realtime work in its own buckets, AI features separate. */
export interface SessionCostSummary {
  session_id: string
  transcription_usd: number
  transcription_seconds: number
  translation_usd: number
  ai_usd: number
  total_usd: number
}

/** Sessions without any attributed usage are absent from the result. */
export async function getSessionCostSummaries(
  sessionIds: readonly string[],
): Promise<SessionCostSummary[]> {
  if (sessionIds.length === 0) return []
  const suffix = `?session_ids=${encodeURIComponent(sessionIds.join(','))}`
  const result = await authFetch<{ session_costs: SessionCostSummary[] }>(
    `/api/user/billing/session-costs${suffix}`,
  )
  return result.session_costs || []
}

export type ModelPurpose = 'translation' | 'summary' | 'chat' | 'embedding'

export interface AvailableModel {
  model_id: string
  purpose: ModelPurpose
  is_default: boolean
}

export interface UserModelPreferences {
  translation_model: string
  summary_model: string
  chat_model: string
}

export async function getAvailableModels(): Promise<AvailableModel[]> {
  const result = await authFetch<{ models: AvailableModel[] }>('/api/models/available')
  return result.models || []
}

export async function getUserModelPreferences(): Promise<UserModelPreferences> {
  return authFetch<UserModelPreferences>('/api/user/model-preferences')
}

export async function saveUserModelPreferences(
  preferences: UserModelPreferences,
): Promise<UserModelPreferences> {
  return authFetch<UserModelPreferences>('/api/user/model-preferences', {
    method: 'PUT',
    body: JSON.stringify(preferences),
  })
}

/** Anonymous pricing for the landing page (`/api/public/pricing`). */
export interface PublicPlan {
  code: string
  name: string
  sort: number
  price_usd_month: number
  price_usd_year: number
  usage_discount_percent: number
  storage_gb: number
  retention_days: number
  max_concurrent_sessions: number
  seats: number
  features: Record<string, boolean | undefined>
  realtime_hour_usd: number
}

export interface PublicTopupTier {
  amount_usd: number
  bonus_percent: number
  bonus_expiry_days: number
}

export interface PublicPricing {
  plans: PublicPlan[]
  topup_tiers: PublicTopupTier[]
  trial_credit_usd: number
  trial_credit_days: number
  payments_enabled: boolean
  checkout_currency: string
  training_program_available?: boolean
  training_discount_percent?: number
}

/** A site notice written in the admin console and shown in the workspace. */
export interface Announcement {
  id: string
  title: string
  body: string
  link_url: string
  link_label: string
  level: 'info' | 'success' | 'warning'
  starts_at: string
  ends_at: string | null
}

/** Notices on display; a signed-in call excludes the ones the account dismissed. */
export async function getAnnouncements(signedIn: boolean): Promise<Announcement[]> {
  if (signedIn) {
    const result = await authFetch<{ announcements?: Announcement[] }>('/api/announcements', { cache: 'no-store' })
    return result.announcements ?? []
  }
  const base = isProduction ? '' : BACKEND_URL
  const response = await fetch(`${base}/api/announcements`, { cache: 'no-store' })
  if (!response.ok) throw new Error(`Announcements request failed: ${response.status}`)
  const result = await response.json() as { announcements?: Announcement[] }
  return result.announcements ?? []
}

export async function dismissAnnouncement(id: string): Promise<void> {
  await authFetch(`/api/announcements/${encodeURIComponent(id)}/dismiss`, { method: 'POST' })
}

export async function getPublicPricing(): Promise<PublicPricing> {
  const base = isProduction ? '' : BACKEND_URL
  const response = await fetch(`${base}/api/public/pricing`, { cache: 'no-store' })
  if (!response.ok) throw new Error(`Pricing request failed: ${response.status}`)
  return await response.json() as PublicPricing
}
