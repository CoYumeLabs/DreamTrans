import { useCallback, useEffect, useRef, useState } from 'react'
import {
  formatUsageUSD,
  getStudyLesson,
  nextStudyScenario,
  revealStudyScenario,
  submitStudyAttempt,
  type StudyFormat,
  type StudyGradeResult,
  type StudyLesson,
  type StudyReveal,
  type StudyServe,
  type StudyHistoryItem,
} from '../api'
import { Icon } from '../unified/components/Icon'
import { useMessages } from '../i18n'
import { StudyReviewCard } from './StudyReview'
import { LessonCard } from './LessonCard'
import { Mascot, type MascotMood } from './Mascot'
import { useStudySound } from './useStudySound'

export type PracticeMode = 'graded' | 'free'

interface PracticePanelProps {
  projectId: string
  skillLabel: string
  /** Route position, shown as the operation code (OP-03). */
  skillIndex?: number
  initialLevel?: string
  initialStreak?: number
  mode: PracticeMode
  /** Open on the lesson card instead of the first question. */
  openLesson: boolean
  onClose: () => void
}

export const INSTRUCTOR_NAME = 'TUTOR-01'

const AUTO_NEXT_SECONDS = 4
const REST_SUGGESTION_AT = 8

/** Hint-free passes needed to leave each level (mirrors the server table). */
const LEVEL_UP_STREAK: Record<string, number> = {
  learner: 2,
  supervised: 3,
  hazard: 3,
  independent: 4,
}

const BONUS_LABELS: Record<string, string> = {
  no_hint: 'NO HINT',
  first_try: 'FIRST TRY',
  self_correction: 'SELF-CORRECTION',
  precise_language: 'PRECISE LANGUAGE',
  alternative_explanation: 'ALT. EXPLANATION',
  hidden_insight: 'HIDDEN INSIGHT',
  transfer: 'TRANSFER',
  language_independence: 'LANGUAGE INDEPENDENCE',
}

const EVENT_LABELS: Record<string, string> = {
  misconception_broken: 'MISCONCEPTION BROKEN',
  transfer_success: 'TRANSFER SUCCESS',
  critical_insight: 'CRITICAL INSIGHT',
  self_correction: 'SELF CORRECTION',
  language_save: 'LANGUAGE SAVE',
}

const OPTION_LETTERS = 'ABCDEFGH'

function comboMultiplier(combo: number): string | null {
  if (combo >= 12) return '×3'
  if (combo >= 8) return '×2'
  if (combo >= 5) return '×1.5'
  if (combo >= 3) return '×1.2'
  if (combo >= 2) return '×1.1'
  return null
}

function pad(value: number): string {
  return value < 10 ? `0${value}` : String(value)
}

function isPass(grade?: string): boolean {
  return grade === 'C' || grade === 'D' || grade === 'HD'
}

function moodForGrade(grade: string): MascotMood {
  switch (grade) {
    case 'HD':
    case 'D':
      return 'proud'
    case 'C':
      return 'happy'
    default:
      // A miss never gets a disappointed face; the tutor is thinking with you.
      return 'thinking'
  }
}

interface HistoryEntry {
  review: StudyHistoryItem
  questionNumber: number
  grade?: string
  pass: boolean
  xp: number
  targets: string[]
  nextStep?: string
  selfCorrected?: boolean
  skipped?: boolean
}

/** Counts up to the final XP so the number lands, not appears. */
function XPCounter({ value }: { value: number }) {
  const [shown, setShown] = useState(0)
  useEffect(() => {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setShown(value)
      return
    }
    let frame = 0
    const start = performance.now()
    const tick = (now: number) => {
      const progress = Math.min(1, (now - start) / 650)
      setShown(Math.round(value * (1 - Math.pow(1 - progress, 3))))
      if (progress < 1) frame = window.requestAnimationFrame(tick)
    }
    frame = window.requestAnimationFrame(tick)
    return () => window.cancelAnimationFrame(frame)
  }, [value])
  return <>{shown}</>
}

/**
 * The practice stage. One card at a time: situation → answer → the stamp,
 * the model answer and the explanation → retry or next. A lesson card
 * opens first for a new skill and is one tap away afterwards. Ends on a
 * 收工卡 that says what got clearer, never how many were wrong.
 */
