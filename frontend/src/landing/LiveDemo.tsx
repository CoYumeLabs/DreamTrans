import { useEffect, useState } from 'react'
import { useMessages, type Messages } from '../i18n'
import { Icon } from '../unified/components/Icon'

/**
 * Hero demo: a miniature workspace that "records" a short lecture. Lines
 * arrive as partials, get finalised, then their translation lands; the view
 * switch cycles through 双语 / 学习 / 译文 so visitors see the reading modes
 * without touching anything.
 */

type DemoMode = 'bilingual' | 'learn' | 'translation'

interface Gloss {
  word: string
  zh: string
}

interface DemoLine {
  speaker: string
  en: string
  zh: string
  gloss?: Gloss[]
  /** AI chip shown after this line is translated. */
  ai?: string
}

// The lecture is always English → Chinese: that is the product's flagship
// pair, and the glosses only make sense that way round. Speaker labels and
// AI chips follow the interface language.
function demoScript(m: Messages): DemoLine[] {
  const { lecturer, student } = m.demo.speakers
  return [
    {
      speaker: lecturer,
      en: 'Today we look at how a neural network learns from examples.',
      zh: '今天我们来看神经网络是如何从样例中学习的。',
      gloss: [{ word: 'neural network', zh: '神经网络' }],
    },
    {
      speaker: lecturer,
      en: 'Each layer applies weights, then a non-linear activation.',
      zh: '每一层先乘以权重，再经过一个非线性激活函数。',
      gloss: [{ word: 'weights', zh: '权重' }, { word: 'activation', zh: '激活函数' }],
    },
    {
      speaker: student,
      en: 'So back-propagation is what adjusts those weights?',
      zh: '所以反向传播就是用来调整这些权重的？',
      gloss: [{ word: 'back-propagation', zh: '反向传播' }],
      ai: m.demo.aiChips[0],
    },
    {
      speaker: lecturer,
      en: 'Exactly. The gradient tells us which direction reduces the loss.',
      zh: '没错。梯度告诉我们往哪个方向能降低损失。',
      gloss: [{ word: 'gradient', zh: '梯度' }, { word: 'loss', zh: '损失' }],
    },
    {
      speaker: lecturer,
      en: 'We will run one epoch on the assignment data next week.',
      zh: '下周我们会在作业数据上跑一个完整轮次。',
      gloss: [{ word: 'epoch', zh: '训练轮次' }],
      ai: m.demo.aiChips[1],
    },
  ]
}

const SCRIPT_LENGTH = 5
const MODES: readonly DemoMode[] = ['bilingual', 'learn', 'translation']

type Stage = 'partial' | 'final' | 'translated'

interface DemoState {
  /** Number of lines that have started appearing. */
  count: number
  /** Stage of the newest line. */
  stage: Stage
}

const STEP_MS: Record<Stage, number> = {
  partial: 900,
  final: 800,
  translated: 1_500,
}
const RESET_PAUSE_MS = 3_200
const MODE_CYCLE_MS = 6_500

function prefersReducedMotion(): boolean {
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches
}

