/* Pure helpers shared by the chart kit and the pages that feed it. */

export type Row = Record<string, string | number | null | undefined>
export interface Series { key: string; label: string; slot?: number }

// Float subtraction in SQL can yield -0, which would print as "-0".
export const number = (value: unknown): number => typeof value === 'number' && Number.isFinite(value) && value !== 0 ? value : 0
export const slotColor = (slot: number) => `var(--viz-${Math.min(4, Math.max(1, slot))})`
export const ordinalColor = (index: number, total: number) => {
  const steps = 6
  const step = total <= 1 ? steps : Math.round(1 + (index / (total - 1)) * (steps - 1))
  return `var(--viz-ord-${Math.min(steps, Math.max(1, step))})`
}

export function formatCompact(value: number, digits = 1): string {
  const abs = Math.abs(value)
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(digits)}M`
  if (abs >= 10_000) return `${(value / 1_000).toFixed(digits)}K`
  return value.toLocaleString(undefined, { maximumFractionDigits: abs >= 100 ? 0 : abs >= 10 ? 1 : 2 })
}
export const formatUsd = (value: number) => `${value < 0 ? '−' : ''}$${formatCompact(Math.abs(value))}`
export const formatPercent = (value: number) => `${(value * 100).toFixed(value >= 0.1 ? 0 : 1)}%`

export function periodLabel(value: unknown, grain: string): string {
  if (typeof value !== 'string') return String(value ?? '—')
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  if (grain === 'month') return `${date.getUTCFullYear()}-${String(date.getUTCMonth() + 1).padStart(2, '0')}`
  return `${date.getUTCMonth() + 1}/${date.getUTCDate()}`
}

/** Clean axis ticks: 0 and up to `count` round numbers reaching past the data. */
export function niceTicks(min: number, max: number, count = 4): number[] {
  const lo = Math.min(0, min), hi = Math.max(0, max)
  const span = hi - lo || 1
  const rough = span / count
  const magnitude = 10 ** Math.floor(Math.log10(rough))
  const residual = rough / magnitude
  const step = (residual >= 5 ? 10 : residual >= 2 ? 5 : residual >= 1 ? 2 : 1) * magnitude
  const start = Math.floor(lo / step) * step
  const end = Math.ceil(hi / step) * step
  const ticks: number[] = []
  for (let value = start; value <= end + step / 2; value += step) ticks.push(Number(value.toFixed(10)))
  return ticks
}

