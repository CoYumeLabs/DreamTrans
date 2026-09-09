import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
} from 'react'
import {
  addProjectMemory,
  askRag,
  cancelAIIndexJob,
  createAIIndexJob,
  createAIProject,
  deleteAIArtifact,
  deleteAIProject,
  deleteKnowledgeSource,
  generateAIArtifact,
  getAIIndexJob,
  linkProjectSession,
  listAIArtifacts,
  listAIProjects,
  listKnowledgeSources,
  previewAIContext,
  previewAIIndex,
  retryAIIndexJob,
  retryKnowledgeSource,
  unlinkProjectSession,
  updateAIProject,
  updateProjectMemory,
  uploadKnowledgeFile,
  AIRequestError,
  formatUSD,
  type AIArtifact,
  type AIIndexJob,
  type AIIndexPreview,
  type AIIndexTarget,
  type AIProject,
  type AIReasoningEffort,
  type AIRetrievalMode,
  type AIRetrievalPreference,
  type KnowledgeSource,
  type OCRLanguage,
  type RagConfig,
  type RagContextMetadata,
  type RagContextMode,
  type RagContextPolicy,
  type RagHistoryMessage,
  type RagTranscriptSegment,
} from '../../api'
import MarkdownView from '../../components/MarkdownView'
import { emitMetric } from '../../utils/metrics'
import {
  isInsufficientBalanceError,
  isInsufficientBalanceMessage,
} from '../workspace/billingErrors'
import {
  aiReasoningPreferenceKey,
  chatHistoryKey,
  legacyChatHistoryKey,
} from '../workspace/browserStorageKeys'
import {
  deleteLocalArtifact,
  listLocalArtifacts,
  saveLocalArtifact,
} from '../workspace/AiArtifactStore'
import {
  AssistantScopeGuard,
  type AssistantScopeSnapshot,
} from '../workspace/AssistantScopeGuard'
import { Icon } from './Icon'
import { messages, useMessages } from '../../i18n'

interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  meta?: string
}

interface ScopedChatMessages {
  messages: ChatMessage[]
  scope: AssistantScopeSnapshot
}

interface AssistantPanelProps {
  config?: RagConfig
  /** Opens the account panel when a request is rejected for insufficient balance. */
  onTopUp?: () => void
  ownerId: string | null
  sessionId: string
  sourceLanguage?: string
  suggestedQuestion?: string
  transcriptContext: string
}

interface PendingActionBase {
  clientRequestId: string
  indexClientRequestId: string
  policy: RagContextPolicy
  projectId?: string
  reasoningEffort: AIReasoningEffort
}

interface PendingChatAction extends PendingActionBase {
  kind: 'chat'
  question: string
  history: RagHistoryMessage[]
}

/** The session-scoped artifact types this panel can generate. */
type SessionArtifactType = 'summary' | 'notes' | 'action_items'

interface PendingArtifactAction extends PendingActionBase {
  kind: 'artifact'
  artifactType: SessionArtifactType
}

type PendingAIAction = PendingChatAction | PendingArtifactAction

interface StoredPendingIndex {
  sessionId: string
  job: AIIndexJob
  action: PendingAIAction
}

function contextPresetOptions() {
  return [16_000, 64_000, 128_000, 256_000].map((value, index) => ({
    value,
    label: messages().assistant.contextPresets[index],
  }))
}
function getReasoningOptions(): Array<{
  value: AIReasoningEffort
  label: string
  description: string
  recommended?: boolean
}> {
  const r = messages().assistant.reasoning
  return [
    { value: 'low', ...r.low },
    { value: 'medium', ...r.medium, recommended: true },
    { value: 'high', ...r.high },
  ]
}
function getOcrOptions(): Array<{ value: OCRLanguage; label: string }> { return [
  { value: 'eng', label: 'English' },
  { value: 'chi_sim', label: messages().assistant.ocr.chi_sim },
  { value: 'jpn', label: '日本語' },
  { value: 'kor', label: '한국어' },
] }
/** Shared accept list for chat + knowledge uploads. */
const knowledgeFileAccept = '.pdf,.docx,.xlsx,.csv,.tsv,.txt,.md,.json,.png,.jpg,.jpeg,.webp'

function defaultOCRLanguages(sourceLanguage?: string): OCRLanguage[] {
  const normalized = sourceLanguage?.trim().toLowerCase().replace('_', '-') ?? ''
  if (normalized === 'ja' || normalized.startsWith('ja-') || normalized === 'jpn') {
    return ['jpn']
  }
  if (normalized === 'ko' || normalized.startsWith('ko-') || normalized === 'kor') {
    return ['kor']
  }
  if (
    normalized === 'zh'
    || normalized.startsWith('zh-')
    || normalized === 'cmn'
    || normalized === 'chi'
    || normalized === 'zho'
  ) {
    return ['chi_sim']
  }
  if (normalized === 'en' || normalized.startsWith('en-') || normalized === 'eng') {
    return ['eng']
  }
  return ['eng', 'chi_sim']
}

function isReasoningEffort(value: unknown): value is AIReasoningEffort {
  return value === 'low' || value === 'medium' || value === 'high'
}

function readReasoningEffort(ownerId: string | null): AIReasoningEffort {
  try {
    const stored = localStorage.getItem(aiReasoningPreferenceKey(ownerId))
    return isReasoningEffort(stored) ? stored : 'medium'
  } catch {
    return 'medium'
  }
}

function reasoningEffortLabel(value: AIReasoningEffort): string {
  return getReasoningOptions().find((option) => option.value === value)?.label
    ?? messages().assistant.reasoning.medium.label
}

function reasoningRequestTimeout(value: AIReasoningEffort): number {
  if (value === 'high') return 130_000
  if (value === 'medium') return 100_000
  return 70_000
}

function artifactTypeLabel(type: SessionArtifactType): string {
  switch (type) {
    case 'summary': return messages().assistant.artifactTypes.summary
    case 'notes': return messages().assistant.artifactTypes.notes
    case 'action_items': return messages().assistant.artifactTypes.action_items
  }
}

function contextModeLabel(value: RagContextMode): string {
  switch (value) {
    case 'full': return messages().assistant.contextModes.full
    case 'retrieval': return messages().assistant.contextModes.retrieval
    default: return messages().assistant.contextModes.smart
  }
}

function defaultSessionProjectName(): string {
  return messages().assistant.defaultProject
}
const indexPollIntervalMs = 1_500
const indexPollLimit = 400

function readHistory(ownerId: string | null, sessionId: string): ChatMessage[] {
  if (!sessionId) return []
  try {
    const scopedKey = chatHistoryKey(ownerId, sessionId)
    let serialized = localStorage.getItem(scopedKey)
    if (!serialized && ownerId === null) {
      const legacyKey = legacyChatHistoryKey(sessionId)
      serialized = localStorage.getItem(legacyKey)
      if (serialized) {
        localStorage.setItem(scopedKey, serialized)
        localStorage.removeItem(legacyKey)
      }
    }
    const parsed = JSON.parse(serialized || '[]') as ChatMessage[]
    return Array.isArray(parsed) ? parsed.slice(-50) : []
  } catch {
    return []
  }
}

function transcriptSegments(context: string): RagTranscriptSegment[] {
  return context
    .split('\n')
    .map((line, index) => {
      const match = line.match(/^\[(\d+(?:\.\d+)?)s\]\s+([^:]+):\s*(.*)$/)
      if (!match) return { id: `context-${index}`, text: line.trim() }
      return {
        id: `context-${index}`,
        start_time: Number(match[1]),
        speaker: match[2].trim(),
        text: match[3].trim(),
      }
    })
    .filter((segment) => segment.text)
}

function retrievalModeLabel(mode?: AIRetrievalMode): string {
  switch (mode) {
    case 'hybrid': return messages().assistant.retrievalModes.hybrid
    case 'semantic': return messages().assistant.retrievalModes.semantic
    case 'lexical_fallback': return messages().assistant.retrievalModes.lexical_fallback
    case 'legacy': return messages().assistant.retrievalModes.legacy
    case 'none': return messages().assistant.retrievalModes.none
    default: return messages().assistant.retrievalModes.unknown
  }
}

function indexStatusLabel(status?: string): string {
  switch (status) {
    case 'unindexed': return messages().assistant.indexStatuses.unindexed
    case 'queued': return messages().assistant.indexStatuses.queued
    case 'processing': return messages().assistant.indexStatuses.processing
    case 'ready': return messages().assistant.indexStatuses.ready
    case 'stale': return messages().assistant.indexStatuses.stale
    case 'error': return messages().assistant.indexStatuses.error
    case 'cancelled': return messages().assistant.indexStatuses.cancelled
    default: return status || messages().assistant.indexStatuses.unknown
  }
}

function sourceStatusLabel(status: KnowledgeSource['status']): string {
  switch (status) {
    case 'queued': return messages().assistant.sourceStatuses.queued
    case 'processing': return messages().assistant.sourceStatuses.processing
    case 'ready': return messages().assistant.sourceStatuses.ready
    case 'error': return messages().assistant.sourceStatuses.error
  }
}

function contextDescription(metadata?: RagContextMetadata): string {
  if (!metadata) return ''
  const copy = messages().assistant.context
  const truncated = metadata.truncated ? copy.truncated : ''
  const sources = metadata.sources?.length ? copy.sources(metadata.sources.length) : ''
  return [
    metadata.effective_mode,
    copy.tokens(metadata.estimated_tokens.toLocaleString()),
    retrievalModeLabel(metadata.retrieval_mode),
    indexStatusLabel(metadata.index_status),
  ].join(' · ') + sources + truncated
}

function readableError(reason: unknown): string {
  if (isInsufficientBalanceError(reason)) {
    return messages().workspace.runtime.insufficientBalance
  }
  return reason instanceof Error ? reason.message : String(reason)
}

function newRequestId(): string {
  return globalThis.crypto.randomUUID()
}