export function PracticePanel({
  projectId, skillLabel, skillIndex, initialLevel, initialStreak, mode, openLesson, onClose,
}: PracticePanelProps) {
  const p = useMessages().study.practice
  const sound = useStudySound()
  const [stage, setStage] = useState<'lesson' | 'question' | 'report'>(openLesson ? 'lesson' : 'question')
  const [lesson, setLesson] = useState<StudyLesson | null | undefined>(undefined)
  const [lessonError, setLessonError] = useState<string | null>(null)
  const [lessonDrawer, setLessonDrawer] = useState(false)
  const [serve, setServe] = useState<StudyServe | null>(null)
  const [result, setResult] = useState<StudyGradeResult | null>(null)
  const [revealOnly, setRevealOnly] = useState<StudyReveal | null>(null)
  const [retrying, setRetrying] = useState(false)
  const [priorReveal, setPriorReveal] = useState<StudyReveal | null>(null)
  const [draft, setDraft] = useState('')
  const [choices, setChoices] = useState<number[]>([])
  const [fill, setFill] = useState('')
  const [judgment, setJudgment] = useState<boolean | null>(null)
  const [hintShown, setHintShown] = useState(false)
  const [zhShown, setZhShown] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [combo, setCombo] = useState(0)
  const [comboPaused, setComboPaused] = useState(false)
  const [level, setLevel] = useState(initialLevel ?? 'learner')
  const [streak, setStreak] = useState(initialStreak ?? 0)
  const [mood, setMood] = useState<MascotMood>('idle')
  const [tutorLine, setTutorLine] = useState(
    mode === 'free' ? p.introFree : p.introGraded,
  )
  const [levelUpFlash, setLevelUpFlash] = useState(false)
  const [sessionCost, setSessionCost] = useState(0)
  const [history, setHistory] = useState<HistoryEntry[]>([])
  const [countdown, setCountdown] = useState<number | null>(null)
  const practiceSessionId = useRef(crypto.randomUUID())
  const cardRef = useRef<HTMLElement | null>(null)
  const [questionNumber, setQuestionNumber] = useState(0)
  const [reviewIndex, setReviewIndex] = useState<number | null>(null)
  const fetchLock = useRef(false)

  // ---- lesson --------------------------------------------------------
  useEffect(() => {
    let cancelled = false
    getStudyLesson(projectId, skillLabel)
      .then((response) => {
        if (cancelled) return
        setLesson(response.lesson)
        if (response.cost_usd) setSessionCost((total) => total + response.cost_usd)
      })
      .catch((reason) => {
        if (cancelled) return
        setLesson(null)
        setLessonError(reason instanceof Error ? reason.message : p.errors.lesson)
      })
    return () => { cancelled = true }
  }, [p.errors.lesson, projectId, skillLabel])

  // ---- serving -------------------------------------------------------
  const fetchNext = useCallback(async () => {
    if (fetchLock.current) return
    fetchLock.current = true
    setBusy(true)
    setError(null)
    setMood('thinking')
    setCountdown(null)
    try {
      const next = await nextStudyScenario(
        projectId, skillLabel, crypto.randomUUID(), practiceSessionId.current,
      )
      setResult(null)
      setRevealOnly(null)
      setPriorReveal(null)
      setRetrying(false)
      setQuestionNumber((number) => number + 1)
      setServe(next)
      setSessionCost((total) => total + (next.cost_usd ?? 0))
      setLevel(next.level)
      setHintShown(false)
      setZhShown(Boolean(next.scaffold?.show_zh))
      setDraft('')
      setChoices([])
      setFill('')
      setJudgment(null)
      setMood('idle')
      if (next.coach_line) setTutorLine(next.coach_line)
    } catch (reason) {
      setMood('glitch')
      setError(reason instanceof Error ? reason.message : p.errors.question)
    } finally {
      setBusy(false)
      fetchLock.current = false
    }
  }, [p.errors.question, projectId, skillLabel])

  // One opening scenario per panel (guards StrictMode's double effect).
  const startedRef = useRef(false)
  useEffect(() => {
    if (startedRef.current) return
    startedRef.current = true
    void fetchNext()
  }, [fetchNext])

  // ---- auto-advance ---------------------------------------------------
  useEffect(() => {
    if (countdown === null) return
    if (countdown <= 0) {
      setCountdown(null)
      sound.next()
      void fetchNext()
      return
    }
    const timer = window.setTimeout(() => setCountdown((value) => (value === null ? null : value - 1)), 1000)
    return () => window.clearTimeout(timer)
  }, [countdown, fetchNext, sound])

  useEffect(() => {
    if (!levelUpFlash) return
    const timer = window.setTimeout(() => setLevelUpFlash(false), 1100)
    return () => window.clearTimeout(timer)
  }, [levelUpFlash])

  useEffect(() => {
    cardRef.current?.scrollIntoView({ block: 'start', behavior: 'smooth' })
  }, [serve, result, revealOnly])

  // ---- answering ------------------------------------------------------
  const format: StudyFormat = serve?.scenario.format ?? 'open'
  const reason = draft.trim()
  const fillText = fill.trim()
  const answered = result !== null || revealOnly !== null
  const canSubmit = !busy && serve !== null && !answered && (
    format === 'single' || format === 'multi'
      ? choices.length > 0
      : format === 'tf'
        ? judgment !== null
        : format === 'cloze'
          ? fillText.length > 0
          : reason.length > 0
  )

  const toggleChoice = (index: number) => {
    sound.tick()
    setChoices((current) => {
      if (format === 'single') return [index]
      return current.includes(index)
        ? current.filter((value) => value !== index)
        : [...current, index].sort((a, b) => a - b)
    })
  }

  const recordHistory = (entry: Omit<HistoryEntry, 'review' | 'questionNumber'>, reveal: StudyReveal | undefined, graded?: StudyGradeResult) => {
    if (!serve) return
    const selectedAnswer = format === 'single' || format === 'multi'
      ? choices.map((index) => `${OPTION_LETTERS[index]}. ${serve.scenario.options?.[index] ?? ''}`).join('\n')
      : format === 'tf' ? judgment === null ? '' : judgment ? p.true : p.false
        : format === 'cloze' ? fill : ''
    const review: StudyHistoryItem = {
      id: crypto.randomUUID(), skill_label: skillLabel, scenario: serve.scenario,
      answer: [selectedAnswer, draft].filter(Boolean).join('\n'),
      grade: graded?.grade, xp: entry.xp, feedback: graded?.feedback,
      next_step: graded?.next_step, reveal, created_at: new Date().toISOString(),
    }
    setHistory((current) => {
      const next = [...current, { ...entry, review, questionNumber }]
      if (next.length === REST_SUGGESTION_AT) {
        setTutorLine(p.rest)
      }
      return next
    })
  }

  const submit = async () => {
    if (!serve || !canSubmit) return
    setBusy(true)
    setError(null)
    setMood('thinking')
    sound.submit()
    try {
      if (mode === 'free') {
        const response = await revealStudyScenario(projectId, serve.scenario_id)
        setSessionCost((total) => total + (response.cost_usd ?? 0))
        setRevealOnly(response.reveal)
        setMood('happy')
        setTutorLine(p.compare)
        recordHistory({ pass: false, xp: 0, targets: response.reveal.targets ?? [], skipped: true }, response.reveal)
        sound.giveup()
        setCountdown(AUTO_NEXT_SECONDS)
        return
      }
      const graded = await submitStudyAttempt(
        projectId,
        {
          scenario_id: serve.scenario_id,
          answer: format === 'cloze' ? fillText : reason,
          ...(format === 'single' || format === 'multi' ? { choices } : {}),
          ...(format === 'tf' && judgment !== null ? { answer_bool: judgment } : {}),
          ...(format !== 'open' && reason ? { reason } : {}),
          used_hint: hintShown,
          used_zh: zhShown,
          practice_session_id: practiceSessionId.current,
        },
        crypto.randomUUID(),
      )
      const passed = isPass(graded.grade)
      setResult(graded)
      setSessionCost((total) => total + (graded.cost_usd ?? 0))
      setLevel(graded.state.level)
      setStreak(graded.state.clean_streak)
      setCombo(graded.combo ?? 0)
      setComboPaused(!passed)
      setMood((graded.events ?? []).includes('misconception_broken') ? 'surprised' : moodForGrade(graded.grade))
      setTutorLine(
        passed
          ? graded.self_corrected
            ? p.corrected
            : graded.grade === 'C' ? p.passedC : p.passed
          : p.almost,
      )
      recordHistory({
        grade: graded.grade, pass: passed, xp: graded.xp,
        targets: graded.reveal?.targets ?? [], nextStep: graded.next_step,
        selfCorrected: graded.self_corrected,
      }, graded.reveal, graded)
      if (passed) sound.pass(graded.grade); else sound.miss()
      if (graded.leveled_up) {
        setLevelUpFlash(true)
        window.setTimeout(() => sound.levelUp(), 350)
      }
      if (passed) setCountdown(AUTO_NEXT_SECONDS)
    } catch (reason) {
      setMood('glitch')
      setError(reason instanceof Error ? reason.message : p.errors.submit)
    } finally {
      setBusy(false)
    }
  }

  const giveUp = async () => {
    if (!serve || busy || answered) return
    setBusy(true)
    setError(null)
    try {
      const response = await revealStudyScenario(projectId, serve.scenario_id)
      setSessionCost((total) => total + (response.cost_usd ?? 0))
      setRevealOnly(response.reveal)
      setMood('idle')
      setTutorLine(p.revealSafe)
      recordHistory({ pass: false, xp: 0, targets: response.reveal.targets ?? [], skipped: true }, response.reveal)
      sound.giveup()
    } catch (reason) {
      setMood('glitch')
      setError(reason instanceof Error ? reason.message : p.errors.reveal)
    } finally {
      setBusy(false)
    }
  }

  const retry = () => {
    if (!result?.reveal || busy) return
    setPriorReveal(result.reveal)
    setResult(null)
    setRetrying(true)
    setCountdown(null)
    setDraft('')
    setChoices([])
    setFill('')
    setJudgment(null)
    setMood('idle')
    setTutorLine(p.retryCoach)
    sound.next()
  }

  const next = () => {
    if (busy) return
    setCountdown(null)
    sound.next()
    void fetchNext()
  }

  const finish = () => {
    if (busy) return
    setReviewIndex(null)
    setCountdown(null)
    if (history.length === 0) {
      onClose()
      return
    }
    sound.report()
    setStage('report')
  }

  const startQuestions = () => {
    sound.tick()
    setStage('question')
  }

  // ---- derived ---------------------------------------------------------
  const streakTarget = LEVEL_UP_STREAK[level] ?? 0
  const activeReveal = result?.reveal ?? revealOnly ?? priorReveal
  const scenario = serve?.scenario
  const stampGrade = result?.grade
  const stampClass = revealOnly
    ? 'is-seen'
    : stampGrade
      ? isPass(stampGrade) ? 'is-pass' : stampGrade === 'P' ? 'is-near' : 'is-miss'
      : ''
  const totalXP = history.reduce((sum, entry) => sum + entry.xp, 0)
  const passedCount = history.filter((entry) => entry.pass).length
  const fixedCount = history.filter((entry) => entry.pass && entry.selfCorrected).length
  const understood = Array.from(new Set(history.filter((entry) => entry.pass).flatMap((entry) => entry.targets))).slice(0, 4)
  const lastGap = [...history].reverse().find((entry) => !entry.pass && entry.nextStep)?.nextStep

  const renderOptions = () => {
    if (!scenario?.options) return null
    const reveal = activeReveal && (result || revealOnly) ? activeReveal : null
    return (
      <div aria-label={p.optionsAria} className="dt-practice__options" role="group">
        {scenario.options.map((option, index) => {
          const selected = choices.includes(index)
          const right = reveal?.answer_indexes?.includes(index)
          const state = reveal
            ? right ? ' is-right' : selected ? ' is-wrong' : ''
            : selected ? ' is-selected' : ''
          return (
            <button
              aria-pressed={selected}
              className={`dt-practice__option${state}`}
              disabled={answered || busy}
              key={index}
              onClick={() => toggleChoice(index)}
              type="button"
            >
              <b>{OPTION_LETTERS[index] ?? index + 1}</b>
              <span>
                {option}
                {reveal?.option_notes?.[index] && <small>{reveal.option_notes[index]}</small>}
              </span>
            </button>
          )
        })}
      </div>
    )
  }

  const renderTrueFalse = () => {
    const reveal = activeReveal && (result || revealOnly) ? activeReveal : null
    return (
      <div aria-label={p.tfAria} className="dt-practice__tf" role="group">
        {[true, false].map((value) => {
          const selected = judgment === value
          const right = reveal?.answer_bool === value
          const state = reveal
            ? right ? ' is-right' : selected ? ' is-wrong' : ''
            : selected ? ' is-selected' : ''
          return (
            <button
              aria-pressed={selected}
              className={`dt-practice__option dt-practice__option--tf${state}`}
              disabled={answered || busy}
              key={String(value)}
              onClick={() => { sound.tick(); setJudgment(value) }}
              type="button"
            >
              <b>{value ? p.true : p.false}</b>
              <small>{value ? 'TRUE' : 'FALSE'}</small>
            </button>
          )
        })}
      </div>
    )
  }

  const renderReveal = (reveal: StudyReveal, passed: boolean) => (
    <dl className="dt-practice__reveal">
      {reveal.format === 'cloze' && reveal.answer_text && (
        <div><dt>{p.answer}</dt><dd>{reveal.answer_text}</dd></div>
      )}
      {reveal.model_answer && (
        <div><dt>{p.modelAnswer}</dt><dd lang="en">{reveal.model_answer}</dd></div>
      )}
      {reveal.explanation && (
        <div><dt>{p.explanation}</dt><dd>{reveal.explanation}</dd></div>
      )}
      {!passed && reveal.gap_to_c && (
        <div><dt>{p.gapToC}</dt><dd className="dt-practice__gap">{reveal.gap_to_c}</dd></div>
      )}
    </dl>
  )

  const openReview = (index: number) => {
    if (busy || index < 0 || index >= history.length) return
    setCountdown(null)
    setReviewIndex(index)
  }
  const previousIndex = history.length - 1 - (answered ? 1 : 0)

  if (reviewIndex !== null && history[reviewIndex]) {
    return (
      <div aria-label={p.review} className="dt-practice dt-practice--report" role="dialog">
        <div className="dt-study-review">
          <nav className="dt-study-review__nav" aria-label={p.reviewNavigation}>
            <button className="st-btn" disabled={reviewIndex === 0} onClick={() => openReview(reviewIndex - 1)} type="button">{p.previous}</button>
            <span>#{pad(history[reviewIndex].questionNumber)}</span>
            <button className="st-btn" disabled={reviewIndex === history.length - 1} onClick={() => openReview(reviewIndex + 1)} type="button">{p.next}</button>
            <button className="st-btn st-btn--primary" onClick={() => setReviewIndex(null)} type="button">{stage === 'report' ? p.backToReport : p.backToCurrent}</button>
          </nav>
          <StudyReviewCard item={history[reviewIndex].review} />
        </div>
      </div>
    )
  }

  // ---- stages ----------------------------------------------------------
  if (stage === 'report') {
    return (
      <div aria-label={p.reportAria} className="dt-practice dt-practice--report" role="dialog">
        <div className="dt-practice__report st-panel">
          <div className="dt-practice__report-head">
            <Mascot mood="proud" size={64} />
            <div>
              <span className="st-label st-label--or">{p.reportTitle}</span>
              <h3>{skillLabel}</h3>
            </div>
          </div>
          <div className="dt-practice__report-xp">
            {mode === 'free' ? p.free : `+${totalXP.toLocaleString('en-US')}`}
            <small>{mode === 'free' ? 'NOT SCORED' : 'XP'}</small>
          </div>
          <dl className="dt-practice__report-list">
            <div>
              <dt>{p.cleared}</dt>
              <dd>
                {understood.length > 0
                  ? understood.map((target) => <span className="dt-practice__target" key={target}>{target}</span>)
                  : p.clearedFallback}
              </dd>
            </div>
            <div>
              <dt>{p.nextStart}</dt>
              <dd>{lastGap ?? p.nextFallback}</dd>
            </div>
            <div>
              <dt>{p.thisRun}</dt>
              <dd>
                {p.runStats(history.length, passedCount)}
                {fixedCount > 0 && p.fixedStats(fixedCount)}
                {sessionCost > 0 && p.charged(formatUsageUSD(sessionCost))}
              </dd>
            </div>
          </dl>
          <div className="dt-practice__report-actions">
            <button className="st-btn" onClick={() => openReview(0)} type="button">{p.review}</button>
            <button
              className="st-btn st-btn--primary"
              onClick={() => { sound.tick(); setStage('question'); if (!serve) void fetchNext() }}
              type="button"
            >
              <Icon name="play" size={12} />
              {p.more}
            </button>
            <button className="st-btn" onClick={onClose} type="button">{p.finish}</button>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div
      aria-label={p.practiceAria(skillLabel)}
      className={`dt-practice${levelUpFlash ? ' is-levelup' : ''}`}
      role="dialog"
    >
      <header className="dt-practice__head">
        <Mascot mood={mood} size={48} />
        <div className="dt-practice__head-text">
          <span className="st-label">
            OP-{pad((skillIndex ?? 0) + 1)} // {INSTRUCTOR_NAME}{mode === 'free' ? p.freeSuffix : ''}
          </span>
          <h3>{skillLabel}</h3>
        </div>
        <div className="dt-practice__meta">
          <span className={`dt-practice__level is-${level}`}>{p.levels[level as keyof typeof p.levels] ?? level}</span>
          {mode === 'graded' && streakTarget > 0 && (
            <span
              className="dt-practice__streak"
              title={p.streakTitle(streakTarget, Math.min(streak, streakTarget))}
            >
              {p.passLabel}
              {Array.from({ length: streakTarget }, (_, index) => (
                <i className={index < streak ? 'is-on' : ''} key={index} />
              ))}
            </span>
          )}
          {combo >= 2 && (
            <span className={`dt-practice__combo${comboPaused ? ' is-paused' : ''}`}>
              {combo} COMBO {comboMultiplier(combo)}{comboPaused ? p.paused : ''}
            </span>
          )}
          {sessionCost > 0 && (
            <span className="dt-practice__cost" title={p.costTitle}>
              {formatUsageUSD(sessionCost)}
            </span>
          )}
        </div>
        <div className="dt-practice__head-actions">
          <button className="st-btn" disabled={busy || previousIndex < 0} onClick={() => openReview(previousIndex)} type="button">{p.previous}</button>
          <button className="st-btn" disabled={busy || history.length === 0} onClick={() => openReview(0)} type="button">{p.review}</button>
          <button
            className={`st-btn st-btn--quiet${lessonDrawer ? ' is-on' : ''}`}
            disabled={lesson === undefined}
            onClick={() => { sound.tick(); setLessonDrawer((open) => !open) }}
            type="button"
          >
            {p.lessonButton}
          </button>
          <button
            aria-label={p.finish}
            disabled={busy}
            className="st-iconbtn"
            onClick={finish}
            title={p.finishTitle}
            type="button"
          >
            <Icon name="close" size={17} />
          </button>
        </div>
      </header>

      <div className="dt-practice__body">
        <div className="dt-practice__stage">
          {stage === 'lesson' && (
            <div className="dt-practice__lesson st-panel">
              {lesson === undefined && !lessonError && (
                <p className="dt-practice__busy">{p.writingLesson}</p>
              )}
              {lessonError && (
                <p className="dt-practice__error" role="alert">{lessonError}</p>
              )}
              {lesson && (
                <LessonCard
                  action={(
                    <button className="st-btn st-btn--primary" onClick={startQuestions} type="button">
                      <Icon name="play" size={12} />
                      {p.lessonDone}
                    </button>
                  )}
                  lesson={lesson.content}
                  skillLabel={skillLabel}
                />
              )}
              {(lessonError || lesson === null) && (
                <button className="st-btn" onClick={startQuestions} type="button">{p.direct}</button>
              )}
              {lesson === undefined && !lessonError && (
                <button className="st-btn st-btn--quiet" onClick={startQuestions} type="button">{p.skipLesson}</button>
              )}
            </div>
          )}

          {stage === 'question' && serve && scenario && (
            <article
              className={`dt-practice__card st-panel${retrying ? ' is-retry' : ''}`}
              key={`${serve.scenario_id}-${retrying ? 'retry' : 'first'}`}
              ref={cardRef}
            >
              <div className="dt-practice__card-head">
                <span className="st-label">
                  {p.tiers[serve.difficulty as keyof typeof p.tiers] ?? p.tiers[1]} // {p.formats[format]}{retrying ? p.retrySuffix : ''}
                </span>
                {scenario.lang && <span className="dt-practice__lang">{scenario.lang}</span>}
                <span className="st-label st-label--mu">#{pad(questionNumber)}</span>
              </div>
              {serve.coach_line && !retrying && <p className="dt-practice__coach">{serve.coach_line}</p>}
              <p className="dt-practice__situation">{scenario.situation}</p>
              <p className="dt-practice__question">{scenario.question}</p>
              {zhShown && scenario.question_zh && <p className="dt-practice__zh">{scenario.question_zh}</p>}

              {(format === 'single' || format === 'multi') && renderOptions()}
              {format === 'tf' && renderTrueFalse()}
              {format === 'cloze' && !answered && (
                <input
                  aria-label={p.fillAria}
                  className="dt-practice__fill"
                  disabled={busy}
                  maxLength={160}
                  onChange={(event) => setFill(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') { event.preventDefault(); void submit() }
                  }}
                  placeholder={p.fillPlaceholder}
                  value={fill}
                />
              )}

              {(scenario.glossary ?? []).length > 0 && !answered && (
                <dl className="dt-practice__glossary">
                  {scenario.glossary!.map((item) => (
                    <div key={item.term}><dt>{item.term}</dt><dd>{item.gloss}</dd></div>
                  ))}
                </dl>
              )}
              {!answered && format !== 'single' && format !== 'multi' && format !== 'tf'
                && (scenario.starters ?? []).length > 0 && (
                <div className="dt-practice__starters">
                  {scenario.starters!.map((starter) => (
                    <button
                      className="dt-practice__starter"
                      key={starter}
                      onClick={() => { sound.tick(); setDraft((current) => (current.trim() ? current : `${starter} `)) }}
                      title={p.starterTitle}
                      type="button"
                    >
                      {starter}
                    </button>
                  ))}
                </div>
              )}
              {hintShown && scenario.hint && <p className="dt-practice__hint">{p.hint(scenario.hint)}</p>}

              {!answered && (
                <>
                  <textarea
                    className="dt-practice__answer"
                    disabled={busy}
                    maxLength={4000}
                    onChange={(event) => setDraft(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
                        event.preventDefault()
                        void submit()
                      }
                    }}
                    placeholder={format === 'open'
                      ? p.openPlaceholder
                      : p.reasonPlaceholder}
                    value={draft}
                  />
                  <div className="dt-practice__actions">
                    <button
                      className="st-btn st-btn--primary"
                      disabled={!canSubmit}
                      onClick={() => { void submit() }}
                      type="button"
                    >
                      {p.submit}
                    </button>
                    <button
                      className="st-btn st-btn--quiet"
                      disabled={busy}
                      onClick={() => { void giveUp() }}
                      title={p.revealTitle}
                      type="button"
                    >
                      {p.reveal}
                    </button>
                    <span className="dt-practice__spacer" />
                    {scenario.question_zh && !zhShown && (
                      <button className="st-btn st-btn--quiet" onClick={() => { sound.tick(); setZhShown(true) }} type="button">
                        {p.showChinese}
                      </button>
                    )}
                    {scenario.hint && !hintShown && (
                      <button
                        className="st-btn st-btn--quiet"
                        onClick={() => { sound.tick(); setHintShown(true); setMood('wink') }}
                        title={p.hintTitle}
                        type="button"
                      >
                        {p.askHint}
                      </button>
                    )}
                  </div>
                </>
              )}

              {retrying && priorReveal && !answered && (
                <details className="dt-practice__prior">
                  <summary>{p.priorReveal}</summary>
                  {renderReveal(priorReveal, false)}
                </details>
              )}

              {(result || revealOnly) && (
                <div className="dt-practice__result">
                  <div className="dt-practice__stamp-row">
                    <span className={`dt-practice__stamp ${stampClass}`}>
                      {revealOnly ? p.seen : p.grades[stampGrade as keyof typeof p.grades] ?? stampGrade}
                    </span>
                    {stampGrade && (
                      <span className={`dt-practice__grade dt-practice__grade--${stampGrade}`}>{stampGrade}</span>
                    )}
                    {result && (
                      <span className="dt-practice__xp">
                        +<XPCounter value={result.xp} />
                        <small>
                          XP
                          {result.difficulty_multiplier && result.difficulty_multiplier !== 1 && p.difficulty(result.difficulty_multiplier)}
                          {isPass(result.grade) && (result.combo ?? 0) >= 2 && ` · COMBO ${comboMultiplier(result.combo ?? 0)}`}
                        </small>
                      </span>
                    )}
                    {result?.bonuses.map((bonus) => (
                      <em className={`dt-practice__bonus${bonus === 'self_correction' ? ' is-ok' : ''}`} key={bonus}>
                        {BONUS_LABELS[bonus] ?? bonus}
                      </em>
                    ))}
                    {revealOnly && <span className="dt-practice__seen-note">{p.seenNote}</span>}
                  </div>

                  {result && (
                    <>
                      <p className="dt-practice__feedback">{result.feedback}</p>
                      <p className="dt-practice__next-step">{result.next_step}</p>
                    </>
                  )}

                  {(activeReveal?.targets ?? []).length > 0 && (
                    <div className="dt-practice__targets">
                      <span className="st-label st-label--mu">{p.targets}</span>
                      {activeReveal!.targets!.map((target) => (
                        <span className="dt-practice__target" key={target}>{target}</span>
                      ))}
                    </div>
                  )}

                  {activeReveal && renderReveal(activeReveal, isPass(result?.grade))}

                  {result?.language_tip && (
                    <p className="dt-practice__tip"><b>EN</b>{result.language_tip}</p>
                  )}
                  {(result?.events ?? []).map((event) => (
                    <p className="dt-practice__event" key={event}>{EVENT_LABELS[event] ?? event}</p>
                  ))}
                  {result?.leveled_up && (
                    <p className="dt-practice__levelup">
                      {p.levelUp(p.levels[result.state.level as keyof typeof p.levels] ?? result.state.level)}
                    </p>
                  )}

                  <div className="dt-practice__next-row">
                    {result?.retry_allowed ? (
                      <>
                        <button className="st-btn st-btn--primary" disabled={busy} onClick={retry} type="button">
                          {p.retry}
                        </button>
                        <button className="st-btn" disabled={busy} onClick={next} type="button">{p.another}</button>
                        <span className="st-label st-label--mu">{p.correctionBonus}</span>
                      </>
                    ) : (
                      <>
                        {countdown !== null && (
                          <span
                            aria-label={p.nextIn(countdown)}
                            className="dt-practice__ring"
                            style={{ '--pct': `${((AUTO_NEXT_SECONDS - countdown) / AUTO_NEXT_SECONDS) * 100}%` } as React.CSSProperties}
                          >
                            <i>{countdown}</i>
                          </span>
                        )}
                        <button className="st-btn st-btn--primary" disabled={busy} onClick={next} type="button">
                          <Icon name="play" size={12} />
                          {p.next}
                        </button>
                        {countdown !== null && (
                          <button className="st-btn st-btn--quiet" onClick={() => { sound.tick(); setCountdown(null) }} type="button">
                            {p.pause}
                          </button>
                        )}
                        <button className="st-btn st-btn--quiet" disabled={busy} onClick={finish} type="button">{p.finish}</button>
                      </>
                    )}
                  </div>
                </div>
              )}
            </article>
          )}

          {stage === 'question' && busy && !serve && (
            <p className="dt-practice__busy">{p.loading}</p>
          )}
          {stage === 'question' && busy && serve && !answered && (
            <p className="dt-practice__busy">{p.grading}</p>
          )}
          {error && (
            <p className="dt-practice__error" role="alert">
              {error}
              <button
                onClick={() => {
                  setError(null)
                  if (!serve) void fetchNext(); else setMood('idle')
                }}
                type="button"
              >
                {serve ? p.gotIt : p.retry}
              </button>
            </p>
          )}
        </div>

        <aside className="dt-practice__rail">
          <div className="st-panel dt-practice__tutor">
            <span className="st-label">{INSTRUCTOR_NAME}</span>
            <p>{tutorLine}</p>
          </div>
          <div className="st-panel dt-practice__progress">
            <span className="st-label">{p.action}</span>
            <div className="dt-practice__progress-bar">
              {history.map((entry, index) => (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => openReview(index)}
                  aria-label={`${p.review} #${entry.questionNumber} · ${entry.grade ?? p.seen}`}
                  className={entry.pass ? 'is-done' : entry.skipped ? 'is-seen' : 'is-miss'}
                  key={index}
                  title={entry.grade ?? p.seen}
                />
              ))}
              {serve && !answered && <i className="is-cur" />}
            </div>
            <small>{p.progress(history.length, passedCount, fixedCount)}</small>
          </div>
          <p className="dt-practice__promise">
            {p.promise}
          </p>
        </aside>
      </div>

      {lessonDrawer && lesson && (
        <div className="dt-practice__drawer" role="dialog" aria-label={p.lessonButton}>
          <button aria-label={p.closeLesson} className="st-iconbtn dt-practice__drawer-close" onClick={() => setLessonDrawer(false)} type="button">
            <Icon name="close" size={16} />
          </button>
          <LessonCard lesson={lesson.content} skillLabel={skillLabel} />
        </div>
      )}
    </div>
  )
}