function formatTimer(totalSeconds: number): string {
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`
}

function partialText(text: string): string {
  const words = text.split(' ')
  return words.slice(0, Math.max(3, Math.ceil(words.length * 0.55))).join(' ')
}

function withGloss(text: string, gloss: Gloss[] | undefined) {
  if (!gloss || gloss.length === 0) return text
  const pattern = new RegExp(`(${gloss.map((item) => item.word.replace(/[.*+?^${}()|[\]\\-]/g, '\\$&')).join('|')})`, 'i')
  const parts = text.split(pattern)
  return parts.map((part, index) => {
    const hit = gloss.find((item) => item.word.toLowerCase() === part.toLowerCase())
    if (!hit) return <span key={index}>{part}</span>
    return (
      <mark className="lp-demo__gloss" key={index}>
        {part}
        <small>{hit.zh}</small>
      </mark>
    )
  })
}

export function LiveDemo() {
  const m = useMessages()
  const SCRIPT = demoScript(m)
  const [reduced] = useState(prefersReducedMotion)
  const [state, setState] = useState<DemoState>(
    // Start with two finished lines so the first paint is not an empty feed.
    reduced ? { count: SCRIPT_LENGTH, stage: 'translated' } : { count: 2, stage: 'translated' },
  )
  const [mode, setMode] = useState<DemoMode>('bilingual')
  const [seconds, setSeconds] = useState(14 * 60 + 2)

  // Line arrival state machine.
  useEffect(() => {
    if (reduced) return
    const done = state.count >= SCRIPT_LENGTH && state.stage === 'translated'
    const delay = done ? RESET_PAUSE_MS : STEP_MS[state.stage]
    const timer = window.setTimeout(() => {
      setState((current) => {
        if (current.count >= SCRIPT_LENGTH && current.stage === 'translated') {
          return { count: 1, stage: 'partial' }
        }
        if (current.stage === 'partial') return { ...current, stage: 'final' }
        if (current.stage === 'final') return { ...current, stage: 'translated' }
        return { count: current.count + 1, stage: 'partial' }
      })
    }, delay)
    return () => window.clearTimeout(timer)
  }, [state, reduced])

  useEffect(() => {
    if (reduced) return
    const timer = window.setInterval(() => {
      setMode((current) => MODES[(MODES.indexOf(current) + 1) % MODES.length] ?? 'bilingual')
    }, MODE_CYCLE_MS)
    return () => window.clearInterval(timer)
  }, [reduced])

  useEffect(() => {
    if (reduced) return
    const tick = window.setInterval(() => setSeconds((value) => value + 1), 1_000)
    return () => window.clearInterval(tick)
  }, [reduced])

  const lines = SCRIPT.slice(0, state.count)

  return (
    <div className="lp-demo" data-mode={mode}>
      <div className="lp-demo__bar">
        <span className="lp-demo__status">
          <i className="lp-demo__dot" />
          {m.demo.recording}
        </span>
        <span className="lp-demo__timer">{formatTimer(seconds)}</span>
      </div>
      <div className="lp-demo__modes" aria-hidden="true">
        {MODES.map((item) => (
          <span className={item === mode ? 'is-active' : undefined} key={item}>
            {m.demo.modes[item]}
          </span>
        ))}
      </div>
      <div className="lp-demo__feed">
        {lines.map((line, index) => {
          const last = index === lines.length - 1
          const stage: Stage = last ? state.stage : 'translated'
          const showEn = mode !== 'translation' || stage !== 'translated'
          const showZh = mode !== 'learn' && stage === 'translated'
          return (
            <div className="lp-demo__group" key={index}>
              <div className={`lp-demo__row${stage === 'partial' ? ' is-partial' : ''}`}>
                <span className="lp-demo__speaker">{line.speaker}</span>
                {showEn && (
                  <p className="lp-demo__en">
                    {stage === 'partial'
                      ? partialText(line.en)
                      : mode === 'learn'
                        ? withGloss(line.en, line.gloss)
                        : line.en}
                    {stage === 'partial' && <i className="lp-demo__caret" />}
                  </p>
                )}
                {showZh && <p className="lp-demo__zh">{line.zh}</p>}
                {mode === 'bilingual' && stage === 'final' && (
                  <p className="lp-demo__zh is-pending">{m.demo.translating}</p>
                )}
              </div>
              {line.ai && stage === 'translated' && (
                <div className="lp-demo__ai">
                  <Icon name="sparkles" size={13} />
                  <span>{line.ai}</span>
                </div>
              )}
            </div>
          )
        })}
      </div>
      <div className="lp-demo__recorder" aria-hidden="true">
        <span><Icon name="sparkles" size={14} />{m.demo.ai}</span>
        <span className="lp-demo__mic"><Icon name="stop" size={16} /></span>
        <span><Icon name="wave" size={14} />{m.demo.insights}</span>
      </div>
    </div>
  )
}