function toAIIndexTarget(
  targetType: 'project' | 'session',
  targetId: string,
  currentSessionId?: string,
): AIIndexTarget {
  if (targetType === 'project') {
    return {
      targetType: 'project',
      targetId,
      projectId: targetId,
      sessionId: currentSessionId || undefined,
    }
  }
  return {
    targetType: 'session',
    targetId,
    sessionId: targetId,
  }
}

function sleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => globalThis.setTimeout(resolve, milliseconds))
}

function safeArtifactFilename(title: string): string {
  return title
    .trim()
    .replace(/[<>:"/\\|?*]/g, '-')
    .replace(/\p{Cc}/gu, '')
    .replace(/\s+/g, ' ')
    .slice(0, 80) || 'Yufolo-AI'
}

function downloadArtifact(artifact: AIArtifact): void {
  const url = URL.createObjectURL(new Blob(
    [`# ${artifact.title}\n\n${artifact.content}\n`],
    { type: 'text/markdown;charset=utf-8' },
  ))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `${safeArtifactFilename(artifact.title)}.md`
  anchor.style.display = 'none'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  globalThis.setTimeout(() => URL.revokeObjectURL(url), 1_000)
}

async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }
  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  textarea.remove()
  if (!copied) throw new Error(messages().assistant.clipboardDenied)
}

function pendingIndexStorageKey(ownerId: string, sessionId: string): string {
  return `dreamtrans:ai-index:${encodeURIComponent(ownerId)}:${encodeURIComponent(sessionId)}`
}

function storePendingIndex(
  ownerId: string | null,
  sessionId: string,
  job: AIIndexJob,
  action: PendingAIAction,
): void {
  if (!ownerId || !sessionId) return
  try {
    const stored: StoredPendingIndex = { sessionId, job, action }
    sessionStorage.setItem(
      pendingIndexStorageKey(ownerId, sessionId),
      JSON.stringify(stored),
    )
  } catch {
    // The in-memory poll still works when session storage is unavailable.
  }
}

function readPendingIndex(
  ownerId: string | null,
  sessionId: string,
): StoredPendingIndex | undefined {
  if (!ownerId || !sessionId) return undefined
  try {
    const serialized = sessionStorage.getItem(pendingIndexStorageKey(ownerId, sessionId))
    if (!serialized) return undefined
    const stored = JSON.parse(serialized) as StoredPendingIndex
    if (
      stored.sessionId !== sessionId
      || !stored.job?.id
      || !stored.action?.clientRequestId
      || !stored.action?.indexClientRequestId
    ) return undefined
    if (!isReasoningEffort(stored.action.reasoningEffort)) {
      stored.action.reasoningEffort = 'medium'
    }
    return stored
  } catch {
    return undefined
  }
}

function clearPendingIndex(ownerId: string | null, sessionId: string): void {
  if (!ownerId || !sessionId) return
  try {
    sessionStorage.removeItem(pendingIndexStorageKey(ownerId, sessionId))
  } catch {
    // Nothing else is required when storage is unavailable.
  }
}

