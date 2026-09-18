import {
  useCallback, useEffect, useRef, useState, type CSSProperties, type DragEvent, type FormEvent,
} from 'react'
import {
  addCourseSlot,
  cancelProjectSkillMap,
  classifySessionsByTimetable,
  createAIProject,
  deleteAIProject,
  deleteCourseSlot,
  deleteKnowledgeSource,
  formatUsageUSD,
  generateProjectSkillMap,
  getProjectSkillMap,
  getStudyCosts,
  getStudyWeeks,
  linkProjectSession,
  listAIProjects,
  listCourseSlots,
  listKnowledgeSources,
  listProjectSessions,
  listStudyStates,
  listTimetable,
  retryKnowledgeSource,
  unlinkProjectSession,
  updateAIProject,
  uploadKnowledgeFile,
  type AIProject,
  type CourseSlot,
  type KnowledgeSource,
  type ProjectSession,
  type SkillMapDocument,
  type SkillMapJob,
  type SkillMapResponse,
  type StudyContinue,
  type StudyCosts,
  type StudySkillState,
  type StudyWeek,
  type StudyWeeks,
  type TimetableClassifyResult,
} from '../api'
import { listSessions, type Session } from '../pro/api/auth'
import { Icon } from '../unified/components/Icon'
import { intlLocale, messages, useMessages } from '../i18n'
import type { HistorySession } from '../unified/components/HistoryPanel'
import { Mascot } from './Mascot'
import { StudyHistory } from './StudyReview'
import { PracticePanel, type PracticeMode } from './PracticePanel'
import { STUDY_BILLING_EVENT } from './StudyApp'
import { useStudySound } from './useStudySound'
import { layoutSkillGraph } from './skillGraph'
import { WeekCalendar } from './WeekCalendar'
import { browserTimezone, hueOf } from './timetable'
import { downloadStudyTranscripts } from './transcriptDownload'
import type { TextDownloadMode } from '../unified/workspace/downloads'

/** Mirrors the server's skill_key normalization (lowercase, collapsed spaces). */
function skillKeyOf(label: string): string {
  return label.toLowerCase().split(/\s+/).filter(Boolean).join(' ')
}

const LEVEL_ORDER = ['learner', 'supervised', 'hazard', 'independent', 'mastered'] as const

/** Hint-free passes needed to leave each level (mirrors the server table). */
const LEVEL_UP_STREAK: Record<string, number> = {
  learner: 2,
  supervised: 3,
  hazard: 3,
  independent: 4,
}

function langTierForLevel(level: string): string {
  const tiers = messages().study.view.languageTiers
  switch (level) {
    case 'hazard':
      return tiers.hazard
    case 'independent':
    case 'mastered':
      return tiers.independent
    default:
      return tiers.beginner
  }
}

interface StudyViewProps {
  /** Loads the session into the transcription workspace. */
  onOpenSession: (session: HistorySession) => void
}

interface PracticeTarget {
  skillLabel: string
  skillIndex: number
  mode: PracticeMode
  openLesson: boolean
}

