import { useEffect, useRef, useState } from 'react'
import { getStudyHistory, type StudyHistoryItem } from '../api'
import { intlLocale, useMessages } from '../i18n'

export function StudyReviewCard({ item }: { item: StudyHistoryItem }) {
  const p = useMessages().study.practice
  const { scenario, reveal } = item
  return (
    <article className="dt-study-review__card st-panel">
      <header>
        <strong>{item.skill_label}</strong>
        <time dateTime={item.created_at}>{new Date(item.created_at).toLocaleString(intlLocale())}</time>
        <span>{item.grade ?? p.seen}{item.grade ? ` · ${item.xp} XP` : ''}</span>
      </header>
      {scenario ? <>
        <p>{scenario.situation}</p>
        <h3>{scenario.question}</h3>
        {scenario.question_zh && <p>{scenario.question_zh}</p>}
        {scenario.options && <ol type="A">
          {scenario.options.map((option, index) => <li key={index}>
            {option}{reveal?.answer_indexes?.includes(index) && <strong> — {p.correctAnswer}</strong>}
            {reveal?.option_notes?.[index] && <p>{reveal.option_notes[index]}</p>}
          </li>)}
        </ol>}
      </> : <p>{p.questionUnavailable}</p>}
      <dl className="dt-practice__reveal">
        <div><dt>{p.yourAnswer}</dt><dd>{item.answer || p.noAnswer}</dd></div>
        {item.feedback && <div><dt>{p.feedbackLabel}</dt><dd>{item.feedback}</dd></div>}
        {item.next_step && <div><dt>{p.nextStart}</dt><dd>{item.next_step}</dd></div>}
        {reveal?.answer_text && <div><dt>{p.answer}</dt><dd>{reveal.answer_text}</dd></div>}
        {typeof reveal?.answer_bool === 'boolean' && <div><dt>{p.answer}</dt><dd>{reveal.answer_bool ? p.true : p.false}</dd></div>}
        {reveal?.model_answer && <div><dt>{p.modelAnswer}</dt><dd>{reveal.model_answer}</dd></div>}
        {reveal?.explanation && <div><dt>{p.explanation}</dt><dd>{reveal.explanation}</dd></div>}
        {reveal?.gap_to_c && <div><dt>{p.gapToC}</dt><dd>{reveal.gap_to_c}</dd></div>}
      </dl>
    </article>
  )
}

/** A separate read-only entry: opening history never mounts the paid practice flow. */
export function StudyHistory({ projectId }: { projectId: string }) {
  const p = useMessages().study.practice
  const [items, setItems] = useState<StudyHistoryItem[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [cursor, setCursor] = useState('')
  const [busy, setBusy] = useState(true)
  const [error, setError] = useState(false)
  const [request, setRequest] = useState({ before: '', revision: 0 })
  const requestLock = useRef(false)

  useEffect(() => {
    let cancelled = false
    getStudyHistory(projectId, request.before).then((response) => {
      if (cancelled) return
      setItems((current) => request.before
        ? [...current, ...response.items.filter((item) => !current.some(({ id }) => id === item.id))]
        : response.items)
      setCursor(response.next_cursor)
    }).catch(() => { if (!cancelled) setError(true) }).finally(() => {
      if (!cancelled) { setBusy(false); requestLock.current = false }
    })
    return () => { cancelled = true }
  }, [projectId, request])

  const load = (before: string) => {
    if (requestLock.current) return
    requestLock.current = true
    setBusy(true)
    setError(false)
    setRequest((current) => ({ before, revision: current.revision + 1 }))
  }
  const index = items.findIndex(({ id }) => id === selected)
  return (
    <section className="dt-study-review" aria-label={p.history}>
      <p>{p.historyDescription}</p>
      {index >= 0 ? <>
        <nav className="dt-study-review__nav" aria-label={p.reviewNavigation}>
          <button className="st-btn" onClick={() => setSelected(null)} type="button">{p.history}</button>
          <button className="st-btn" disabled={index === 0} onClick={() => setSelected(items[index - 1].id)} type="button">{p.newerAttempt}</button>
          <button className="st-btn" disabled={index === items.length - 1} onClick={() => setSelected(items[index + 1].id)} type="button">{p.olderAttempt}</button>
        </nav>
        <StudyReviewCard item={items[index]} />
      </> : <ol className="dt-study-review__list">
        {items.map((item) => <li key={item.id}>
          <button className="st-btn" onClick={() => setSelected(item.id)} type="button">
            <strong>{item.scenario?.question || item.skill_label}</strong>
            <span>{item.skill_label} · {item.grade || p.seen} · {new Date(item.created_at).toLocaleString(intlLocale())}</span>
          </button>
        </li>)}
      </ol>}
      {!busy && !error && items.length === 0 && <p>{p.noHistory}</p>}
      {busy && <p role="status">{p.loadingHistory}</p>}
      {error && <p role="alert">{p.historyError} <button className="st-btn" disabled={busy} onClick={() => load(request.before)} type="button">{p.retry}</button></p>}
      {!error && cursor && <button className="st-btn" disabled={busy} onClick={() => load(cursor)} type="button">{p.loadMore}</button>}
    </section>
  )
}