export function AssistantPanel({
  config,
  ownerId,
  sessionId,
  sourceLanguage,
  suggestedQuestion,
  transcriptContext,
  onTopUp,
}: AssistantPanelProps) {
  const m = useMessages()
  const a = m.assistant
  const scopeGuardRef = useRef<AssistantScopeGuard | null>(null)
  if (!scopeGuardRef.current) {
    scopeGuardRef.current = new AssistantScopeGuard(ownerId, sessionId)
  }
  const renderScope = scopeGuardRef.current.update(ownerId, sessionId)
  const isCurrentScope = useCallback((scope: AssistantScopeSnapshot) => (
    scopeGuardRef.current?.isCurrent(scope) ?? false
  ), [])

  const [tab, setTab] = useState<'chat' | 'artifacts' | 'memory'>('chat')
  const [scopedMessages, setScopedMessages] = useState<ScopedChatMessages>(() => ({
    messages: readHistory(ownerId, sessionId),
    scope: renderScope,
  }))
  const messages = useMemo(() => (
    scopedMessages.scope.key === renderScope.key
    && scopedMessages.scope.generation === renderScope.generation
  ) ? scopedMessages.messages : [], [renderScope, scopedMessages])
  const setMessages = useCallback((
    update: ChatMessage[] | ((current: ChatMessage[]) => ChatMessage[]),
  ) => {
    setScopedMessages((current) => {
      if (
        !isCurrentScope(renderScope)
        || current.scope.key !== renderScope.key
        || current.scope.generation !== renderScope.generation
      ) return current
      return {
        messages: typeof update === 'function'
          ? update(current.messages)
          : update,
        scope: current.scope,
      }
    })
  }, [isCurrentScope, renderScope])
  const [input, setInput] = useState('')
  const [loading, setLoading] = useState(false)
  const [contextMode, setContextMode] = useState<RagContextMode>('smart')
  const [maxContextTokens, setMaxContextTokens] = useState(64_000)
  const [reasoningEffort, setReasoningEffort] = useState<AIReasoningEffort>(
    () => readReasoningEffort(ownerId),
  )
  const reasoningOwnerRef = useRef(ownerId)
  const [policyOverridden, setPolicyOverridden] = useState(false)
  const [contextMetadata, setContextMetadata] = useState<RagContextMetadata>()
  const [contextPreview, setContextPreview] = useState('')
  const [contextPreviewBusy, setContextPreviewBusy] = useState(false)
  const [artifacts, setArtifacts] = useState<AIArtifact[]>([])
  const [artifactLoading, setArtifactLoading] = useState<SessionArtifactType | null>(null)
  const [artifactError, setArtifactError] = useState('')
  const [projects, setProjects] = useState<AIProject[]>([])
  const [projectRestoreBusy, setProjectRestoreBusy] = useState(Boolean(ownerId))
  const [projectRestoreError, setProjectRestoreError] = useState('')
  const [linkedProjectId, setLinkedProjectId] = useState('')
  const [projectId, setProjectId] = useState('')
  const [newProjectName, setNewProjectName] = useState('')
  const [projectName, setProjectName] = useState('')
  const [projectDescription, setProjectDescription] = useState('')
  const [projectContextMode, setProjectContextMode] = useState<RagContextMode>('smart')
  const [projectMaxTokens, setProjectMaxTokens] = useState(64_000)
  const [sources, setSources] = useState<KnowledgeSource[]>([])
  const [memoryName, setMemoryName] = useState('')
  const [memoryContent, setMemoryContent] = useState('')
  const [editingSourceId, setEditingSourceId] = useState('')
  const [editingMemoryName, setEditingMemoryName] = useState('')
  const [editingMemoryContent, setEditingMemoryContent] = useState('')
  const [ocrLanguages, setOcrLanguages] = useState<OCRLanguage[]>(
    () => defaultOCRLanguages(sourceLanguage),
  )
  const [memoryBusy, setMemoryBusy] = useState(false)
  const [memoryError, setMemoryError] = useState('')
  const [indexConfirmation, setIndexConfirmation] = useState<AIIndexPreview>()
  const [pendingIndexAction, setPendingIndexAction] = useState<PendingAIAction>()
  const [indexJob, setIndexJob] = useState<AIIndexJob>()
  const [indexBusy, setIndexBusy] = useState(false)
  const [indexError, setIndexError] = useState('')
  const [chatBalanceBlocked, setChatBalanceBlocked] = useState(false)
  const [notice, setNotice] = useState('')
  const listRef = useRef<HTMLDivElement>(null)
  const indexGateRef = useRef<HTMLElement>(null)
  const indexPollGenerationRef = useRef(0)
  const resumedIndexJobRef = useRef('')
  const requestActionRef = useRef<
    ((action: PendingAIAction) => Promise<void>) | undefined
  >(undefined)

  const policy: RagContextPolicy = useMemo(
    () => ({ mode: contextMode, max_tokens: maxContextTokens }),
    [contextMode, maxContextTokens],
  )
  const clientTranscript = useMemo(
    () => transcriptSegments(transcriptContext),
    [transcriptContext],
  )
  const selectedProject = useMemo(
    () => projects.find((project) => project.id === projectId),
    [projectId, projects],
  )

  useEffect(() => {
    setOcrLanguages(defaultOCRLanguages(sourceLanguage))
  }, [sessionId, sourceLanguage])

  useEffect(() => {
    if (reasoningOwnerRef.current !== ownerId) {
      reasoningOwnerRef.current = ownerId
      setReasoningEffort(readReasoningEffort(ownerId))
      return
    }
    try {
      localStorage.setItem(
        aiReasoningPreferenceKey(ownerId),
        reasoningEffort,
      )
    } catch {
      // Keep the in-memory preference when browser storage is unavailable.
    }
  }, [ownerId, reasoningEffort])

  const applyProjectPolicy = useCallback((project?: AIProject) => {
    if (!project) return
    setContextMode(project.context_mode)
    setMaxContextTokens(project.max_context_tokens)
    setPolicyOverridden(false)
  }, [])

  useEffect(() => {
    indexPollGenerationRef.current += 1
    setScopedMessages({
      messages: readHistory(ownerId, sessionId),
      scope: renderScope,
    })
    setLoading(false)
    setInput('')
    setArtifacts([])
    setArtifactLoading(null)
    setArtifactError('')
    setContextMetadata(undefined)
    setContextPreview('')
    setContextPreviewBusy(false)
    setProjects([])
    setProjectRestoreBusy(Boolean(ownerId))
    setLinkedProjectId('')
    setProjectId('')
    setNewProjectName('')
    setProjectName('')
    setProjectDescription('')
    setProjectContextMode('smart')
    setProjectMaxTokens(64_000)
    setSources([])
    setMemoryName('')
    setMemoryContent('')
    setMemoryBusy(false)
    setMemoryError('')
    setEditingSourceId('')
    setEditingMemoryName('')
    setEditingMemoryContent('')
    setContextMode('smart')
    setMaxContextTokens(64_000)
    setPolicyOverridden(false)
    setIndexConfirmation(undefined)
    setPendingIndexAction(undefined)
    setIndexJob(undefined)
    setIndexBusy(false)
    setIndexError('')
    setNotice('')
    setProjectRestoreError('')
    resumedIndexJobRef.current = ''
  }, [ownerId, renderScope, sessionId])

  useEffect(() => {
    if (!suggestedQuestion) return
    setTab('chat')
    setInput(suggestedQuestion)
  }, [suggestedQuestion])

  useEffect(() => {
    if (
      !sessionId
      || !isCurrentScope(scopedMessages.scope)
      || scopedMessages.scope.key !== renderScope.key
      || scopedMessages.scope.generation !== renderScope.generation
    ) return
    try {
      localStorage.setItem(
        chatHistoryKey(ownerId, sessionId),
        JSON.stringify(scopedMessages.messages.slice(-50)),
      )
    } catch {
      // Keep current-tab state when browser storage is unavailable.
    }
  }, [
    isCurrentScope,
    ownerId,
    renderScope,
    scopedMessages,
    sessionId,
  ])

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [messages, loading])

  useEffect(() => {
    if (!notice) return
    const timeout = globalThis.setTimeout(() => setNotice(''), 4_000)
    return () => globalThis.clearTimeout(timeout)
  }, [notice])

  useEffect(() => {
    if (!indexConfirmation) return
    const frame = globalThis.requestAnimationFrame(() => indexGateRef.current?.focus())
    return () => globalThis.cancelAnimationFrame(frame)
  }, [indexConfirmation])

  useEffect(() => () => {
    indexPollGenerationRef.current += 1
  }, [])

  useEffect(() => {
    if (!selectedProject) {
      setProjectName('')
      setProjectDescription('')
      return
    }
    setProjectName(selectedProject.name)
    setProjectDescription(selectedProject.description)
    setProjectContextMode(selectedProject.context_mode)
    setProjectMaxTokens(selectedProject.max_context_tokens)
  }, [selectedProject])

  const refreshArtifacts = useCallback(async () => {
    const requestScope = renderScope
    if (!sessionId || !isCurrentScope(requestScope)) return
    setArtifactError('')
    try {
      const nextArtifacts = ownerId
        ? await listAIArtifacts(sessionId)
        : await listLocalArtifacts(sessionId)
      if (!isCurrentScope(requestScope)) return
      setArtifacts(nextArtifacts)
    } catch (reason) {
      if (!isCurrentScope(requestScope)) return
      setArtifactError(readableError(reason))
    }
  }, [isCurrentScope, ownerId, renderScope, sessionId])

  useEffect(() => {
    if (tab === 'artifacts') void refreshArtifacts()
  }, [refreshArtifacts, tab])

  const refreshProjects = useCallback(async (restoreLinkedProject: boolean) => {
    const requestScope = renderScope
    if (!isCurrentScope(requestScope)) return
    if (!ownerId) {
      setProjectRestoreBusy(false)
      setProjectRestoreError('')
      setProjects([])
      setProjectId('')
      setLinkedProjectId('')
      return
    }
    if (restoreLinkedProject) {
      setProjectRestoreBusy(true)
      setProjectRestoreError('')
    }
    setMemoryError('')
    try {
      const response = await listAIProjects(sessionId || undefined)
      if (!isCurrentScope(requestScope)) return
      setProjects(response.projects)
      const linkedId = response.linked_project_id ?? ''
      setLinkedProjectId(linkedId)
      if (restoreLinkedProject) {
        const linkedProject = response.projects.find((project) => project.id === linkedId)
        setProjectId(linkedProject?.id ?? '')
        if (linkedProject) {
          applyProjectPolicy(linkedProject)
        } else {
          setContextMode('smart')
          setMaxContextTokens(64_000)
          setPolicyOverridden(false)
        }
      } else {
        setProjectId((current) => (
          current && response.projects.some((project) => project.id === current)
            ? current
            : linkedId
        ))
      }
    } catch (reason) {
      if (!isCurrentScope(requestScope)) return
      const message = readableError(reason)
      if (restoreLinkedProject) setProjectRestoreError(message)
      else setMemoryError(message)
    } finally {
      if (restoreLinkedProject && isCurrentScope(requestScope)) {
        setProjectRestoreBusy(false)
      }
    }
  }, [
    applyProjectPolicy,
    isCurrentScope,
    ownerId,
    renderScope,
    sessionId,
  ])

  useEffect(() => {
    void refreshProjects(true)
  }, [refreshProjects])

  const refreshSources = useCallback(async () => {
    const requestScope = renderScope
    if (!isCurrentScope(requestScope)) return
    if (!projectId) {
      setSources([])
      return
    }
    try {
      const nextSources = await listKnowledgeSources(projectId)
      if (!isCurrentScope(requestScope)) return
      setSources(nextSources)
    } catch (reason) {
      if (!isCurrentScope(requestScope)) return
      setMemoryError(readableError(reason))
    }
  }, [isCurrentScope, projectId, renderScope])

  useEffect(() => {
    setEditingSourceId('')
    void refreshSources()
  }, [refreshSources])

  useEffect(() => {
    if (!sources.some((source) => (
      source.status === 'queued' || source.status === 'processing'
    ))) return
    const timer = globalThis.setInterval(() => { void refreshSources() }, 3_000)
    return () => globalThis.clearInterval(timer)
  }, [refreshSources, sources])

  const actionTranscript = useCallback((action: PendingAIAction) => (
    action.policy.mode === 'retrieval' ? [] : clientTranscript
  ), [clientTranscript])

  const indexTargetForAction = useCallback((
    action: PendingAIAction,
  ): AIIndexTarget | undefined => {
    if (action.projectId) {
      return {
        targetType: 'project',
        targetId: action.projectId,
        projectId: action.projectId,
        sessionId: sessionId || undefined,
      }
    }
    if (ownerId && sessionId) {
      return {
        targetType: 'session',
        targetId: sessionId,
        sessionId,
      }
    }
    return undefined
  }, [ownerId, sessionId])

  const executeAction = useCallback(async (
    action: PendingAIAction,
    retrievalPreference: AIRetrievalPreference,
  ) => {
    const actionScope = renderScope
    if (!isCurrentScope(actionScope)) return
    if (action.kind === 'chat') {
      setLoading(true)
      setIndexError('')
      setInput((current) => current.trim() === action.question ? '' : current)
      setMessages((current) => [...current, {
        id: newRequestId(),
        role: 'user',
        content: action.question,
      }])
      const startedAt = performance.now()
      try {
        const response = await askRag(
          sessionId,
          action.question,
          6,
          config,
          reasoningRequestTimeout(action.reasoningEffort),
          {
            history: action.history,
            clientTranscript: actionTranscript(action),
            contextPolicy: action.policy,
            projectId: action.projectId,
            retrievalPreference,
            clientRequestId: action.clientRequestId,
            reasoningEffort: action.reasoningEffort,
          },
        )
        if (!isCurrentScope(actionScope)) return
        setContextMetadata(response.context)
        const details = [
          response.usage?.model,
          response.usage?.total_tokens
            ? `${response.usage.total_tokens.toLocaleString()} tokens`
            : '',
          response.usage?.cached_tokens
            ? `${response.usage.cached_tokens.toLocaleString()} cached`
            : '',
          response.context?.retrieval_mode
            ? retrievalModeLabel(response.context.retrieval_mode)
            : '',
          response.latency_ms ? `${(response.latency_ms / 1_000).toFixed(1)}s` : '',
        ].filter(Boolean).join(' · ')
        emitMetric({
          kind: 'chat',
          latency_ms: response.latency_ms ?? performance.now() - startedAt,
          model: response.usage?.model,
        })
        setMessages((current) => [...current, {
          id: newRequestId(),
          role: 'assistant',
          content: response.answer,
          meta: details,
        }])
      } catch (reason) {
        if (!isCurrentScope(actionScope)) return
        if (isInsufficientBalanceError(reason)) setChatBalanceBlocked(true)
        setMessages((current) => [...current, {
          id: newRequestId(),
          role: 'assistant',
          content: a.requestFailed(readableError(reason)),
        }])
      } finally {
        if (isCurrentScope(actionScope)) setLoading(false)
      }
      return
    }

    // Artifact generation runs in the background from the UI's perspective:
    // chat and settings stay usable while the long LLM call completes.
    setArtifactLoading(action.artifactType)
    setArtifactError('')
    setNotice('')
    try {
      const response = await generateAIArtifact(
        sessionId,
        action.artifactType,
        actionTranscript(action),
        action.policy,
        config,
        action.projectId,
        retrievalPreference,
        action.clientRequestId,
        action.reasoningEffort,
        reasoningRequestTimeout(action.reasoningEffort),
      )
      if (!isCurrentScope(actionScope)) return
      setContextMetadata(response.context)
      if (!ownerId) {
        await saveLocalArtifact(sessionId, response.artifact)
        if (!isCurrentScope(actionScope)) return
      }
      await refreshArtifacts()
      if (!isCurrentScope(actionScope)) return
      const title = response.artifact.title?.trim()
        || artifactTypeLabel(action.artifactType)
      setNotice(a.generated(title))
    } catch (reason) {
      if (!isCurrentScope(actionScope)) return
      setArtifactError(readableError(reason))
    } finally {
      if (isCurrentScope(actionScope)) setArtifactLoading(null)
    }
  }, [
    a,
    actionTranscript,
    config,
    isCurrentScope,
    ownerId,
    refreshArtifacts,
    renderScope,
    setMessages,
    sessionId,
  ])

  const pollIndexJob = useCallback(async (
    initialJob: AIIndexJob,
    action: PendingAIAction,
  ) => {
    const pollScope = renderScope
    if (!isCurrentScope(pollScope)) return
    const generation = ++indexPollGenerationRef.current
    let currentJob = initialJob
    setPendingIndexAction(action)
    setIndexConfirmation(undefined)
    setIndexJob(currentJob)
    setIndexBusy(false)
    setIndexError('')
    storePendingIndex(ownerId, sessionId, currentJob, action)
    try {
      for (let attempt = 0; attempt < indexPollLimit; attempt += 1) {
        if (
          generation !== indexPollGenerationRef.current
          || !isCurrentScope(pollScope)
        ) return
        setIndexJob(currentJob)
        storePendingIndex(ownerId, sessionId, currentJob, action)
        if (currentJob.status === 'ready') {
          clearPendingIndex(ownerId, sessionId)
          setIndexJob(undefined)
          setPendingIndexAction(undefined)
          setIndexBusy(false)
          const nextAction = {
            ...action,
            indexClientRequestId: newRequestId(),
          }
          if (requestActionRef.current) {
            await requestActionRef.current(nextAction)
          } else {
            await executeAction(action, 'auto')
          }
          return
        }
        if (
          currentJob.status === 'error'
          || currentJob.status === 'cancelled'
        ) {
          setIndexError(currentJob.error_message || a.indexFailed)
          return
        }
        await sleep(indexPollIntervalMs)
        if (
          generation !== indexPollGenerationRef.current
          || !isCurrentScope(pollScope)
        ) return
        currentJob = await getAIIndexJob(currentJob.id)
      }
      if (!isCurrentScope(pollScope)) return
      setIndexError(a.indexStillRunning)
    } catch (reason) {
      if (!isCurrentScope(pollScope)) return
      setIndexError(readableError(reason))
    } finally {
      if (
        generation === indexPollGenerationRef.current
        && isCurrentScope(pollScope)
      ) setIndexBusy(false)
    }
  }, [
    a,
    executeAction,
    isCurrentScope,
    ownerId,
    renderScope,
    sessionId,
  ])

  useEffect(() => {
    const stored = readPendingIndex(ownerId, sessionId)
    if (!stored || resumedIndexJobRef.current === stored.job.id) return
    resumedIndexJobRef.current = stored.job.id
    void pollIndexJob(stored.job, stored.action)
  }, [ownerId, pollIndexJob, sessionId])

  const requestAction = useCallback(async (action: PendingAIAction) => {
    const actionScope = renderScope
    if (!isCurrentScope(actionScope)) return
    const defaultTarget = indexTargetForAction(action)
    if (!defaultTarget) {
      await executeAction(action, 'auto')
      return
    }
    let target: AIIndexTarget = defaultTarget
    setIndexBusy(true)
    setIndexError('')
    let retrievalNeeded: boolean
    try {
      const assembledPreview = await previewAIContext(
        sessionId,
        actionTranscript(action),
        action.policy,
        {
          question: action.kind === 'chat' ? action.question : undefined,
          history: action.kind === 'chat' ? action.history : undefined,
          projectId: action.projectId,
          artifactType: action.kind === 'artifact' ? action.artifactType : undefined,
          topK: action.kind === 'chat' ? 6 : 20,
          config,
          executeSemantic: false,
        },
      )
      if (!isCurrentScope(actionScope)) return
      setContextMetadata(assembledPreview)
      const pendingTarget = assembledPreview.index_targets?.find(
        (candidate) => candidate.index_status !== 'ready',
      )
      if (pendingTarget) {
        target = toAIIndexTarget(
          pendingTarget.target_type,
          pendingTarget.target_id,
          sessionId,
        )
      }
      // Selecting a project means its knowledge is part of this operation even
      // when free lexical search happens to return no match. Always inspect the
      // project index in that case so the first semantic use cannot silently
      // degrade to an empty lexical result.
      retrievalNeeded = Boolean(action.projectId)
        || assembledPreview.rag_used
        || (
          assembledPreview.retrieval_mode !== undefined
          && assembledPreview.retrieval_mode !== 'none'
        )
        || action.policy.mode === 'retrieval'
    } catch (reason) {
      if (!isCurrentScope(actionScope)) return
      setIndexBusy(false)
      setIndexError(a.contextFailed(readableError(reason)))
      return
    }
    if (!retrievalNeeded) {
      if (!isCurrentScope(actionScope)) return
      setIndexBusy(false)
      await executeAction(action, 'auto')
      return
    }
    try {
      const preview = await previewAIIndex(target)
      if (!isCurrentScope(actionScope)) return
      if (
        preview.index_status === 'ready'
        || preview.chunk_count === 0
        || preview.requires_indexing === false
      ) {
        setIndexBusy(false)
        await executeAction(action, 'auto')
        return
      }
      const activeJob = preview.active_job
      if (
        activeJob
        && (activeJob.status === 'queued' || activeJob.status === 'processing')
      ) {
        await pollIndexJob(activeJob, action)
        return
      }
      setPendingIndexAction(action)
      setIndexConfirmation(preview)
    } catch (reason) {
      if (!isCurrentScope(actionScope)) return
      if (
        target.targetType === 'session'
        && reason instanceof AIRequestError
        && reason.status === 404
      ) {
        // A new/local session may not exist in PostgreSQL yet. The backend
        // keeps explicit indexing disabled and uses compatibility retrieval.
        await executeAction(action, 'auto')
      } else {
        setIndexError(a.indexCheckFailed(readableError(reason)))
      }
    } finally {
      if (isCurrentScope(actionScope)) setIndexBusy(false)
    }
  }, [
    a,
    actionTranscript,
    config,
    executeAction,
    indexTargetForAction,
    isCurrentScope,
    pollIndexJob,
    renderScope,
    sessionId,
  ])
  requestActionRef.current = requestAction

  const send = (event: FormEvent) => {
    event.preventDefault()
    const question = input.trim()
    if (!question || loading || indexBusy || pendingIndexAction) return
    const action: PendingChatAction = {
      kind: 'chat',
      question,
      history: messages.slice(-12).map(({ role, content }) => ({ role, content })),
      clientRequestId: newRequestId(),
      indexClientRequestId: newRequestId(),
      policy: { ...policy },
      projectId: projectId || undefined,
      reasoningEffort,
    }
    void requestAction(action)
  }

  const previewContext = async () => {
    const previewScope = renderScope
    if (!isCurrentScope(previewScope)) return
    setContextPreviewBusy(true)
    setContextPreview('')
    try {
      const preview = await previewAIContext(
        sessionId,
        contextMode === 'retrieval' ? [] : clientTranscript,
        policy,
        {
          question: input.trim() || undefined,
          history: messages
            .slice(-12)
            .map(({ role, content }) => ({ role, content })),
          projectId: projectId || undefined,
          // Context inspection is a free lexical preflight. Paid semantic
          // work only starts after the user confirms an index or submits the
          // actual chat/artifact action.
          executeSemantic: false,
        },
      )
      if (!isCurrentScope(previewScope)) return
      setContextMetadata(preview)
      const sourceLines = preview.sources?.map((source) => (
        `- ${source.label || source.kind}${source.start_time === undefined
          ? ''
          : `（${source.start_time.toFixed(1)}s）`}`
      )) ?? []
      setContextPreview([
        contextDescription(preview),
        a.context.segments(preview.segment_count.toLocaleString()),
        a.context.actualSources(sourceLines.join('\n')),
        preview.preview,
      ].filter(Boolean).join('\n\n'))
    } catch (reason) {
      if (!isCurrentScope(previewScope)) return
      setContextPreview(readableError(reason))
    } finally {
      if (isCurrentScope(previewScope)) setContextPreviewBusy(false)
    }
  }

  const generateArtifact = (artifactType: SessionArtifactType) => {
    if (!sessionId || artifactLoading || indexBusy || pendingIndexAction) return
    const action: PendingArtifactAction = {
      kind: 'artifact',
      artifactType,
      clientRequestId: newRequestId(),
      indexClientRequestId: newRequestId(),
      policy: { ...policy },
      projectId: projectId || undefined,
      reasoningEffort,
    }
    void requestAction(action)
  }

  const reopenIndexConfirmation = async (
    action: PendingAIAction,
    target: AIIndexTarget,
    operationScope: AssistantScopeSnapshot,
  ) => {
    const renewedAction = {
      ...action,
      indexClientRequestId: newRequestId(),
    }
    const preview = await previewAIIndex(target)
    if (!isCurrentScope(operationScope)) return

    indexPollGenerationRef.current += 1
    clearPendingIndex(ownerId, sessionId)
    setIndexJob(undefined)
    setIndexError('')
    if (
      preview.index_status === 'ready'
      || preview.chunk_count === 0
      || preview.requires_indexing === false
    ) {
      setIndexConfirmation(undefined)
      setPendingIndexAction(undefined)
      setIndexBusy(false)
      await executeAction(renewedAction, 'auto')
      return
    }
    if (
      preview.active_job
      && (
        preview.active_job.status === 'queued'
        || preview.active_job.status === 'processing'
      )
    ) {
      await pollIndexJob(preview.active_job, renewedAction)
      return
    }
    setPendingIndexAction(renewedAction)
    setIndexConfirmation(preview)
    setIndexBusy(false)
  }

  const buildSemanticIndex = async () => {
    const operationScope = renderScope
    if (!isCurrentScope(operationScope)) return
    const action = pendingIndexAction
    const confirmedTarget = (
      indexConfirmation?.target_type
      && indexConfirmation.target_id
    )
      ? toAIIndexTarget(
        indexConfirmation.target_type,
        indexConfirmation.target_id,
        sessionId,
      )
      : undefined
    const target = confirmedTarget ?? (action ? indexTargetForAction(action) : undefined)
    const confirmationToken = indexConfirmation?.confirmation_token
    if (!action || !target || !confirmationToken || indexBusy) return
    setIndexBusy(true)
    setIndexError('')
    try {
      const job = await createAIIndexJob({
        ...target,
        clientRequestId: action.indexClientRequestId,
        confirmationToken,
      })
      if (!isCurrentScope(operationScope)) return
      await pollIndexJob(job, action)
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      if (reason instanceof AIRequestError && reason.status === 409) {
        try {
          await reopenIndexConfirmation(action, target, operationScope)
        } catch (previewReason) {
          if (!isCurrentScope(operationScope)) return
          setIndexError(readableError(previewReason))
          setIndexBusy(false)
        }
        return
      }
      setIndexError(readableError(reason))
      setIndexBusy(false)
    }
  }

  const useLexicalOnce = () => {
    const action = pendingIndexAction
    if (!action) return
    indexPollGenerationRef.current += 1
    clearPendingIndex(ownerId, sessionId)
    setIndexConfirmation(undefined)
    setIndexJob(undefined)
    setPendingIndexAction(undefined)
    setIndexError('')
    setIndexBusy(false)
    void executeAction(action, 'lexical_only')
  }

  const cancelPendingAction = async () => {
    const operationScope = renderScope
    if (!isCurrentScope(operationScope)) return
    indexPollGenerationRef.current += 1
    clearPendingIndex(ownerId, sessionId)
    const activeJob = indexJob
    setIndexConfirmation(undefined)
    setIndexJob(undefined)
    setPendingIndexAction(undefined)
    setIndexError('')
    setIndexBusy(false)
    if (activeJob && (activeJob.status === 'queued' || activeJob.status === 'processing')) {
      try {
        await cancelAIIndexJob(activeJob.id)
      } catch (reason) {
        if (!isCurrentScope(operationScope)) return
        setIndexError(a.cancelIndexFailed(readableError(reason)))
      }
    }
  }

  const retryIndexJob = async () => {
    const operationScope = renderScope
    if (!indexJob || !pendingIndexAction || indexBusy) return
    if (!isCurrentScope(operationScope)) return
    setIndexBusy(true)
    setIndexError('')
    try {
      const job = (
        indexJob.status === 'error'
        || indexJob.status === 'cancelled'
      )
        ? await retryAIIndexJob(indexJob.id)
        : await getAIIndexJob(indexJob.id)
      if (!isCurrentScope(operationScope)) return
      await pollIndexJob(job, pendingIndexAction)
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      if (reason instanceof AIRequestError && reason.status === 409) {
        try {
          await reopenIndexConfirmation(
            pendingIndexAction,
            toAIIndexTarget(indexJob.target_type, indexJob.target_id, sessionId),
            operationScope,
          )
        } catch (previewReason) {
          if (!isCurrentScope(operationScope)) return
          setIndexError(readableError(previewReason))
          setIndexBusy(false)
        }
        return
      }
      setIndexError(readableError(reason))
      setIndexBusy(false)
    }
  }

  const createProject = async () => {
    const operationScope = renderScope
    const name = newProjectName.trim()
    if (!name || memoryBusy || !isCurrentScope(operationScope)) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      const project = await createAIProject(name, {
        context_mode: contextMode,
        max_context_tokens: maxContextTokens,
      })
      if (sessionId) await linkProjectSession(project.id, sessionId)
      if (!isCurrentScope(operationScope)) return
      setProjects((current) => [project, ...current])
      setProjectId(project.id)
      setLinkedProjectId(project.id)
      applyProjectPolicy(project)
      setNewProjectName('')
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const selectProject = async (nextProjectId: string) => {
    const operationScope = renderScope
    if (
      memoryBusy
      || nextProjectId === projectId
      || !isCurrentScope(operationScope)
    ) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      if (!nextProjectId) {
        if (linkedProjectId && sessionId) {
          await unlinkProjectSession(linkedProjectId, sessionId)
        }
        if (!isCurrentScope(operationScope)) return
        setProjectId('')
        setLinkedProjectId('')
        setSources([])
        setContextMode('smart')
        setMaxContextTokens(64_000)
        setPolicyOverridden(false)
        return
      }
      if (sessionId) await linkProjectSession(nextProjectId, sessionId)
      if (!isCurrentScope(operationScope)) return
      setProjectId(nextProjectId)
      setLinkedProjectId(nextProjectId)
      applyProjectPolicy(projects.find((project) => project.id === nextProjectId))
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const saveProject = async () => {
    const operationScope = renderScope
    if (
      !projectId
      || !projectName.trim()
      || memoryBusy
      || !isCurrentScope(operationScope)
    ) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      const updated = await updateAIProject(projectId, {
        name: projectName.trim(),
        description: projectDescription.trim(),
        context_mode: projectContextMode,
        max_context_tokens: projectMaxTokens,
      })
      if (!isCurrentScope(operationScope)) return
      setProjects((current) => current.map((project) => (
        project.id === updated.id ? updated : project
      )))
      if (!policyOverridden) applyProjectPolicy(updated)
      setNotice(a.projectSaved)
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const removeProject = async () => {
    const operationScope = renderScope
    if (
      !projectId
      || memoryBusy
      || !isCurrentScope(operationScope)
      || !globalThis.confirm(a.deleteProjectConfirm)
    ) return
    if (!isCurrentScope(operationScope)) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      await deleteAIProject(projectId)
      if (!isCurrentScope(operationScope)) return
      setProjectId('')
      setLinkedProjectId('')
      setSources([])
      setContextMode('smart')
      setMaxContextTokens(64_000)
      setPolicyOverridden(false)
      await refreshProjects(false)
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const addMemory = async () => {
    const operationScope = renderScope
    if (
      !projectId
      || !memoryName.trim()
      || !memoryContent.trim()
      || memoryBusy
      || !isCurrentScope(operationScope)
    ) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      await addProjectMemory(projectId, memoryName.trim(), memoryContent.trim())
      if (!isCurrentScope(operationScope)) return
      setMemoryName('')
      setMemoryContent('')
      await refreshSources()
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const startEditingMemory = (source: KnowledgeSource) => {
    setEditingSourceId(source.id)
    setEditingMemoryName(source.name)
    setEditingMemoryContent(source.content ?? '')
  }

  const saveMemoryEdit = async () => {
    const operationScope = renderScope
    if (
      !projectId
      || !editingSourceId
      || !editingMemoryName.trim()
      || !editingMemoryContent.trim()
      || memoryBusy
      || !isCurrentScope(operationScope)
    ) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      await updateProjectMemory(projectId, editingSourceId, {
        name: editingMemoryName.trim(),
        content: editingMemoryContent.trim(),
      })
      if (!isCurrentScope(operationScope)) return
      setEditingSourceId('')
      await refreshSources()
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  /**
   * Ensure a project exists and is linked for knowledge uploads.
   * Prefer the current selection; otherwise create a default session project.
   * Does not remove any existing project selection UX — only fills a gap.
   */
  const ensureProjectForKnowledge = async (
    operationScope: AssistantScopeSnapshot,
  ): Promise<string | null> => {
    if (projectId) return projectId
    if (!ownerId) {
      setNotice(a.loginToUpload)
      return null
    }
    if (!sessionId) {
      setMemoryError(a.noSession)
      return null
    }
    const existing = projects.find((project) => project.id === linkedProjectId)
    if (existing) {
      setProjectId(existing.id)
      applyProjectPolicy(existing)
      return existing.id
    }
    const project = await createAIProject(defaultSessionProjectName(), {
      description: a.autoProjectDescription,
      context_mode: contextMode,
      max_context_tokens: maxContextTokens,
    })
    await linkProjectSession(project.id, sessionId)
    if (!isCurrentScope(operationScope)) return null
    setProjects((current) => [project, ...current.filter((item) => item.id !== project.id)])
    setProjectId(project.id)
    setLinkedProjectId(project.id)
    applyProjectPolicy(project)
    return project.id
  }

  const uploadFile = async (file?: File, options?: { fromChat?: boolean }) => {
    const operationScope = renderScope
    if (!file || memoryBusy || !isCurrentScope(operationScope)) return
    if (ocrLanguages.length === 0) {
      const message = a.ocrSelect
      if (options?.fromChat) setNotice(message)
      else setMemoryError(message)
      return
    }
    setMemoryBusy(true)
    setMemoryError('')
    try {
      const targetProjectId = await ensureProjectForKnowledge(operationScope)
      if (!targetProjectId || !isCurrentScope(operationScope)) return
      await uploadKnowledgeFile(
        targetProjectId,
        file,
        ocrLanguages,
        sessionId || undefined,
      )
      if (!isCurrentScope(operationScope)) return
      // Use the project id we just uploaded to — React state may not have
      // flushed projectId yet when auto-creating from chat.
      const nextSources = await listKnowledgeSources(targetProjectId)
      if (!isCurrentScope(operationScope)) return
      setSources(nextSources)
      if (options?.fromChat) {
        setNotice(a.fileAdded(file.name))
      }
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      const message = readableError(reason)
      if (options?.fromChat) setNotice(a.uploadFailed(message))
      else setMemoryError(message)
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const onChatFileSelected = (file?: File) => {
    if (!file) return
    void uploadFile(file, { fromChat: true })
  }

  const retrySource = async (source: KnowledgeSource) => {
    const operationScope = renderScope
    if (!projectId || memoryBusy || !isCurrentScope(operationScope)) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      await retryKnowledgeSource(projectId, source.id)
      if (!isCurrentScope(operationScope)) return
      await refreshSources()
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const removeSource = async (source: KnowledgeSource) => {
    const operationScope = renderScope
    if (
      !projectId
      || memoryBusy
      || !isCurrentScope(operationScope)
      || !globalThis.confirm(a.removeSourceConfirm(source.name))
    ) return
    if (!isCurrentScope(operationScope)) return
    setMemoryBusy(true)
    setMemoryError('')
    try {
      await deleteKnowledgeSource(projectId, source.id)
      if (!isCurrentScope(operationScope)) return
      await refreshSources()
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setMemoryError(readableError(reason))
    } finally {
      if (isCurrentScope(operationScope)) setMemoryBusy(false)
    }
  }

  const copyArtifact = async (artifact: AIArtifact) => {
    const operationScope = renderScope
    if (
      !isCurrentScope(operationScope)
      || !artifacts.some((candidate) => candidate.id === artifact.id)
    ) return
    try {
      await copyText(artifact.content)
      if (!isCurrentScope(operationScope)) return
      setNotice(a.copied(artifact.title))
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setArtifactError(a.copyFailed(readableError(reason)))
    }
  }

  const removeArtifact = async (artifact: AIArtifact) => {
    const operationScope = renderScope
    if (
      !isCurrentScope(operationScope)
      || !artifacts.some((candidate) => candidate.id === artifact.id)
    ) return
    if (!globalThis.confirm(a.deleteArtifactConfirm(artifact.title))) return
    if (!isCurrentScope(operationScope)) return
    setArtifactError('')
    try {
      if (ownerId) await deleteAIArtifact(artifact.id)
      else await deleteLocalArtifact(sessionId, artifact.id)
      if (!isCurrentScope(operationScope)) return
      await refreshArtifacts()
    } catch (reason) {
      if (!isCurrentScope(operationScope)) return
      setArtifactError(readableError(reason))
    }
  }

  const indexProgress = indexJob?.chunk_count
    ? Math.min(100, Math.round(indexJob.processed_chunks / indexJob.chunk_count * 100))
    : 0
  // Keep chat and settings free while long-running artifact generation works
  // in the background. Index confirmation still gates both flows because it
  // decides how the next paid request will retrieve context.
  const indexGateOpen = indexBusy
    || pendingIndexAction !== undefined
    || projectRestoreBusy
  const chatBlocked = loading || indexGateOpen
  const artifactStartBlocked = artifactLoading !== null || indexGateOpen
  const settingsBlocked = indexGateOpen

  const chatContextHint = selectedProject
    ? a.contextHint.project(selectedProject.name, sources.length)
    : ownerId
      ? a.contextHint.owner
      : a.contextHint.guest

  const balanceBlocked = chatBalanceBlocked
    || [indexError, memoryError, artifactError].some(isInsufficientBalanceMessage)

  return (
    <div className="dt-assistant">
      {balanceBlocked && (
        <div className="dt-ai-notice dt-ai-notice--balance" role="alert">
          <span>{m.workspace.runtime.insufficientBalance}</span>
          <span className="dt-ai-notice__actions">
            {onTopUp && (
              <button
                className="dt-button dt-button--primary dt-button--small"
                onClick={onTopUp}
                type="button"
              >
                {a.topUp}
              </button>
            )}
            <button
              aria-label={a.dismissBalance}
              className="dt-button dt-button--text dt-button--small"
              onClick={() => setChatBalanceBlocked(false)}
              type="button"
            >
              {a.gotIt}
            </button>
          </span>
        </div>
      )}
      <div className="dt-ai-tabs" role="tablist" aria-label={a.tabsAria}>
        {([
          ['chat', a.tabs.chat, 'message'],
          ['artifacts', a.tabs.artifacts, 'sparkles'],
          ['memory', a.tabs.memory, 'archive'],
        ] as const).map(([value, label, icon]) => (
          <button
            aria-selected={tab === value}
            className={tab === value ? 'is-active' : ''}
            key={value}
            onClick={() => setTab(value)}
            role="tab"
            type="button"
          >
            <Icon name={icon} size={15} />
            <span>{label}</span>
          </button>
        ))}
      </div>

      <details className="dt-ai-settings">
        <summary>
          <span className="dt-ai-settings__summary-icon">
            <Icon name="settings" size={16} />
          </span>
          <span className="dt-ai-settings__summary-copy">
            <strong>{a.preferences.title}</strong>
            <small>
              {reasoningEffortLabel(reasoningEffort)}
              {' · '}
              {contextModeLabel(contextMode)}
              {' · '}
              {Math.round(maxContextTokens / 1_000)}K
              {a.preferences.expand}
            </small>
          </span>
        </summary>
        <div className="dt-ai-settings__body">
          <p className="dt-ai-settings__hint">
            {a.preferences.hint}
          </p>
          <fieldset className="dt-ai-reasoning">
            <legend>{a.preferences.depth}</legend>
            <div aria-label={a.preferences.depthAria} role="radiogroup">
              {getReasoningOptions().map((option) => (
                <button
                  aria-checked={reasoningEffort === option.value}
                  className={reasoningEffort === option.value ? 'is-active' : ''}
                  disabled={settingsBlocked}
                  key={option.value}
                  onClick={() => setReasoningEffort(option.value)}
                  role="radio"
                  type="button"
                >
                  <span>
                    <strong>{option.label}</strong>
                    {option.recommended && <em>{a.reasoning.recommended}</em>}
                  </span>
                  <small>{option.description}</small>
                </button>
              ))}
            </div>
            <small>
              {a.preferences.depthHint}
            </small>
          </fieldset>

          <details className="dt-ai-advanced">
            <summary>
              <span>
                <strong>{a.preferences.advanced}</strong>
                <small>{a.preferences.advancedHint}</small>
              </span>
            </summary>
            <div className="dt-ai-advanced__body">
              <div className="dt-ai-context-controls">
                <label>
                  <span>{a.preferences.readMode}</span>
                  <select
                    aria-label={a.preferences.contextAria}
                    disabled={settingsBlocked}
                    onChange={(event) => {
                      setContextMode(event.target.value as RagContextMode)
                      setPolicyOverridden(true)
                    }}
                    value={contextMode}
                  >
                    <option value="smart">{a.contextModes.smartRecommended}</option>
                    <option value="full">{a.contextModes.full}</option>
                    <option value="retrieval">{a.contextModes.retrieval}</option>
                  </select>
                </label>
                <label>
                  <span>{a.preferences.contextLength}</span>
                  <select
                    aria-label={a.preferences.tokenLimit}
                    disabled={settingsBlocked}
                    onChange={(event) => {
                      setMaxContextTokens(Number(event.target.value))
                      setPolicyOverridden(true)
                    }}
                    value={maxContextTokens}
                  >
                    {contextPresetOptions().map((preset) => (
                      <option key={preset.value} value={preset.value}>{preset.label}</option>
                    ))}
                  </select>
                </label>
              </div>
              <fieldset className="dt-ai-language-picker">
                <legend>{a.ocr.title}</legend>
                {getOcrOptions().map((option) => (
                  <label key={option.value}>
                    <input
                      checked={ocrLanguages.includes(option.value)}
                      onChange={(event) => setOcrLanguages((current) => (
                        event.target.checked
                          ? [...current, option.value]
                          : current.filter((language) => language !== option.value)
                      ))}
                      type="checkbox"
                    />
                    {option.label}
                  </label>
                ))}
              </fieldset>
              {ocrLanguages.length === 0 && (
                <small className="dt-inline-error">{a.ocr.required}</small>
              )}
              <div className="dt-ai-settings__actions">
                <button
                  className="dt-button dt-button--secondary dt-button--small"
                  disabled={contextPreviewBusy || projectRestoreBusy}
                  onClick={() => { void previewContext() }}
                  type="button"
                >
                  {contextPreviewBusy ? a.preferences.previewing : a.preferences.preview}
                </button>
                {policyOverridden && selectedProject && (
                  <button
                    className="dt-button dt-button--text dt-button--small"
                    onClick={() => applyProjectPolicy(selectedProject)}
                    type="button"
                  >
                    {a.preferences.reset}
                  </button>
                )}
              </div>
              {contextMetadata && (
                <div className="dt-ai-context-status" role="status">
                  <strong>{a.preferences.recent}</strong>
                  <span>{contextDescription(contextMetadata)}</span>
                </div>
              )}
              {contextPreview && (
                <details className="dt-ai-context-preview" open>
                  <summary>{a.preferences.actual}</summary>
                  <pre>{contextPreview}</pre>
                </details>
              )}
            </div>
          </details>
        </div>
      </details>

      {(indexConfirmation || indexJob) && pendingIndexAction && (
        <section
          aria-labelledby="dt-index-confirmation-title"
          className="dt-ai-index-gate"
          ref={indexGateRef}
          role="dialog"
          tabIndex={-1}
        >
          <strong id="dt-index-confirmation-title">
            {indexJob ? a.index.preparing : a.index.first}
          </strong>
          {indexConfirmation && !indexJob && (
            <>
              <p>
                {a.index.explanation(Boolean(pendingIndexAction.projectId))}
              </p>
              <details className="dt-ai-index-details">
                <summary>{a.index.details}</summary>
                <dl>
                  <div>
                    <dt>{a.index.model}</dt>
                    <dd>{indexConfirmation.model} · {indexConfirmation.dimensions}d</dd>
                  </div>
                  <div>
                    <dt>{a.index.chunks}</dt>
                    <dd>
                      {(indexConfirmation.pending_chunks ?? indexConfirmation.chunk_count).toLocaleString()}
                      {' / '}
                      {indexConfirmation.chunk_count.toLocaleString()}
                    </dd>
                  </div>
                  <div><dt>{a.index.estimatedTokens}</dt><dd>{indexConfirmation.estimated_tokens.toLocaleString()}</dd></div>
                  <div>
                    <dt>{a.index.estimatedCost}</dt>
                    <dd>{formatUSD(indexConfirmation.estimated_dp, 4)}</dd>
                  </div>
                </dl>
              </details>
            </>
          )}
          {indexJob && (
            <>
              <p aria-live="polite">
                {indexStatusLabel(indexJob.status)}: {' '}
                {a.index.progress(indexJob.processed_chunks.toLocaleString(), indexJob.chunk_count.toLocaleString(), indexProgress)}
              </p>
              <progress max={100} value={indexProgress}>{indexProgress}%</progress>
            </>
          )}
          {indexError && <div className="dt-inline-error" role="alert">{indexError}</div>}
          <div className="dt-ai-action-row">
            {!indexJob && (
              <button
                className="dt-button dt-button--primary"
                disabled={indexBusy}
                onClick={() => { void buildSemanticIndex() }}
                type="button"
              >
                {indexBusy ? a.index.creating : a.index.confirm}
              </button>
            )}
            {indexJob && indexError && (
              <button
                className="dt-button dt-button--primary"
                disabled={indexBusy}
                onClick={() => { void retryIndexJob() }}
                type="button"
              >
                {a.index.retry}
              </button>
            )}
            <button
              className="dt-button dt-button--secondary"
              disabled={indexBusy}
              onClick={useLexicalOnce}
              type="button"
            >
              {a.index.lexical}
            </button>
            <button
              className="dt-button dt-button--text"
              disabled={indexBusy}
              onClick={() => { void cancelPendingAction() }}
              type="button"
            >
              {a.index.cancel}
            </button>
          </div>
        </section>
      )}
      {indexBusy && !indexConfirmation && !indexJob && (
        <div className="dt-ai-notice" aria-live="polite">{a.index.checking}</div>
      )}
      {projectRestoreBusy && (
        <div className="dt-ai-notice" aria-live="polite">{a.index.restoring}</div>
      )}
      {projectRestoreError && (
        <div className="dt-inline-error dt-ai-retry-notice" role="alert">
          <span>{a.index.restoreFailed(projectRestoreError)}</span>
          <button
            className="dt-button dt-button--secondary dt-button--small"
            onClick={() => { void refreshProjects(true) }}
            type="button"
          >
            {a.memory.retry}
          </button>
        </div>
      )}
      {indexError && !pendingIndexAction && (
        <div className="dt-inline-error" role="alert">{indexError}</div>
      )}
      {artifactLoading && (
        <div className="dt-ai-notice dt-ai-notice--progress" aria-live="polite">
          {a.artifacts.generatingTitle(artifactTypeLabel(artifactLoading))} {a.artifacts.continueChat}
        </div>
      )}
      {artifactError && tab !== 'artifacts' && (
        <div className="dt-inline-error dt-ai-retry-notice" role="alert">
          <span>{a.requestFailed(artifactError)}</span>
          <button
            className="dt-button dt-button--secondary dt-button--small"
            onClick={() => setArtifactError('')}
            type="button"
          >
            {m.common.close}
          </button>
        </div>
      )}
      {notice && !artifactLoading && (
        <div className="dt-ai-notice" aria-live="polite">{notice}</div>
      )}

      {tab === 'chat' && (
        <section aria-label={a.chat.aria} className="dt-chat" role="tabpanel">
          <header className="dt-chat__header">
            <span>
              <strong>{a.chat.title}</strong>
              <small>{chatContextHint}</small>
            </span>
            {messages.length > 0 && (
              <button
                className="dt-button dt-button--text dt-button--small"
                disabled={loading}
                onClick={() => {
                  setMessages([])
                  localStorage.removeItem(chatHistoryKey(ownerId, sessionId))
                }}
                type="button"
              >
                {a.chat.clear}
              </button>
            )}
          </header>
          <div className="dt-chat__messages" ref={listRef}>
            {messages.length === 0 && (
              <div className="dt-ai-starter">
                <span className="dt-ai-starter__icon"><Icon name="sparkles" size={22} /></span>
                <strong>{a.chat.emptyTitle}</strong>
                <span>{a.chat.emptyBody}</span>
                <div>
                  {a.starters.map((question) => (
                    <button
                      disabled={chatBlocked}
                      key={question}
                      onClick={() => setInput(question)}
                      type="button"
                    >
                      {question}
                    </button>
                  ))}
                </div>
              </div>
            )}
            {messages.map((message) => (
              <article className={`dt-chat__message dt-chat__message--${message.role}`} key={message.id}>
                <div className="dt-chat__bubble">
                  {message.role === 'assistant'
                    ? <MarkdownView text={message.content} />
                    : message.content}
                </div>
                {message.meta && <small>{message.meta}</small>}
              </article>
            ))}
            {loading && (
              <div className="dt-chat__thinking" aria-label={a.chat.answeringAria}>
                <span /><span /><span />
                <small>{a.reasoning.thinking(reasoningEffortLabel(reasoningEffort))}</small>
              </div>
            )}
          </div>
          <form className="dt-chat__composer" onSubmit={send}>
            {ownerId && sources.length > 0 && (
              <div className="dt-chat__attachments" aria-label={a.chat.attachmentsAria}>
                {sources.slice(0, 4).map((source) => (
                  <button
                    className="dt-chat__attachment-chip"
                    key={source.id}
                    onClick={() => setTab('memory')}
                    type="button"
                  >
                    <Icon name={source.source_type === 'file' ? 'paperclip' : 'archive'} size={12} />
                    <span>{source.name}</span>
                    <small>{sourceStatusLabel(source.status)}</small>
                  </button>
                ))}
                {sources.length > 4 && (
                  <button
                    className="dt-chat__attachment-chip dt-chat__attachment-chip--more"
                    onClick={() => setTab('memory')}
                    type="button"
                  >
                    {a.chat.items(sources.length - 4)}
                  </button>
                )}
              </div>
            )}
            <textarea
              aria-label={a.chat.askAria}
              disabled={chatBlocked}
              onChange={(event) => setInput(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter' && !event.shiftKey) {
                  event.preventDefault()
                  event.currentTarget.form?.requestSubmit()
                }
              }}
              placeholder={a.chat.placeholder}
              rows={3}
              value={input}
            />
            <footer className="dt-chat__composer-footer">
              <div className="dt-chat__composer-tools">
                <label
                  className={
                    memoryBusy || chatBlocked
                      ? 'dt-chat__attach is-disabled'
                      : 'dt-chat__attach'
                  }
                  title={
                    ownerId
                      ? a.chat.upload(a.acceptedFiles)
                      : a.chat.uploadLogin
                  }
                >
                  <Icon name="paperclip" size={16} />
                  <span>{a.chat.attachment}</span>
                  <input
                    accept={knowledgeFileAccept}
                    disabled={memoryBusy || chatBlocked}
                    onChange={(event) => {
                      onChatFileSelected(event.target.files?.[0])
                      event.currentTarget.value = ''
                    }}
                    type="file"
                  />
                </label>
                {memoryBusy && (
                  <small className="dt-chat__upload-status">{a.chat.uploading}</small>
                )}
              </div>
              <div className="dt-chat__composer-send">
                <small>{a.chat.keyboard}</small>
                <button
                  className="dt-button dt-button--primary"
                  disabled={!input.trim() || chatBlocked}
                  type="submit"
                >
                  <Icon name="sparkles" size={15} />
                  {a.chat.send}
                </button>
              </div>
            </footer>
          </form>
        </section>
      )}

      {tab === 'artifacts' && (
        <section aria-label={a.artifacts.aria} className="dt-summary dt-ai-artifacts" role="tabpanel">
          <header className="dt-ai-section-heading">
            <span>
              <strong>{a.artifacts.title}</strong>
              <small>{a.artifacts.lead}</small>
            </span>
          </header>
          <div className="dt-ai-artifact-actions">
            {([
              ['summary', a.artifactTypes.summary, a.artifacts.summary, 'sparkles'],
              ['notes', a.artifactTypes.notes, a.artifacts.notes, 'archive'],
              ['action_items', a.artifactTypes.action_items, a.artifacts.actions, 'check'],
            ] as const).map(([type, label, description, icon]) => (
              <button
                aria-busy={artifactLoading === type}
                className={
                  artifactLoading === type
                    ? 'dt-ai-action-card is-loading'
                    : 'dt-ai-action-card'
                }
                disabled={artifactStartBlocked}
                key={type}
                onClick={() => generateArtifact(type)}
                type="button"
              >
                <span className="dt-ai-action-card__icon"><Icon name={icon} size={18} /></span>
                <span>
                  <strong>{artifactLoading === type ? a.artifacts.generating : label}</strong>
                  <small>
                    {artifactLoading === type
                      ? a.artifacts.continueChat
                      : description}
                  </small>
                </span>
              </button>
            ))}
          </div>
          {artifactError && <div className="dt-inline-error" role="alert">{artifactError}</div>}
          {artifactLoading && (
            <article
              aria-busy="true"
              aria-label={a.artifacts.generatingAria(artifactTypeLabel(artifactLoading))}
              className="dt-ai-artifact dt-ai-artifact--pending"
            >
              <header>
                <span>
                  <strong>{a.artifacts.generatingTitle(artifactTypeLabel(artifactLoading))}</strong>
                  <small>{a.artifacts.processing}</small>
                </span>
              </header>
              <div className="dt-ai-pending-pulse" aria-hidden="true">
                <span /><span /><span />
              </div>
            </article>
          )}
          {artifacts.length === 0 && !artifactError && !artifactLoading && (
            <div className="dt-ai-empty-state">
              <Icon name="archive" size={20} />
              <span>{a.artifacts.empty}</span>
            </div>
          )}
          {artifacts.map((artifact) => (
            <article className="dt-ai-artifact" key={artifact.id}>
              <header>
                <span>
                  <strong>{artifact.title}</strong>
                  <small>
                    {new Date(artifact.created_at).toLocaleString()}
                    {artifact.model ? ` · ${artifact.model}` : ''}
                    {artifact.context_tokens
                      ? a.artifacts.contextTokens(artifact.context_tokens.toLocaleString())
                      : ''}
                  </small>
                </span>
                <div className="dt-ai-action-row">
                  <button className="dt-button dt-button--text dt-button--small" onClick={() => { void copyArtifact(artifact) }} type="button">{a.artifacts.copy}</button>
                  <button className="dt-button dt-button--text dt-button--small" onClick={() => downloadArtifact(artifact)} type="button">{a.artifacts.download}</button>
                  <button className="dt-button dt-button--text dt-button--small" onClick={() => { void removeArtifact(artifact) }} type="button">{a.artifacts.delete}</button>
                </div>
              </header>
              {artifact.content.trim() ? (
                <MarkdownView text={artifact.content} />
              ) : (
                <div className="dt-inline-error" role="alert">
                  {a.artifacts.emptyBody}
                </div>
              )}
            </article>
          ))}
        </section>
      )}

      {tab === 'memory' && (
        <section aria-label={a.memory.aria} className="dt-summary dt-ai-memory" role="tabpanel">
          <header className="dt-ai-section-heading">
            <span>
              <strong>{a.memory.title}</strong>
              <small>{a.memory.lead}</small>
            </span>
          </header>
          {!ownerId ? (
            <div className="dt-empty dt-empty--compact">
              <strong>{a.memory.loginTitle}</strong>
              <span>{a.memory.loginBody}</span>
            </div>
          ) : (
            <>
              <section className="dt-ai-project-switcher">
                <div className="dt-summary__toolbar">
                  <label className="dt-ai-field">
                    {a.memory.currentProject}
                    <select
                      disabled={memoryBusy}
                      onChange={(event) => { void selectProject(event.target.value) }}
                      value={projectId}
                    >
                      <option value="">{a.memory.none}</option>
                      {projects.map((project) => (
                        <option key={project.id} value={project.id}>{project.name}</option>
                      ))}
                    </select>
                  </label>
                  {linkedProjectId && (
                    <button
                      className="dt-button dt-button--text dt-button--small"
                      disabled={memoryBusy}
                      onClick={() => { void selectProject('') }}
                      type="button"
                    >
                      {a.memory.unlink}
                    </button>
                  )}
                </div>
                <div className="dt-ai-memory-row">
                  <input
                    aria-label={a.memory.newNameAria}
                    onChange={(event) => setNewProjectName(event.target.value)}
                    placeholder={a.memory.newNamePlaceholder}
                    value={newProjectName}
                  />
                  <button className="dt-button dt-button--secondary" disabled={!newProjectName.trim() || memoryBusy} onClick={() => { void createProject() }} type="button">
                    {a.memory.create}
                  </button>
                </div>
                {!selectedProject && (
                  <p className="dt-ai-settings__hint">
                    {a.memory.autoProjectHint(defaultSessionProjectName())}
                  </p>
                )}
              </section>
              {selectedProject && (
                <>
                  <details className="dt-ai-project-editor">
                    <summary>
                      <span>
                        <strong id="dt-project-settings-title">{a.memory.projectSettings}</strong>
                        <small>{contextModeLabel(projectContextMode)} · {Math.round(projectMaxTokens / 1_000)}K</small>
                      </span>
                    </summary>
                    <div className="dt-ai-project-editor__body">
                      <label className="dt-ai-field">
                        {a.memory.name}
                        <input onChange={(event) => setProjectName(event.target.value)} value={projectName} />
                      </label>
                      <label className="dt-ai-field">
                        {a.memory.description}
                        <textarea onChange={(event) => setProjectDescription(event.target.value)} rows={3} value={projectDescription} />
                      </label>
                      <div className="dt-ai-memory-row">
                        <label className="dt-ai-field">
                          {a.memory.defaultMode}
                          <select onChange={(event) => setProjectContextMode(event.target.value as RagContextMode)} value={projectContextMode}>
                            <option value="smart">{a.contextModes.smartRecommended}</option>
                            <option value="full">{a.contextModes.full}</option>
                            <option value="retrieval">{a.contextModes.retrieval}</option>
                          </select>
                        </label>
                        <label className="dt-ai-field">
                          {a.memory.defaultLength}
                          <select onChange={(event) => setProjectMaxTokens(Number(event.target.value))} value={projectMaxTokens}>
                  {contextPresetOptions().map((preset) => (
                              <option key={preset.value} value={preset.value}>{preset.label}</option>
                            ))}
                          </select>
                        </label>
                      </div>
                      <div className="dt-ai-action-row">
                        <button className="dt-button dt-button--secondary" disabled={memoryBusy || !projectName.trim()} onClick={() => { void saveProject() }} type="button">
                          {a.memory.saveProject}
                        </button>
                        <button className="dt-button dt-button--text" disabled={memoryBusy} onClick={() => { void removeProject() }} type="button">
                          {a.memory.deleteProject}
                        </button>
                      </div>
                    </div>
                  </details>

                  <section className="dt-ai-knowledge-add">
                    <header>
                      <span>
                        <strong>{a.memory.addTitle}</strong>
                        <small>{a.memory.addLead}</small>
                      </span>
                    </header>
                    <label className="dt-button dt-button--secondary dt-ai-file-button">
                      <Icon name="paperclip" size={16} />
                      {a.memory.upload(a.acceptedFiles)}
                      <input
                        accept={knowledgeFileAccept}
                        disabled={memoryBusy || ocrLanguages.length === 0}
                        onChange={(event) => {
                          void uploadFile(event.target.files?.[0])
                          event.currentTarget.value = ''
                        }}
                        type="file"
                      />
                    </label>
                    <details className="dt-ai-memory-disclosure">
                      <summary>{a.memory.ocrAdvanced}</summary>
                      <fieldset className="dt-ai-language-picker">
                        <legend className="visually-hidden">{a.ocr.title}</legend>
                        {getOcrOptions().map((option) => (
                          <label key={option.value}>
                            <input
                              checked={ocrLanguages.includes(option.value)}
                              onChange={(event) => setOcrLanguages((current) => (
                                event.target.checked
                                  ? [...current, option.value]
                                  : current.filter((language) => language !== option.value)
                              ))}
                              type="checkbox"
                            />
                            {option.label}
                          </label>
                        ))}
                      </fieldset>
                      {ocrLanguages.length === 0 && (
                        <small className="dt-inline-error">{a.ocr.required}</small>
                      )}
                    </details>
                    <details className="dt-ai-memory-disclosure">
                      <summary>{a.memory.addMemory}</summary>
                      <div className="dt-ai-memory-form">
                        <input onChange={(event) => setMemoryName(event.target.value)} placeholder={a.memory.memoryName} value={memoryName} />
                        <textarea onChange={(event) => setMemoryContent(event.target.value)} placeholder={a.memory.memoryContent} rows={4} value={memoryContent} />
                        <button className="dt-button dt-button--secondary" disabled={memoryBusy || !memoryName.trim() || !memoryContent.trim()} onClick={() => { void addMemory() }} type="button">
                          {a.memory.add}
                        </button>
                      </div>
                    </details>
                  </section>
                  {memoryError && <div className="dt-inline-error" role="alert">{memoryError}</div>}
                  <div className="dt-ai-source-list__heading">
                    <strong>{a.memory.added}</strong>
                    <small>{a.memory.itemCount(sources.length)}</small>
                  </div>
                  <div className="dt-ai-source-list">
                    {sources.length === 0 && (
                      <div className="dt-ai-empty-state">
                        <span>{a.memory.empty}</span>
                      </div>
                    )}
                    {sources.map((source) => (
                      <article key={source.id}>
                        <div className="dt-ai-source-title">
                          <span>
                            <strong>{source.name}</strong>
                            <small>
                              {source.source_type === 'memory' ? a.memory.sourceTypes.memory : source.source_type === 'lms' ? a.memory.sourceTypes.lms : a.memory.sourceTypes.file}
                              {' · '}{sourceStatusLabel(source.status)}
                              {source.chunk_count ? ` · ${source.chunk_count} chunks` : ''}
                              {source.index_status
                                ? a.memory.semanticIndex(indexStatusLabel(source.index_status))
                                : ''}
                              {source.ocr_languages?.length
                                ? ` · OCR ${source.ocr_languages.join('+')}`
                                : ''}
                            </small>
                          </span>
                          <div className="dt-ai-action-row">
                            {source.source_type === 'memory' && (
                              <button className="dt-button dt-button--text dt-button--small" disabled={memoryBusy} onClick={() => startEditingMemory(source)} type="button">
                                {a.memory.edit}
                              </button>
                            )}
                            {source.source_type === 'file' && source.status === 'error' && (
                              <button className="dt-button dt-button--text dt-button--small" disabled={memoryBusy} onClick={() => { void retrySource(source) }} type="button">
                                {a.memory.retry}
                              </button>
                            )}
                            <button className="dt-button dt-button--text dt-button--small" disabled={memoryBusy} onClick={() => { void removeSource(source) }} type="button">
                              {a.memory.remove}
                            </button>
                          </div>
                        </div>
                        {editingSourceId === source.id && (
                          <div className="dt-ai-memory-form">
                            <input aria-label={a.memory.memoryName} onChange={(event) => setEditingMemoryName(event.target.value)} value={editingMemoryName} />
                            <textarea aria-label={a.memory.memoryContent} onChange={(event) => setEditingMemoryContent(event.target.value)} rows={5} value={editingMemoryContent} />
                            {!source.content && (
                              <small className="dt-muted">{a.memory.completeMemory}</small>
                            )}
                            <div className="dt-ai-action-row">
                              <button className="dt-button dt-button--secondary dt-button--small" disabled={memoryBusy || !editingMemoryName.trim() || !editingMemoryContent.trim()} onClick={() => { void saveMemoryEdit() }} type="button">
                                {a.memory.save}
                              </button>
                              <button className="dt-button dt-button--text dt-button--small" disabled={memoryBusy} onClick={() => setEditingSourceId('')} type="button">
                                {a.memory.cancel}
                              </button>
                            </div>
                          </div>
                        )}
                        {source.error_message && <small className="dt-inline-error">{source.error_message}</small>}
                        {source.index_error_message && (
                          <small className="dt-inline-error">{source.index_error_message}</small>
                        )}
                      </article>
                    ))}
                  </div>
                </>
              )}
            </>
          )}
        </section>
      )}
    </div>
  )
}