function formatDate(value: string): string {
  const parsed = Date.parse(value)
  if (!parsed) return ''
  return new Intl.DateTimeFormat(intlLocale(), {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(parsed)
}

function formatDuration(seconds: number): string {
  const format = messages().format
  if (!seconds) return format.lessThanMinute
  const hours = Math.floor(seconds / 3_600)
  const minutes = Math.floor(seconds % 3_600 / 60)
  if (hours > 0) {
    return format.hoursMinutes(hours, minutes)
  }
  return format.minutes(Math.max(1, minutes))
}

function relativeDay(value?: string): string {
  if (!value) return ''
  const parsed = Date.parse(value)
  if (!parsed) return ''
  const days = Math.floor((Date.now() - parsed) / 86_400_000)
  const relative = messages().study.view.relative
  if (days <= 0) return relative.today
  if (days === 1) return relative.yesterday
  return relative.daysAgo(days)
}

function toHistorySession(session: ProjectSession): HistorySession {
  return {
    id: session.id,
    title: session.title,
    createdAt: Date.parse(session.created_at) || Date.now(),
    durationSeconds: session.duration_seconds,
    status: session.status,
    location: 'cloud',
  }
}

function errorMessage(reason: unknown, fallback: string): string {
  const message = reason instanceof Error ? reason.message : String(reason ?? '')
  return message ? messages().study.view.errorDetail(fallback, message) : fallback
}

/** Stable hue per course so its cover colour survives reloads. */
function pad(value: number): string {
  return value < 10 ? `0${value}` : String(value)
}

const MATERIAL_ACCEPT = '.pdf,.pptx,.docx,.xlsx,.txt,.md,.csv,.tsv,.json,.png,.jpg,.jpeg,.webp'

function featureLabel(feature?: string, action?: string): string {
  const labels = messages().study.view.features
  if (feature && feature in labels) return labels[feature as keyof typeof labels]
  if (action && action in labels) return labels[action as keyof typeof labels]
  return labels.other
}

function announceBilling(): void {
  window.dispatchEvent(new Event(STUDY_BILLING_EVENT))
}

function formatBytes(bytes?: number): string {
  if (!bytes) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

/**
 * 学习模式. Courses are the entry. A course opens on 今日行动: one mission
 * card (the next skill and why), one big button, and the route as a strip of
 * nodes. Everything administrative (sessions, materials, costs, regenerating
 * the route) lives on the 课程管理 tab.
 */
export function StudyView({ onOpenSession }: StudyViewProps) {
  const v = useMessages().study.view
  const practiceCopy = useMessages().study.practice
  const levelShort = (level: string) => v.levels[level as keyof typeof v.levels] ?? level
  const levelTitle = (level: string) => v.levelTitles[level as keyof typeof v.levelTitles] ?? level
  const sound = useStudySound()
  const [courses, setCourses] = useState<AIProject[]>([])
  const [coursesLoading, setCoursesLoading] = useState(true)
  const [course, setCourse] = useState<AIProject | null>(null)
  const activeCourseId = useRef<string | null>(null)
  const [tab, setTab] = useState<'home' | 'manage' | 'history'>('home')
  const [sessions, setSessions] = useState<ProjectSession[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [downloadMode, setDownloadMode] = useState<TextDownloadMode>('bilingual')
  const [downloadProgress, setDownloadProgress] = useState<string | null>(null)
  const downloadController = useRef<AbortController | null>(null)
  const [creating, setCreating] = useState(false)
  const [draftName, setDraftName] = useState('')
  const [candidates, setCandidates] = useState<Session[] | null>(null)
  const [materials, setMaterials] = useState<KnowledgeSource[] | null>(null)
  const [uploading, setUploading] = useState(0)
  const [dragOver, setDragOver] = useState(false)
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const [costs, setCosts] = useState<StudyCosts | null>(null)
  const [costItemsShown, setCostItemsShown] = useState(false)
  // null = none stored yet; undefined = still loading.
  const [skillMap, setSkillMap] = useState<SkillMapDocument | null | undefined>(undefined)
  const [mapStale, setMapStale] = useState(false)
  const [serverMaterialsPending, setServerMaterialsPending] = useState(false)
  const [skillMapJob, setSkillMapJob] = useState<SkillMapJob | null>(null)
  const [skillMapBusy, setSkillMapBusy] = useState(false)
  const [expandedSkillId, setExpandedSkillId] = useState('')
  const [skillStates, setSkillStates] = useState<Record<string, StudySkillState>>({})
  const [continueSkill, setContinueSkill] = useState<StudyContinue | null>(null)
  const [practice, setPractice] = useState<PracticeTarget | null>(null)
  // 按周: null while loading or when the course has nothing to group yet.
  const [weeks, setWeeks] = useState<StudyWeeks | null>(null)
  const [selectedWeek, setSelectedWeek] = useState<number | null>(null)
  const [weekStartDraft, setWeekStartDraft] = useState('')
  // 课表: this course's class times, every course's for the calendar.
  const [slots, setSlots] = useState<CourseSlot[] | null>(null)
  const [timetable, setTimetable] = useState<CourseSlot[] | null>(null)
  const [slotDraft, setSlotDraft] = useState({ weekday: 1, start: '', end: '', label: '' })
  const [classifyPreview, setClassifyPreview] = useState<TimetableClassifyResult | null>(null)
  const [classifying, setClassifying] = useState(false)
  const [classifyNotice, setClassifyNotice] = useState('')

  const cancelDownload = () => {
    downloadController.current?.abort()
    downloadController.current = null
    setDownloadProgress(null)
  }

  useEffect(() => () => downloadController.current?.abort(), [])

  const downloadTranscripts = async (week?: StudyWeek) => {
    if (!course || downloadController.current) return
    const controller = new AbortController()
    downloadController.current = controller
    setDownloadProgress(v.download.preparing)
    setError(null)
    try {
      const downloadSessions = week
        ? (await getStudyWeeks(course.id)).weeks.find((item) => item.week === week.week)?.sessions ?? []
        : await listProjectSessions(course.id)
      controller.signal.throwIfAborted()
      await downloadStudyTranscripts({
        title: week ? `${course.name}-${week.label}` : course.name,
        sessions: downloadSessions,
        mode: downloadMode,
        signal: controller.signal,
        onProgress: (completed, total) => setDownloadProgress(v.download.progress(completed, total)),
      })
    } catch (reason) {
      if (!controller.signal.aborted) {
        setError(`${v.download.failed} ${errorMessage(reason, v.download.retry)}`)
      }
    } finally {
      if (downloadController.current === controller) {
        downloadController.current = null
        setDownloadProgress(null)
      }
    }
  }

  const refreshCourses = useCallback(async () => {
    setCoursesLoading(true)
    try {
      const result = await listAIProjects()
      setCourses(result.projects)
    } catch (reason) {
      setError(errorMessage(reason, v.errors.courses))
    } finally {
      setCoursesLoading(false)
    }
  }, [v.errors.courses])

  useEffect(() => { void refreshCourses() }, [refreshCourses])

  const refreshTimetable = useCallback(async () => {
    try {
      setTimetable(await listTimetable())
    } catch (reason) {
      setTimetable((current) => current ?? [])
      setError(errorMessage(reason, v.timetable.errors.load))
    }
  }, [v.timetable.errors.load])

  useEffect(() => { void refreshTimetable() }, [refreshTimetable])

  const refreshSlots = useCallback(async (courseId: string) => {
    try {
      setSlots(await listCourseSlots(courseId))
    } catch (reason) {
      setSlots((current) => current ?? [])
      setError(errorMessage(reason, v.timetable.errors.load))
    }
  }, [v.timetable.errors.load])

  const refreshSessions = useCallback(async (courseId: string) => {
    setSessions(null)
    try {
      setSessions(await listProjectSessions(courseId))
    } catch (reason) {
      setSessions([])
      setError(errorMessage(reason, v.errors.sessions))
    }
  }, [v.errors.sessions])

  const refreshMaterials = useCallback(async (courseId: string) => {
    try {
      setMaterials(await listKnowledgeSources(courseId))
    } catch (reason) {
      setMaterials((current) => current ?? [])
      setError(errorMessage(reason, v.errors.materials))
    }
  }, [v.errors.materials])

  const refreshCosts = useCallback(async (courseId: string) => {
    try {
      setCosts(await getStudyCosts(courseId))
    } catch {
      // The cost card is informational; the page works without it.
    }
  }, [])

  const refreshWeeks = useCallback(async (courseId: string) => {
    try {
      const result = await getStudyWeeks(courseId)
      setWeeks(result)
      setSelectedWeek((current) => (
        current && result.weeks.some((week) => week.week === current)
          ? current
          : result.focus?.week ?? (result.current_week || result.weeks.at(-1)?.week || null)
      ))
    } catch {
      setWeeks(null)
    }
  }, [])

  const applySkillMapResponse = useCallback((
    result: SkillMapResponse,
    keepExistingMap = false,
  ) => {
    setMapStale(result.stale ?? false)
    setServerMaterialsPending(result.materials_pending ?? false)
    if (result.map || !keepExistingMap) {
      setSkillMap(result.map)
    }
    setSkillMapJob((previous) => {
      const next = result.job ?? null
      if (previous && next && previous.id === next.id
        && previous.status !== 'ready' && next.status === 'ready') {
        void refreshCosts(next.project_id)
        void refreshWeeks(next.project_id)
        announceBilling()
      }
      return next
    })
    if (result.job?.status === 'error') {
      setError(result.job.error_message
        ? v.errorDetail(v.errors.mapGenerate, result.job.error_message)
        : v.errors.mapGenerate)
    }
  }, [refreshCosts, refreshWeeks, v])

  const refreshSkillMap = useCallback(async (courseId: string, keepExistingMap = false) => {
    if (!keepExistingMap) setSkillMap(undefined)
    try {
      const result = await getProjectSkillMap(courseId)
      if (activeCourseId.current !== courseId) return
      applySkillMapResponse(result, keepExistingMap)
    } catch (reason) {
      if (activeCourseId.current !== courseId) return
      if (!keepExistingMap) setSkillMap(null)
      setError(errorMessage(reason, v.errors.mapLoad))
    }
  }, [applySkillMapResponse, v.errors.mapLoad])

  const refreshSkillStates = useCallback(async (courseId: string) => {
    try {
      const result = await listStudyStates(courseId)
      const byKey: Record<string, StudySkillState> = {}
      for (const state of result.states) byKey[state.skill_key] = state
      setSkillStates(byKey)
      setContinueSkill(result.continue)
    } catch {
      // The route renders fine unlit; practice results refresh this again.
    }
  }, [])

  const openCourse = (next: AIProject) => {
    cancelDownload()
    activeCourseId.current = next.id
    setMapStale(false)
    setServerMaterialsPending(false)
    setError(null)
    setCandidates(null)
    setExpandedSkillId('')
    setSkillStates({})
    setCourse(next)
    setTab('home')
    setMaterials(null)
    setCosts(null)
    setCostItemsShown(false)
    void refreshCosts(next.id)
    void refreshSessions(next.id)
    void refreshMaterials(next.id)
    void refreshSkillMap(next.id)
    void refreshSkillStates(next.id)
    setWeeks(null)
    setSelectedWeek(null)
    setWeekStartDraft(next.week_start ?? '')
    void refreshWeeks(next.id)
    setSlots(null)
    setSlotDraft({ weekday: 1, start: '', end: '', label: '' })
    void refreshSlots(next.id)
  }

  const closeCourse = () => {
    cancelDownload()
    activeCourseId.current = null
    setCourse(null)
    setSessions(null)
    setCandidates(null)
    setMaterials(null)
    setCosts(null)
    setSkillMap(undefined)
    setSkillMapJob(null)
    setExpandedSkillId('')
    setSkillStates({})
    setContinueSkill(null)
    setPractice(null)
    setWeeks(null)
    setSelectedWeek(null)
    setSlots(null)
    setError(null)
    // Class times may have changed; the calendar on the course list reads them.
    void refreshTimetable()
  }

  const jobRunning = skillMapJob?.status === 'queued' || skillMapJob?.status === 'processing'

  useEffect(() => {
    if (!course || !jobRunning) return
    void refreshSkillMap(course.id, true)
    const timer = window.setInterval(() => {
      void refreshSkillMap(course.id, true)
    }, 2500)
    return () => window.clearInterval(timer)
  }, [course, jobRunning, refreshSkillMap])

  const materialsPending = (materials ?? []).some(
    ({ status }) => status === 'queued' || status === 'processing',
  )
  useEffect(() => {
    if (!course || (!materialsPending && !serverMaterialsPending)) return
    const timer = window.setInterval(() => { void refreshMaterials(course.id) }, 3000)
    return () => window.clearInterval(timer)
  }, [course, materialsPending, serverMaterialsPending, refreshMaterials])

  // Read-only checks also catch extraction completion and external Moodle uploads.
  useEffect(() => {
    if (!course) return
    void refreshSkillMap(course.id, true)
    const timer = window.setInterval(() => { void refreshSkillMap(course.id, true) }, 10000)
    return () => window.clearInterval(timer)
  }, [course, materials, refreshSkillMap])

  const uploadMaterials = async (files: FileList | File[]) => {
    if (!course) return
    const list = Array.from(files)
    if (list.length === 0) return
    setError(null)
    setUploading((count) => count + list.length)
    for (const file of list) {
      try {
        const source = await uploadKnowledgeFile(course.id, file)
        setMaterials((current) => [source, ...(current ?? []).filter(({ id }) => id !== source.id)])
      } catch (reason) {
        setError(errorMessage(reason, v.errors.upload(file.name)))
      } finally {
        setUploading((count) => count - 1)
      }
    }
    void refreshMaterials(course.id)
    void refreshWeeks(course.id)
  }

  const retryMaterial = async (source: KnowledgeSource) => {
    if (!course) return
    try {
      const updated = await retryKnowledgeSource(course.id, source.id)
      setMaterials((current) => (current ?? []).map((item) => (item.id === updated.id ? updated : item)))
    } catch (reason) {
      setError(errorMessage(reason, v.errors.retryExtract))
    }
  }

  const removeMaterial = async (source: KnowledgeSource) => {
    if (!course) return
    if (!window.confirm(v.confirmDeleteMaterial(source.name))) return
    try {
      await deleteKnowledgeSource(course.id, source.id)
      setMaterials((current) => (current ?? []).filter(({ id }) => id !== source.id))
    } catch (reason) {
      setError(errorMessage(reason, v.errors.deleteMaterial))
    }
  }

  const onDropMaterials = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault()
    setDragOver(false)
    void uploadMaterials(event.dataTransfer.files)
  }

  const generateSkillMap = async () => {
    if (!course || skillMapBusy || jobRunning) return
    if (materialsPending || serverMaterialsPending || uploading > 0) { setError(v.materialsProcessing); return }
    if (!window.confirm(v.confirmRegenerate)) return
    setSkillMapBusy(true)
    setError(null)
    sound.submit()
    try {
      applySkillMapResponse(
        await generateProjectSkillMap(course.id, crypto.randomUUID()),
        true,
      )
      setExpandedSkillId('')
    } catch (reason) {
      setError(errorMessage(reason, v.errors.mapGenerate))
    } finally {
      setSkillMapBusy(false)
    }
  }

  const cancelSkillMap = async () => {
    if (!course) return
    try {
      applySkillMapResponse(await cancelProjectSkillMap(course.id), true)
    } catch (reason) {
      setError(errorMessage(reason, v.errors.cancelMap))
    }
  }

  const perform = useCallback(async (fallback: string, operation: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    try {
      await operation()
    } catch (reason) {
      setError(errorMessage(reason, fallback))
    } finally {
      setBusy(false)
    }
  }, [])

  const submitCreate = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const name = draftName.trim()
    if (!name || busy) return
    await perform(v.errors.createCourse, async () => {
      const created = await createAIProject(name)
      setDraftName('')
      setCreating(false)
      await refreshCourses()
      openCourse(created)
    })
  }

  const renameCourse = async () => {
    if (!course) return
    const next = window.prompt(v.courseNamePrompt, course.name)?.trim()
    if (!next || next === course.name) return
    await perform(v.errors.renameCourse, async () => {
      const updated = await updateAIProject(course.id, { name: next })
      setCourse(updated)
      await refreshCourses()
    })
  }

  const removeCourse = async () => {
    if (!course) return
    if (!window.confirm(
      v.confirmDeleteCourse(course.name || v.untitledCourse),
    )) return
    await perform(v.errors.deleteCourse, async () => {
      await deleteAIProject(course.id)
      closeCourse()
      await refreshCourses()
    })
  }

  const openPicker = async () => {
    if (!course) return
    await perform(v.errors.cloudSessions, async () => {
      const result = await listSessions(1, 100)
      setCandidates(result.sessions.filter(
        (session) => session.project_id !== course.id,
      ))
    })
  }

  const addSession = async (candidate: Session) => {
    if (!course) return
    await perform(v.errors.addSession, async () => {
      await linkProjectSession(course.id, candidate.id)
      setCandidates((current) => current?.filter(({ id }) => id !== candidate.id) ?? null)
      await refreshSessions(course.id)
      void refreshWeeks(course.id)
    })
  }

  const removeSession = async (session: ProjectSession) => {
    if (!course) return
    await perform(v.errors.removeSession, async () => {
      await unlinkProjectSession(course.id, session.id)
      await refreshSessions(course.id)
      void refreshWeeks(course.id)
    })
  }

  const saveWeekStart = async () => {
    if (!course) return
    const value = weekStartDraft.trim()
    await perform(v.errors.saveWeek, async () => {
      const updated = await updateAIProject(course.id, { week_start: value })
      setCourse(updated)
      await refreshWeeks(course.id)
    })
  }

  const addSlot = async () => {
    if (!course || !slotDraft.start || !slotDraft.end) return
    await perform(v.timetable.errors.add, async () => {
      const slot = await addCourseSlot(course.id, {
        weekday: slotDraft.weekday,
        start: slotDraft.start,
        end: slotDraft.end,
        timezone: browserTimezone(),
        label: slotDraft.label.trim(),
      })
      setSlots((current) => [...(current ?? []), slot]
        .sort((left, right) => left.weekday - right.weekday || left.start.localeCompare(right.start)))
      setSlotDraft((draft) => ({ ...draft, start: '', end: '', label: '' }))
    })
  }

  const removeSlot = async (slot: CourseSlot) => {
    if (!course) return
    await perform(v.timetable.errors.remove, async () => {
      await deleteCourseSlot(course.id, slot.id)
      setSlots((current) => current?.filter(({ id }) => id !== slot.id) ?? null)
    })
  }

  // 自动归类: preview first, apply on confirmation. Both are free of AI.
  const runClassify = async (apply: boolean) => {
    setClassifying(true)
    setClassifyNotice('')
    setError(null)
    try {
      const result = await classifySessionsByTimetable(apply)
      if (apply) {
        setClassifyPreview(null)
        setClassifyNotice(v.timetable.applied(result.applied))
      } else if (result.assignments.length === 0) {
        setClassifyPreview(null)
        setClassifyNotice(`${v.timetable.nothingToDo} ${v.timetable.previewSummary(result.scanned, result.kept, result.unmatched)}`)
      } else {
        setClassifyPreview(result)
      }
    } catch (reason) {
      setError(errorMessage(reason, v.timetable.errors.classify))
    } finally {
      setClassifying(false)
    }
  }

  const startPractice = (skillLabel: string, mode: PracticeMode, openLesson?: boolean) => {
    if (!skillMap) return
    if (mapStale || materialsPending || serverMaterialsPending || uploading > 0) {
      setError(v.materialsChanged)
      return
    }
    const skillIndex = Math.max(0, skillMap.skills.findIndex(
      (skill) => skillKeyOf(skill.label) === skillKeyOf(skillLabel),
    ))
    const state = skillStates[skillKeyOf(skillLabel)]
    sound.submit()
    setPractice({
      skillLabel,
      skillIndex,
      mode,
      // A skill never practiced opens on its lesson; afterwards it is one tap away.
      openLesson: openLesson ?? !state,
    })
  }

  const courseNameById = new Map(courses.map(({ id, name }) => [id, name]))

  // Progress across the current route (only skills that are on it count).
  const mapStates = (skillMap?.skills ?? [])
    .map((skill) => skillStates[skillKeyOf(skill.label)])
    .filter((state): state is StudySkillState => Boolean(state))
  const masteredCount = mapStates.filter(({ level }) => level === 'mastered').length
  const xpTotal = mapStates.reduce((sum, { xp_total }) => sum + xp_total, 0)
  const sessionCount = sessions?.length ?? 0
  const readyMaterials = (materials ?? []).filter(({ status }) => status === 'ready').length
  const hasInput = sessionCount > 0 || readyMaterials > 0
  const generating = jobRunning || skillMapBusy
  // 按周 focus (an owed week) outranks the route order for today's mission.
  const focusSkill = weeks?.focus?.skill_label && skillMap?.skills.some(
    (skill) => skillKeyOf(skill.label) === skillKeyOf(weeks.focus!.skill_label!),
  )
    ? { skill_label: weeks.focus!.skill_label!, level: 'learner' as const, reason: weeks.focus!.reason }
    : null
  const missionSkill = focusSkill ?? continueSkill
  const continueState = missionSkill ? skillStates[skillKeyOf(missionSkill.skill_label)] : undefined
  const continueLevel = continueState?.level ?? missionSkill?.level ?? 'learner'
  const continueIndex = missionSkill && skillMap
    ? skillMap.skills.findIndex((skill) => skillKeyOf(skill.label) === skillKeyOf(missionSkill.skill_label))
    : -1
  const selected = weeks?.weeks.find((week) => week.week === selectedWeek) ?? null
  const passNeeded = LEVEL_UP_STREAK[continueLevel] ?? 0
  const passHave = Math.min(continueState?.clean_streak ?? 0, passNeeded)
  const recent = mapStates
    .filter((state) => state.last_grade && ['C', 'D', 'HD'].includes(state.last_grade))
    .sort((a, b) => Date.parse(b.updated_at ?? '') - Date.parse(a.updated_at ?? ''))
    .slice(0, 4)

  const renderProgress = () => (
    <div className="dt-study__progress">
      <span className="dt-study__progress-head">
        {skillMapJob && skillMapJob.chunk_count > 0
          ? v.jobReading(skillMapJob.processed_chunks, skillMapJob.chunk_count)
          : v.jobQueued}
        {jobRunning && (
          <button
            className="dt-study__progress-cancel"
            onClick={() => { void cancelSkillMap() }}
            title={v.cancelTitle}
            type="button"
          >
            {v.cancel}
          </button>
        )}
      </span>
      <div className="dt-study__progress-bar">
        <i
          className={skillMapJob && skillMapJob.chunk_count > 0 ? '' : 'is-indeterminate'}
          style={skillMapJob && skillMapJob.chunk_count > 0
            ? { '--pct': `${Math.max(4, Math.round(100 * skillMapJob.processed_chunks / skillMapJob.chunk_count))}%` } as CSSProperties
            : undefined}
        />
      </div>
    </div>
  )

  const routeGraph = skillMap ? layoutSkillGraph(skillMap.skills) : null
  const renderRoute = () => skillMap && routeGraph && (
    <div className="dt-route st-panel">
      <div className="dt-study__section-heading">
        <span className="st-label">
          <Icon name="map" size={14} />
          {v.routeTitle}
        </span>
        <span className="st-label st-label--mu">
          {v.routeStats(masteredCount, skillMap.skills.length, xpTotal.toLocaleString(intlLocale()))}
        </span>
      </div>
      <div className="dt-route__strip">
        <div className="dt-route__graph" style={{ width: routeGraph.width, height: routeGraph.height }}>
          <svg className="dt-route__edges" width={routeGraph.width} height={routeGraph.height} aria-hidden="true">
            {routeGraph.edges.map(({ from, to, start, end }) => (
              <g key={`${from}-${to}`} data-from={from} data-to={to}>
                <path d={`M ${start.x + 130} ${start.y + 32} C ${start.x + 170} ${start.y + 32}, ${end.x - 40} ${end.y + 32}, ${end.x - 6} ${end.y + 32}`} />
                <path d={`M ${end.x - 12} ${end.y + 28} L ${end.x - 6} ${end.y + 32} L ${end.x - 12} ${end.y + 36}`} />
              </g>
            ))}
          </svg>
        {routeGraph.nodes.map(({ skill, index, x, y }) => {
          const state = skillStates[skillKeyOf(skill.label)]
          const level = state?.level ?? 'unlit'
          const isCurrent = index === continueIndex
          const parentLabels = routeGraph.edges.filter((edge) => edge.to === skill.id).map((edge) => edge.start.skill.label)
          return (
            <div className="dt-route__cell" key={skill.id} style={{ left: x, top: y }}>
              <button
                className={`dt-route__node is-${level}${isCurrent ? ' is-cur' : ''}`}
                onClick={() => startPractice(skill.label, 'graded')}
                title={state ? `${levelTitle(state.level)} · ${state.xp_total} XP` : v.unpractised}
                type="button"
              >
                <span className="dt-route__hex">
                  <span>{level === 'mastered' ? <Icon name="check" size={16} /> : pad(index + 1)}</span>
                </span>
                <small>{skill.label}</small>
                <em>{isCurrent ? v.nextStop : state ? levelShort(state.level) : v.notStarted}</em>
                {parentLabels.length > 0 && <span className="dt-route__parents" title={parentLabels.join(' · ')}>← {parentLabels.join(' · ')}</span>}
              </button>
            </div>
          )
        })}
        </div>
      </div>
      <div className="dt-route__legend">
        {LEVEL_ORDER.map((level) => (
          <span key={level}><i className={`is-${level}`} />{levelShort(level)}</span>
        ))}
        <span><i className="is-unlit" />{v.notStarted}</span>
      </div>
    </div>
  )

  const weekStatusLabel = (week: StudyWeek): string => {
    switch (week.status) {
      case 'done': return v.weekStates.done
      case 'current': return v.weekStates.current
      case 'behind': return v.weekStates.behind
      case 'upcoming': return v.weekStates.upcoming
      default: return v.weekStates.empty
    }
  }

  const renderWeeks = () => {
    if (!weeks || weeks.weeks.length === 0) return null
    return (
      <div className="dt-weeks st-panel">
        <div className="dt-study__section-heading">
          <span className="st-label">
            <Icon name="history" size={14} />
            {v.courseProgress} // {weeks.current_week ? `WEEK ${pad(weeks.current_week)}` : 'BY WEEK'}
          </span>
          <span className="st-label st-label--mu">
            {weeks.week_start
              ? v.weekStart(weeks.week_start, weeks.week_start_inferred)
              : v.groupedByName}
          </span>
        </div>
        {weeks.behind_weeks.length > 0 && weeks.focus && (
          <p className="dt-weeks__note">
            {v.weeksBehind(weeks.behind_weeks.join(', '), weeks.focus.reason)}
            {weeks.focus.skill_label && (
              <button
                className="st-btn st-btn--quiet"
                onClick={() => startPractice(weeks.focus!.skill_label!, 'graded')}
                type="button"
              >
                {v.startWeek(weeks.focus.week)}
              </button>
            )}
          </p>
        )}
        <div className="dt-weeks__strip" role="tablist" aria-label={v.teachingWeeks}>
          {weeks.weeks.map((week) => (
            <button
              aria-selected={week.week === selectedWeek}
              className={`dt-weeks__chip is-${week.status}${week.week === selectedWeek ? ' is-selected' : ''}`}
              key={week.week}
              onClick={() => setSelectedWeek(week.week)}
              role="tab"
              title={`${week.label} · ${weekStatusLabel(week)}${week.start ? ` · ${week.start}` : ''}`}
              type="button"
            >
              <b>{pad(week.week)}</b>
              <small>{weekStatusLabel(week)}</small>
            </button>
          ))}
        </div>
        {selected && (
          <div className="dt-weeks__detail">
            <div className="dt-weeks__detail-head">
              <strong>{selected.label}</strong>
              <span className="st-label st-label--mu">
                {selected.start ? `${selected.start} → ${selected.end}` : ''}
                {selected.sessions.length + selected.sources.length + selected.skills.length === 0 && v.noWeekMaterials}
              </span>
              <button
                className="st-btn"
                disabled={downloadProgress !== null || selected.sessions.length === 0}
                onClick={() => { void downloadTranscripts(selected) }}
                title={v.download.hint}
                type="button"
              >
                <Icon name="download" size={14} />
                {v.download.week}
              </button>
            </div>
            {selected.skills.length > 0 && (
              <div className="dt-weeks__skills">
                {selected.skills.map((skill) => (
                  <button
                    className={`dt-weeks__skill is-${skill.level}`}
                    key={skill.id}
                    onClick={() => startPractice(skill.label, 'graded')}
                    title={skill.level === 'unlit' ? v.unpractised : levelTitle(skill.level)}
                    type="button"
                  >
                    <i />
                    {skill.label}
                    <small>{skill.level === 'unlit' ? v.notStarted : levelShort(skill.level)}</small>
                  </button>
                ))}
              </div>
            )}
            {(selected.sessions.length > 0 || selected.sources.length > 0) && (
              <ul className="dt-weeks__materials">
                {selected.sessions.map((session) => (
                  <li key={session.id}>
                    <button
                      onClick={() => onOpenSession({
                        id: session.id, title: session.title,
                        createdAt: Date.parse(session.started_at) || Date.now(),
                        durationSeconds: 0, status: 'completed', location: 'cloud',
                      })}
                      title={v.openWorkspace}
                      type="button"
                    >
                      <Icon name="history" size={13} />
                      {session.title || v.untitledSession}
                    </button>
                  </li>
                ))}
                {selected.sources.map((source) => (
                  <li key={source.id}>
                    <span>
                      <Icon name={source.source_type === 'lms' ? 'cloud' : 'paperclip'} size={13} />
                      {source.name}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>
    )
  }

  const renderHome = () => (
    <div className="dt-study__home">
      <section className="dt-study__main">
        {sessions !== null && !hasInput && (
          <div className="dt-study__mission st-panel is-empty">
            <span className="st-label st-label--or">{v.setup}</span>
            <h3>{v.emptyCourse}</h3>
            <p className="dt-study__mission-why">
              {v.emptyCourseBody}
            </p>
            <div className="dt-study__mission-go">
              <button className="st-btn st-btn--primary" onClick={() => setTab('manage')} type="button">
                {v.addInputs}
              </button>
            </div>
          </div>
        )}

        {hasInput && skillMap === null && (
          <div className="dt-study__mission st-panel is-empty">
            <span className="st-label st-label--or">{v.routeStep}</span>
            <h3>{v.generateRoute}</h3>
            <p className="dt-study__mission-why">
              {v.generateBody(sessionCount, readyMaterials)}
            </p>
            {generating ? renderProgress() : (
              <div className="dt-study__mission-go">
                <button className="st-btn st-btn--primary" onClick={() => { void generateSkillMap() }} type="button">
                  <Icon name="sparkles" size={14} />
                  {v.generateRoute}
                </button>
              </div>
            )}
          </div>
        )}

        {skillMap === undefined && <p className="dt-study__empty">{v.loadingRoute}</p>}

        {skillMap && missionSkill && (
          <div className="dt-study__mission st-panel">
            <div className="dt-study__mission-grid" aria-hidden="true" />
            <span className="dt-study__mission-wm" aria-hidden="true">{pad(continueIndex + 1)}</span>
            <span className="st-label st-label--or">
              {v.mission} // {weeks?.focus ? `WEEK ${pad(weeks.focus.week)}` : 'RECOMMENDED'}
            </span>
            <div className="dt-study__mission-code">
              <b>OP-{pad(continueIndex + 1)}</b>
              <span className="st-label st-label--mu">
                {levelShort(continueLevel)} · {langTierForLevel(continueLevel)}
              </span>
            </div>
            <h3>{missionSkill.skill_label}</h3>
            <p className="dt-study__mission-why">{missionSkill.reason ?? v.routeReason}</p>
            <div className="dt-study__mission-chips">
              {passNeeded > 0 && (
                <span className="st-chip">
                  {v.pass}
                  <span className="dt-study__pass">
                    {Array.from({ length: passNeeded }, (_, index) => (
                      <i className={index < passHave ? 'is-on' : ''} key={index} />
                    ))}
                  </span>
                  {passNeeded - passHave <= 1 ? v.oneToLevel : v.toLevel(passNeeded - passHave)}
                </span>
              )}
              <span className="st-chip st-chip--cy">{v.gradingRule}</span>
              {continueState?.last_error_pattern && (
                <span className="st-chip st-chip--or">{v.lastStuck(continueState.last_error_pattern)}</span>
              )}
            </div>
            <div className="dt-study__mission-go">
              <button
                className="st-btn st-btn--orange st-btn--big"
                onClick={() => startPractice(missionSkill.skill_label, 'graded')}
                type="button"
              >
                <Icon name="play" size={14} />
                {v.startAction}
              </button>
              <button
                className="st-btn"
                onClick={() => startPractice(missionSkill.skill_label, 'graded', true)}
                type="button"
              >
                {v.lessonFirst}
              </button>
              <button
                className="st-btn st-btn--quiet"
                onClick={() => startPractice(missionSkill.skill_label, 'free', false)}
                title={v.freeTitle}
                type="button"
              >
                {v.free}
              </button>
              <small className="st-label st-label--mu">{v.xpRules}</small>
            </div>
            <span className="dt-study__mission-bar" aria-hidden="true" />
          </div>
        )}

        {skillMap && !missionSkill && (
          <div className="dt-study__mission st-panel is-empty">
            <span className="st-label st-label--or">ALL CLEAR</span>
            <h3>{v.allMastered}</h3>
            <p className="dt-study__mission-why">{v.allMasteredBody}</p>
          </div>
        )}

        {renderWeeks()}
        {renderRoute()}
      </section>

      <aside className="dt-study__side">
        <div className="st-panel dt-study__tutor">
          <span className="st-label">{v.tutorLabel}</span>
          <div className="dt-study__tutor-row">
            <Mascot mood={continueSkill ? 'happy' : skillMap ? 'proud' : 'idle'} size={56} />
            <p>
              {!hasInput && sessions !== null
                ? v.tutorEmpty
                : !skillMap
                  ? v.tutorReady
                  : continueSkill
                    ? recent.length > 0
                      ? v.tutorReturn
                      : v.tutorFirst
                    : v.tutorMastered}
            </p>
          </div>
        </div>

        {skillMap && (
          <div className="st-panel dt-study__recent">
            <span className="st-label">{v.cleared}</span>
            {recent.length === 0 ? (
              <p className="dt-study__empty">{v.noCleared}</p>
            ) : (
              <ul>
                {recent.map((state) => (
                  <li key={state.skill_key}>
                    <span>{state.skill_label}</span>
                    <b>{levelShort(state.level)} · {relativeDay(state.updated_at)}</b>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}

        <div className="st-panel dt-study__promise">
          <span className="st-label st-label--mu">{v.rules}</span>
          <ul>
            {v.ruleItems.map((item) => <li key={item}>{item}</li>)}
          </ul>
        </div>
      </aside>
    </div>
  )

  const renderManage = () => (
    <div className="dt-study__manage">
      <div className="st-panel dt-study__map">
        <div className="dt-study__section-heading">
          <span className="st-label">
            <Icon name="map" size={14} />
            {v.mapTitle}
            {skillMap && <small>// {pad(skillMap.skills.length)} NODES</small>}
          </span>
          <button
            className="st-btn"
            disabled={generating || !hasInput || materialsPending || serverMaterialsPending || uploading > 0}
            onClick={() => { void generateSkillMap() }}
            title={!hasInput
              ? v.mapNeedInput
              : v.mapCostHint}
            type="button"
          >
            {generating ? v.generating : skillMap ? v.regenerate : v.generateRoute}
          </button>
        </div>
        {generating && renderProgress()}
        {skillMap === null && !generating && (
          <p className="dt-study__empty">{v.noMap}</p>
        )}
        {skillMap && (
          <>
            <div className="dt-study__skills">
              {skillMap.skills.map((skill, index) => {
                const expanded = expandedSkillId === skill.id
                const state = skillStates[skillKeyOf(skill.label)]
                const prerequisiteLabels = (skill.prerequisites ?? [])
                  .map((id) => skillMap.skills.find((item) => item.id === id)?.label)
                  .filter((label): label is string => Boolean(label))
                return (
                  <div className={`dt-study__skill${expanded ? ' is-expanded' : ''}`} key={skill.id}>
                    <button
                      className="dt-study__skill-head"
                      onClick={() => setExpandedSkillId(expanded ? '' : skill.id)}
                      type="button"
                    >
                      <span className={`dt-study__skill-state is-${state?.level ?? 'unlit'}`}>{pad(index + 1)}</span>
                      <span className="dt-study__skill-label">
                        {skill.label}
                        {skill.new && <em className="dt-study__skill-new">NEW</em>}
                      </span>
                      {skill.outcome && <small>{skill.outcome}</small>}
                    </button>
                    <button
                      className="st-btn st-btn--quiet dt-study__practice"
                      onClick={() => startPractice(skill.label, 'graded')}
                      type="button"
                    >
                      <Icon name="play" size={12} />
                      {v.practise}
                    </button>
                    {expanded && (
                      <div className="dt-study__skill-detail">
                        {skill.summary && <p>{skill.summary}</p>}
                        {prerequisiteLabels.length > 0 && (
                          <p className="dt-study__skill-prereq">REQUIRES // {prerequisiteLabels.join(' · ')}</p>
                        )}
                        {(skill.evidence ?? []).map((evidence, evidenceIndex) => {
                          if (evidence.source_id) {
                            return (
                              <div className="dt-study__skill-evidence is-source" key={evidenceIndex} title={v.fromMaterial}>
                                <span>“{evidence.quote}”</span>
                                <small>{v.materialLabel(evidence.source_title || v.courseMaterial)}</small>
                              </div>
                            )
                          }
                          const evidenceSession = sessions?.find(({ id }) => id === evidence.session_id)
                          return (
                            <button
                              className="dt-study__skill-evidence"
                              disabled={!evidenceSession}
                              key={evidenceIndex}
                              onClick={() => { if (evidenceSession) onOpenSession(toHistorySession(evidenceSession)) }}
                              title={evidenceSession ? v.evidenceSessionOpen : v.evidenceSessionMissing}
                              type="button"
                            >
                              <span>“{evidence.quote}”</span>
                              <small>{evidence.session_title || v.courseSession}</small>
                            </button>
                          )
                        })}
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
            <p className="dt-study__skill-meta">
              {skillMapJob?.status === 'ready' && (skillMapJob.cost_usd ?? 0) > 0 && (
                <>{v.generationCost(formatUsageUSD(skillMapJob.cost_usd ?? 0))}</>
              )}
              {v.basedOn(skillMap.session_count, skillMap.source_count ?? 0)}
              {skillMap.truncated && v.legacyTruncated}
              {skillMap.generated_at && ` · ${formatDate(skillMap.generated_at)}`}
            </p>
          </>
        )}
      </div>

      <div className="dt-study__manage-side">
        <div className="st-panel dt-study__sessions-panel">
          <div className="dt-study__section-heading">
            <span className="st-label">
              <Icon name="history" size={14} />
              {v.sessionsTitle}
              {sessions && <small>// {pad(sessions.length)}</small>}
            </span>
            <button
              className="st-btn"
              disabled={busy}
              onClick={() => { if (candidates) setCandidates(null); else void openPicker() }}
              type="button"
            >
              {candidates ? v.collapse : v.addSession}
            </button>
          </div>
          {candidates && (
            <div className="dt-study__picker">
              {candidates.length === 0 && (
                <p className="dt-study__empty">{v.noCandidates}</p>
              )}
              {candidates.map((candidate) => (
                <div className="dt-study__row" key={candidate.id}>
                  <button
                    className="dt-study__row-main"
                    disabled={busy}
                    onClick={() => { void addSession(candidate) }}
                    type="button"
                  >
                    <Icon name="plus" size={14} />
                    <span className="dt-study__row-title">{candidate.title || v.untitledSession}</span>
                    <small>
                      {formatDate(candidate.created_at)}
                      {candidate.project_id && v.belongsTo(courseNameById.get(candidate.project_id) ?? v.otherCourse)}
                    </small>
                  </button>
                </div>
              ))}
            </div>
          )}
          {sessions === null && <p className="dt-study__empty">{v.loadingSessions}</p>}
          {sessions?.length === 0 && !candidates && (
            <p className="dt-study__empty">{v.noSessions}</p>
          )}
          <div className="dt-study__sessions">
            {sessions?.map((session) => (
              <div className="dt-study__row" key={session.id}>
                <button
                  className="dt-study__row-main"
                  onClick={() => onOpenSession(toHistorySession(session))}
                  title={v.openWorkspace}
                  type="button"
                >
                  <Icon name="history" size={14} />
                  <span className="dt-study__row-title">
                    {session.title || v.untitledSession}
                    {session.assigned_by === 'timetable' && (
                      <em className="dt-study__row-badge" title={v.timetable.byTimetableTitle}>{v.timetable.byTimetable}</em>
                    )}
                  </span>
                  <small>{formatDate(session.started_at)} · {formatDuration(session.duration_seconds)}</small>
                </button>
                <button
                  aria-label={v.removeSessionAria(session.title || v.untitledSession)}
                  className="st-iconbtn"
                  disabled={busy}
                  onClick={() => { void removeSession(session) }}
                  title={v.removeSessionTitle}
                  type="button"
                >
                  <Icon name="close" size={14} />
                </button>
              </div>
            ))}
          </div>
        </div>

        <div
          className={`st-panel dt-study__materials${dragOver ? ' is-dragover' : ''}`}
          onDragLeave={() => setDragOver(false)}
          onDragOver={(event) => { event.preventDefault(); setDragOver(true) }}
          onDrop={onDropMaterials}
        >
          <div className="dt-study__section-heading">
            <span className="st-label">
              <Icon name="paperclip" size={14} />
              {v.materialsTitle}
              {materials && <small>// {pad(materials.length)}</small>}
            </span>
            <button
              className="st-btn"
              disabled={uploading > 0}
              onClick={() => fileInputRef.current?.click()}
              type="button"
            >
              {uploading > 0 ? v.uploading(uploading) : v.uploadMaterials}
            </button>
            <input
              accept={MATERIAL_ACCEPT}
              aria-label={v.uploadAria}
              hidden
              multiple
              onChange={(event) => {
                if (event.target.files) void uploadMaterials(event.target.files)
                event.target.value = ''
              }}
              ref={fileInputRef}
              type="file"
            />
          </div>
          <p className="dt-study__materials-hint">
            {v.materialsHelp}
          </p>
          {materials === null && <p className="dt-study__empty">{v.loadingMaterials}</p>}
          <div className="dt-study__sources">
            {materials?.map((source) => (
              <div className={`dt-study__source is-${source.status}`} key={source.id}>
                <span className="dt-study__source-main">
                  <span className="dt-study__source-name">{source.name}</span>
                  <small>
                    {source.source_type === 'lms' && <i className="dt-study__source-lms">MOODLE · </i>}
                    <i className="dt-study__source-status">{v.sourceStatuses[source.status]}</i>
                    {source.size_bytes ? ` · ${formatBytes(source.size_bytes)}` : ''}
                    {source.status === 'ready' && source.chunk_count ? v.chunks(source.chunk_count) : ''}
                    {source.status === 'error' && source.error_message ? ` · ${source.error_message}` : ''}
                  </small>
                </span>
                {source.status === 'error' && (
                  <button
                    aria-label={v.retryExtractAria(source.name)}
                    className="st-iconbtn"
                    onClick={() => { void retryMaterial(source) }}
                    title={v.retryExtract}
                    type="button"
                  >
                    <Icon name="wave" size={14} />
                  </button>
                )}
                <button
                  aria-label={v.deleteMaterialAria(source.name)}
                  className="st-iconbtn st-iconbtn--danger"
                  onClick={() => { void removeMaterial(source) }}
                  title={v.deleteMaterial}
                  type="button"
                >
                  <Icon name="close" size={14} />
                </button>
              </div>
            ))}
          </div>
        </div>

        <div className="st-panel dt-study__costs">
          <div className="dt-study__section-heading">
            <span className="st-label">
              <Icon name="shield" size={14} />
              {v.costsTitle}
              <small>// USD</small>
            </span>
            {costs && costs.items.length > 0 && (
              <button className="st-btn st-btn--quiet" onClick={() => setCostItemsShown((value) => !value)} type="button">
                {costItemsShown ? v.collapseDetails : v.details}
              </button>
            )}
          </div>
          {costs === null && <p className="dt-study__empty">{v.counting}</p>}
          {costs && !costs.billing_enabled && (
            <p className="dt-study__empty">{v.billingOff}</p>
          )}
          {costs?.billing_enabled && (
            <>
              <div className="dt-study__cost-total">
                <b>{formatUsageUSD(costs.summary.total_usd)}</b>
                <span>{v.courseCalls(costs.summary.operations)}</span>
              </div>
              <div className="dt-study__cost-split">
                {(['skill_map', 'study_lesson', 'study_bank', 'study_grade'] as const).map((feature) => (
                  <span key={feature}>
                    <small>{v.features[feature]}</small>
                    <b>{formatUsageUSD(costs.summary.by_feature[feature] ?? 0)}</b>
                  </span>
                ))}
              </div>
              <p className="dt-study__cost-hint">
                {v.costsHelp}
              </p>
              {costItemsShown && (
                <ul className="dt-study__cost-items">
                  {costs.items.map((item) => (
                    <li key={item.id}>
                      <span>{featureLabel(item.feature, item.action)}</span>
                      <small>{item.model || '—'} · {formatDate(item.created_at)}</small>
                      <b>{item.refunded ? v.refunded : formatUsageUSD(item.cost_usd)}</b>
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}
        </div>

        <div className="st-panel dt-study__settings">
          <div className="dt-study__section-heading">
            <span className="st-label st-label--mu"><Icon name="settings" size={14} />{v.settings}</span>
          </div>
          <label className="dt-study__week-start">
            <span>{v.weekMonday}</span>
            <span className="dt-study__week-start-row">
              <input
                onChange={(event) => setWeekStartDraft(event.target.value)}
                placeholder={weeks?.week_start ? v.inferredWeek(weeks.week_start) : v.weekExample}
                type="date"
                value={weekStartDraft}
              />
              <button className="st-btn" disabled={busy} onClick={() => { void saveWeekStart() }} type="button">
                {v.save}
              </button>
            </span>
            <small>{v.weekHelp}</small>
          </label>
          <section aria-labelledby="dt-study-timetable-title" className="dt-study__timetable">
            <span className="st-label st-label--mu" id="dt-study-timetable-title">
              <Icon name="history" size={14} />{v.timetable.title}
              <small>// {v.timetable.code}</small>
            </span>
            <p className="dt-study__timetable-help">{v.timetable.help}</p>
            {slots === null && <p className="dt-study__empty">{v.timetable.loading}</p>}
            {slots?.length === 0 && <p className="dt-study__empty">{v.timetable.noSlots}</p>}
            {slots && slots.length > 0 && (
              <ul className="dt-study__slots">
                {slots.map((slot) => {
                  const text = `${v.timetable.weekdays[slot.weekday - 1]} ${slot.start}–${slot.end}`
                  return (
                    <li key={slot.id} style={{ '--hue': hueOf(slot.project_id) } as CSSProperties}>
                      <span>
                        <b>{text}</b>
                        {slot.label && <small>{slot.label}</small>}
                        {slot.timezone !== browserTimezone() && <small>{v.timetable.timezone(slot.timezone)}</small>}
                      </span>
                      <button
                        aria-label={v.timetable.removeAria(text)}
                        className="st-iconbtn"
                        disabled={busy}
                        onClick={() => { void removeSlot(slot) }}
                        title={v.timetable.remove}
                        type="button"
                      >
                        <Icon name="close" size={14} />
                      </button>
                    </li>
                  )
                })}
              </ul>
            )}
            <form
              className="dt-study__slot-form"
              onSubmit={(event) => { event.preventDefault(); void addSlot() }}
            >
              <label>
                <span>{v.timetable.weekday}</span>
                <select
                  onChange={(event) => setSlotDraft((draft) => ({ ...draft, weekday: Number(event.target.value) }))}
                  value={slotDraft.weekday}
                >
                  {v.timetable.weekdays.map((weekday, index) => (
                    <option key={weekday} value={index + 1}>{weekday}</option>
                  ))}
                </select>
              </label>
              <label>
                <span>{v.timetable.start}</span>
                <input
                  onChange={(event) => setSlotDraft((draft) => ({ ...draft, start: event.target.value }))}
                  required
                  type="time"
                  value={slotDraft.start}
                />
              </label>
              <label>
                <span>{v.timetable.end}</span>
                <input
                  min={slotDraft.start || undefined}
                  onChange={(event) => setSlotDraft((draft) => ({ ...draft, end: event.target.value }))}
                  required
                  type="time"
                  value={slotDraft.end}
                />
              </label>
              <label className="dt-study__slot-label">
                <span>{v.timetable.label}</span>
                <input
                  maxLength={60}
                  onChange={(event) => setSlotDraft((draft) => ({ ...draft, label: event.target.value }))}
                  placeholder={v.timetable.labelPlaceholder}
                  value={slotDraft.label}
                />
              </label>
              <button
                className="st-btn"
                disabled={busy || !slotDraft.start || !slotDraft.end || slotDraft.end <= slotDraft.start}
                type="submit"
              >
                {v.timetable.add}
              </button>
            </form>
          </section>
          <div className="dt-study__settings-actions">
            <button className="st-btn st-btn--quiet" disabled={busy} onClick={() => { void renameCourse() }} type="button">
              {v.rename}
            </button>
            <button className="st-btn st-btn--quiet is-danger" disabled={busy} onClick={() => { void removeCourse() }} type="button">
              {v.deleteCourse}
            </button>
          </div>
        </div>
      </div>
    </div>
  )

  return (
    <div className="dt-study">
      {error && (
        <p className="dt-study__error" role="alert">
          {error}
          <button aria-label={v.closeError} onClick={() => setError(null)} type="button">
            <Icon name="close" size={12} />
          </button>
        </p>
      )}

      {!course && (
        <>
          <header className="dt-study__hero">
            <span className="st-label st-label--or">COURSES // {pad(courses.length)}</span>
            <h2>{v.chooseCourse}</h2>
            <p className="dt-study__lead">
              {v.chooseBody}
            </p>
          </header>

          {coursesLoading && courses.length === 0 && (
            <p className="dt-study__empty">{v.loadingCourses}</p>
          )}

          {timetable && (courses.length > 0 || timetable.length > 0) && (
            <section aria-labelledby="dt-study-calendar-title" className="st-panel dt-study__calendar">
              <div className="dt-study__section-heading">
                <span className="st-label" id="dt-study-calendar-title">
                  <Icon name="history" size={14} />
                  {v.timetable.calendarTitle}
                  <small>// {pad(timetable.length)}</small>
                </span>
                {timetable.length > 0 && (
                  <button
                    className="st-btn"
                    disabled={classifying || classifyPreview !== null}
                    onClick={() => { void runClassify(false) }}
                    type="button"
                  >
                    {classifying && !classifyPreview ? v.timetable.classifying : v.timetable.classify}
                  </button>
                )}
              </div>
              {timetable.length === 0 ? (
                <p className="dt-study__empty">{v.timetable.calendarEmpty}</p>
              ) : (
                <WeekCalendar courseNames={courseNameById} slots={timetable} />
              )}
              {classifyNotice && <p className="dt-study__classify-notice" role="status">{classifyNotice}</p>}
              {classifyPreview && (
                <div className="dt-study__classify" role="region" aria-label={v.timetable.classify}>
                  <p className="dt-study__classify-summary">
                    {v.timetable.previewSummary(classifyPreview.scanned, classifyPreview.kept, classifyPreview.unmatched)}
                  </p>
                  <ul className="dt-study__classify-list">
                    {classifyPreview.assignments.map((item) => (
                      <li key={item.session_id} style={{ '--hue': hueOf(item.project_id) } as CSSProperties}>
                        <span className="dt-study__classify-session">
                          <b>{item.title || v.untitledSession}</b>
                          <small>{formatDate(item.started_at)} · {formatDuration(item.duration_seconds)}</small>
                        </span>
                        <span className="dt-study__classify-target">
                          <small>{item.change === 'move' ? v.timetable.move : v.timetable.assign}</small>
                          <b>{courseNameById.get(item.project_id) ?? v.otherCourse}</b>
                          <small>
                            {item.overlap_minutes > 0 ? v.timetable.overlap(item.overlap_minutes) : v.timetable.byStart}
                            {item.from_project_id && ` · ${v.timetable.from(courseNameById.get(item.from_project_id) ?? v.otherCourse)}`}
                          </small>
                        </span>
                      </li>
                    ))}
                  </ul>
                  <div className="dt-study__settings-actions">
                    <button
                      className="st-btn st-btn--primary"
                      disabled={classifying}
                      onClick={() => { void runClassify(true) }}
                      type="button"
                    >
                      {classifying ? v.timetable.classifying : v.timetable.apply(classifyPreview.assignments.length)}
                    </button>
                    <button
                      className="st-btn st-btn--quiet"
                      disabled={classifying}
                      onClick={() => setClassifyPreview(null)}
                      type="button"
                    >
                      {v.timetable.cancelPreview}
                    </button>
                  </div>
                </div>
              )}
            </section>
          )}

          <div className="dt-study__grid">
            {courses.map((item, index) => (
              <button
                className="dt-study__card st-panel"
                key={item.id}
                onClick={() => openCourse(item)}
                style={{ '--hue': hueOf(item.id) } as CSSProperties}
                type="button"
              >
                <span className="dt-study__card-cover">
                  <span className="dt-study__card-code">COURSE {pad(index + 1)}</span>
                </span>
                <span className="dt-study__card-body">
                  <strong>{item.name || v.untitledCourse}</strong>
                  {item.description && <span>{item.description}</span>}
                </span>
              </button>
            ))}
            {creating ? (
              <form
                className="dt-study__card dt-study__card--new dt-study__create st-panel"
                onSubmit={(event) => { void submitCreate(event) }}
              >
                <input
                  autoFocus
                  maxLength={160}
                  onChange={(event) => setDraftName(event.target.value)}
                  placeholder={v.courseNamePlaceholder}
                  value={draftName}
                />
                <button className="st-btn st-btn--primary" disabled={busy || !draftName.trim()} type="submit">
                  {v.create}
                </button>
                <button className="st-btn st-btn--quiet" onClick={() => { setCreating(false); setDraftName('') }} type="button">
                  {v.cancel}
                </button>
              </form>
            ) : (
              <button className="dt-study__card dt-study__card--new st-panel" onClick={() => setCreating(true)} type="button">
                <Icon name="plus" size={22} />
                <strong>{v.newCourse}</strong>
              </button>
            )}
          </div>
        </>
      )}

      {course && (
        <>
          <header className="dt-study__course-head">
            <button className="dt-study__back" onClick={closeCourse} type="button">
              <Icon name="arrow-down" size={12} />
              {v.allCourses}
            </button>
            <h2>{course.name || v.untitledCourse}</h2>
            <nav aria-label={v.coursePage} className="dt-study__tabs">
              <button className={tab === 'home' ? 'is-on' : ''} onClick={() => setTab('home')} type="button">
                {v.today}
              </button>
              <button className={tab === 'manage' ? 'is-on' : ''} onClick={() => setTab('manage')} type="button">
                {v.manage}
              </button>
              <button className={tab === 'history' ? 'is-on' : ''} onClick={() => setTab('history')} type="button">
                {practiceCopy.history}
              </button>
            </nav>
          </header>

          <div className="dt-study__downloads">
            <label>
              <span>{v.download.format}</span>
              <select
                disabled={downloadProgress !== null}
                onChange={(event) => setDownloadMode(event.target.value as TextDownloadMode)}
                value={downloadMode}
              >
                <option value="original">{v.download.original}</option>
                <option value="translation">{v.download.translation}</option>
                <option value="bilingual">{v.download.bilingual}</option>
              </select>
            </label>
            <button
              className="st-btn"
              disabled={downloadProgress !== null || !sessions?.length}
              onClick={() => { void downloadTranscripts() }}
              title={v.download.hint}
              type="button"
            >
              <Icon name="download" size={14} />
              {v.download.course}
            </button>
            <small>{v.download.hint}</small>
            {downloadProgress !== null && (
              <>
                <span role="status">{downloadProgress}</span>
                <button className="st-btn st-btn--quiet" onClick={cancelDownload} type="button">
                  {v.cancel}
                </button>
              </>
            )}
          </div>

          {(mapStale || materialsPending || serverMaterialsPending) && (
            <p role="status">{materialsPending || serverMaterialsPending ? v.materialsProcessing : v.materialsChanged}</p>
          )}
          <p className="dt-study__materials-hint">{v.studyUsageNotice}</p>
          {tab === 'home' ? renderHome() : tab === 'history' ? <StudyHistory key={course.id} projectId={course.id} /> : renderManage()}

          {practice && (
            <PracticePanel
              initialLevel={skillStates[skillKeyOf(practice.skillLabel)]?.level}
              initialStreak={skillStates[skillKeyOf(practice.skillLabel)]?.clean_streak}
              key={`${practice.skillLabel}-${practice.mode}`}
              mode={practice.mode}
              onClose={() => {
                setPractice(null)
                void refreshSkillStates(course.id)
                void refreshCosts(course.id)
                void refreshWeeks(course.id)
                announceBilling()
              }}
              openLesson={practice.openLesson}
              projectId={course.id}
              skillIndex={practice.skillIndex}
              skillLabel={practice.skillLabel}
            />
          )}
        </>
      )}
    </div>
  )
}
