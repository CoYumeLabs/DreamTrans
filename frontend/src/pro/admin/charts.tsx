import { useEffect, useRef, useState, type ReactNode, type RefObject } from 'react'
import { niceTicks, number, slotColor, type Row, type Series } from './chartUtils'


/*
 * Small chart kit for the console. Colors are CSS custom properties defined in
 * pro-admin.css (--viz-*); the palette was validated for CVD separation and
 * contrast against the white card surface, so use the slots in order and never
 * generate new hues here.
 */

function useWidth<T extends HTMLElement>(): [RefObject<T | null>, number] {
  const ref = useRef<T>(null)
  const [width, setWidth] = useState(0)
  useEffect(() => {
    const element = ref.current
    if (!element) return
    const update = () => setWidth(element.clientWidth)
    update()
    const observer = new ResizeObserver(update)
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  return [ref, width]
}

/** Column path with 4px rounded data-end and a square baseline. */
function columnPath(x: number, y0: number, y1: number, width: number): string {
  const top = Math.min(y0, y1), bottom = Math.max(y0, y1)
  const height = bottom - top
  const radius = Math.min(4, width / 2, height)
  if (height <= 0) return ''
  if (y1 <= y0) {
    return `M${x},${bottom} V${top + radius} a${radius},${radius} 0 0 1 ${radius},-${radius} H${x + width - radius} a${radius},${radius} 0 0 1 ${radius},${radius} V${bottom} Z`
  }
  return `M${x},${top} V${bottom - radius} a${radius},${radius} 0 0 0 ${radius},${radius} H${x + width - radius} a${radius},${radius} 0 0 0 ${radius},-${radius} V${top} Z`
}

export function Legend({ series, kind = 'rect' }: { series: Series[]; kind?: 'rect' | 'line' }) {
  if (series.length < 2) return null
  return (
    <div className="viz-legend">
      {series.map((item, index) => (
        <span key={item.key}>
          <i className={`viz-legend__key viz-legend__key--${kind}`} style={{ background: slotColor(item.slot ?? index + 1) }} />
          {item.label}
        </span>
      ))}
    </div>
  )
}

interface TooltipState { index: number; left: number }

function Tooltip({ title, entries, left, width }: { title: string; entries: { label: string; value: string; color?: string }[]; left: number; width: number }) {
  const flip = left > width * 0.6
  return (
    <div className="viz-tooltip" style={{ left, transform: flip ? 'translateX(calc(-100% - 12px))' : 'translateX(12px)' }} role="status">
      <strong>{title}</strong>
      {entries.map(entry => (
        <div key={entry.label}>
          {entry.color && <i style={{ background: entry.color }} />}
          <span>{entry.label}</span>
          <b>{entry.value}</b>
        </div>
      ))}
    </div>
  )
}

interface FrameProps {
  rows: Row[]
  series: Series[]
  xKey: string
  xLabel: (value: unknown) => string
  format: (value: number) => string
  title: string
  height?: number
  extra?: (row: Row) => { label: string; value: string }[]
}

const PAD = { top: 12, right: 12, bottom: 26, left: 48 }

function frame(rows: Row[], series: Series[], stacked: boolean, height: number, width: number) {
  const totals = rows.map(row => {
    const values = series.map(item => number(row[item.key]))
    if (!stacked) return { max: Math.max(0, ...values), min: Math.min(0, ...values) }
    return {
      max: values.filter(value => value > 0).reduce((sum, value) => sum + value, 0),
      min: values.filter(value => value < 0).reduce((sum, value) => sum + value, 0),
    }
  })
  const ticks = niceTicks(Math.min(0, ...totals.map(total => total.min)), Math.max(0, ...totals.map(total => total.max)))
  const lo = ticks[0], hi = ticks[ticks.length - 1]
  const plotWidth = Math.max(0, width - PAD.left - PAD.right)
  const plotHeight = height - PAD.top - PAD.bottom
  const y = (value: number) => PAD.top + plotHeight - ((value - lo) / (hi - lo || 1)) * plotHeight
  const band = rows.length ? plotWidth / rows.length : plotWidth
  const x = (index: number) => PAD.left + index * band
  return { ticks, y, x, band, plotWidth, plotHeight }
}

function Axes({ ticks, y, xLabels, x, band, width, format }: { ticks: number[]; y: (value: number) => number; xLabels: string[]; x: (index: number) => number; band: number; width: number; format: (value: number) => string }) {
  const every = Math.max(1, Math.ceil(xLabels.length / Math.max(1, Math.floor((width - PAD.left) / 56))))
  return (
    <g className="viz-axes">
      {ticks.map(tick => (
        <g key={tick}>
          <line x1={PAD.left} x2={width - PAD.right} y1={y(tick)} y2={y(tick)} className={tick === 0 ? 'viz-baseline' : 'viz-gridline'} />
          <text x={PAD.left - 8} y={y(tick)} dy="0.35em" textAnchor="end" className="viz-tick">{format(tick)}</text>
        </g>
      ))}
      {xLabels.map((label, index) => (index % every === 0 || index === xLabels.length - 1) && (
        <text key={index} x={x(index) + band / 2} y={y(ticks[0]) + 18} textAnchor="middle" className="viz-tick">{label}</text>
      ))}
    </g>
  )
}

export function ColumnChart({ rows, series, xKey, xLabel, format, title, height = 220, stacked = false, extra }: FrameProps & { stacked?: boolean }) {
  const [ref, width] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<TooltipState | null>(null)
  const { ticks, y, x, band, plotWidth } = frame(rows, series, stacked, height, width)
  if (!rows.length) return <p className="pa-chart-empty">所选范围内暂无数据</p>
  const groupWidth = stacked ? 1 : series.length
  const slot = band / groupWidth
  const thickness = Math.min(24, Math.max(3, slot - (stacked ? 8 : 2)))
  const labels = rows.map(row => xLabel(row[xKey]))
  return (
    <div className="viz" ref={ref} onPointerLeave={() => setHover(null)}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label={title}>
          <title>{title}，详细数值见表格</title>
          <Axes ticks={ticks} y={y} xLabels={labels} x={x} band={band} width={width} format={format} />
          {hover && <rect x={x(hover.index)} y={PAD.top} width={band} height={height - PAD.top - PAD.bottom} className="viz-hover-band" />}
          {rows.map((row, index) => {
            let positive = 0, negative = 0
            return series.map((item, s) => {
              const value = number(row[item.key])
              if (value === 0) return null
              let y0: number, y1: number, bx: number
              if (stacked) {
                const from = value > 0 ? positive : negative
                const to = from + value
                if (value > 0) positive = to; else negative = to
                y0 = y(from); y1 = y(to)
                // 2px surface gap between segments: shave the inner edge.
                if (value > 0 && from > 0) y0 -= 2
                if (value < 0 && from < 0) y0 += 2
                bx = x(index) + (band - thickness) / 2
              } else {
                y0 = y(0); y1 = y(value)
                bx = x(index) + s * slot + (slot - thickness) / 2
              }
              return <path key={item.key} d={columnPath(bx, y0, y1, thickness)} fill={slotColor(item.slot ?? s + 1)} className={hover && hover.index !== index ? 'viz-dim' : ''} />
            })
          })}
          {rows.map((_, index) => (
            <rect key={index} x={x(index)} y={PAD.top} width={band} height={height - PAD.top - PAD.bottom} fill="transparent"
              onPointerMove={() => setHover({ index, left: x(index) + band / 2 })} onFocus={() => setHover({ index, left: x(index) + band / 2 })} tabIndex={0} aria-label={labels[index]} />
          ))}
        </svg>
      )}
      {hover && rows[hover.index] && (
        <Tooltip title={labels[hover.index]} left={hover.left} width={PAD.left + plotWidth}
          entries={[...series.map((item, s) => ({ label: item.label, value: format(number(rows[hover.index][item.key])), color: slotColor(item.slot ?? s + 1) })), ...(extra?.(rows[hover.index]) ?? [])]} />
      )}
    </div>
  )
}

export function LineChart({ rows, series, xKey, xLabel, format, title, height = 220, extra }: FrameProps) {
  const [ref, width] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<TooltipState | null>(null)
  const { ticks, y, x, band, plotWidth } = frame(rows, series, false, height, width)
  if (!rows.length) return <p className="pa-chart-empty">所选范围内暂无数据</p>
  const labels = rows.map(row => xLabel(row[xKey]))
  const cx = (index: number) => x(index) + band / 2
  const single = series.length === 1
  return (
    <div className="viz" ref={ref} onPointerLeave={() => setHover(null)}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label={title}>
          <title>{title}，详细数值见表格</title>
          <Axes ticks={ticks} y={y} xLabels={labels} x={x} band={band} width={width} format={format} />
          {hover && <line x1={cx(hover.index)} x2={cx(hover.index)} y1={PAD.top} y2={height - PAD.bottom} className="viz-crosshair" />}
          {series.map((item, s) => {
            const points = rows.map((row, index) => [cx(index), y(number(row[item.key]))] as const)
            const color = slotColor(item.slot ?? s + 1)
            const line = points.map(([px, py], index) => `${index ? 'L' : 'M'}${px},${py}`).join(' ')
            return (
              <g key={item.key}>
                {single && <path d={`${line} L${points[points.length - 1][0]},${y(0)} L${points[0][0]},${y(0)} Z`} fill={color} opacity={0.1} />}
                <path d={line} fill="none" stroke={color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
                {points.map(([px, py], index) => (hover?.index === index || rows.length <= 12) && (
                  <circle key={index} cx={px} cy={py} r={hover?.index === index ? 5 : 3.5} fill={color} className="viz-marker" />
                ))}
              </g>
            )
          })}
          {rows.map((_, index) => (
            <rect key={index} x={x(index)} y={PAD.top} width={band} height={height - PAD.top - PAD.bottom} fill="transparent"
              onPointerMove={() => setHover({ index, left: cx(index) })} onFocus={() => setHover({ index, left: cx(index) })} tabIndex={0} aria-label={labels[index]} />
          ))}
        </svg>
      )}
      {hover && rows[hover.index] && (
        <Tooltip title={labels[hover.index]} left={hover.left} width={PAD.left + plotWidth}
          entries={[...series.map((item, s) => ({ label: item.label, value: format(number(rows[hover.index][item.key])), color: slotColor(item.slot ?? s + 1) })), ...(extra?.(rows[hover.index]) ?? [])]} />
      )}
    </div>
  )
}

export interface BarItem { key: string; label: ReactNode; value: number; display?: string; color?: string }

/** Horizontal bars with the label on the left and the value at the tip. Negative values grow left from a shared zero. */
export function BarList({ items, format, max: forcedMax }: { items: BarItem[]; format: (value: number) => string; max?: number }) {
  if (!items.length) return <p className="pa-chart-empty">所选范围内暂无数据</p>
  const positive = Math.max(0, forcedMax ?? 0, ...items.map(item => item.value))
  const negative = Math.min(0, ...items.map(item => item.value))
  const span = positive - negative || 1
  const zero = (-negative / span) * 100
  return (
    <div className="viz-bars" style={{ ['--viz-zero' as string]: `${zero}%` }}>
      {items.map(item => {
        const widthPercent = (Math.abs(item.value) / span) * 100
        return (
          <div className="viz-bars__row" key={item.key}>
            <span className="viz-bars__label">{item.label}</span>
            <span className="viz-bars__track">
              <i
                className={`viz-bars__fill${item.value < 0 ? ' is-negative' : ''}`}
                style={{ width: `${widthPercent}%`, [item.value < 0 ? 'right' : 'left']: `${zero}%`, background: item.color ?? (item.value < 0 ? 'var(--viz-neg)' : 'var(--viz-1)') }}
              />
            </span>
            <span className="viz-bars__value">{item.display ?? format(item.value)}</span>
          </div>
        )
      })}
    </div>
  )
}

/** A grid of cells shaded on one sequential ramp; the value is always printed. */
export function Heatmap({ rowLabels, colLabels, values, format, caption }: { rowLabels: string[]; colLabels: string[]; values: (number | null)[][]; format: (value: number) => string; caption: string }) {
  if (!rowLabels.length) return <p className="pa-chart-empty">所选范围内暂无数据</p>
  const flat = values.flat().filter((value): value is number => value != null)
  const max = Math.max(0.0001, ...flat)
  return (
    <div className="pa-table-wrap">
      <table className="viz-heatmap" aria-label={caption}>
        <thead><tr><th />{colLabels.map(label => <th key={label}>{label}</th>)}</tr></thead>
        <tbody>
          {rowLabels.map((label, r) => (
            <tr key={label}>
              <th>{label}</th>
              {colLabels.map((col, c) => {
                const value = values[r]?.[c]
                if (value == null) return <td key={col} className="is-empty">—</td>
                const step = Math.min(6, Math.max(1, Math.ceil((value / max) * 6)))
                return <td key={col} className={`viz-heatmap__cell viz-heatmap__cell--${step}`}>{format(value)}</td>
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** Stat-tile trend: recent points in the de-emphasis hue, the current period in the accent. */
export function Sparkline({ values }: { values: number[] }) {
  const points = values.slice(-16)
  if (points.length < 2) return null
  const width = 120, height = 32, gap = 2
  const max = Math.max(1, ...points.map(Math.abs))
  const slot = width / points.length
  const thickness = Math.max(2, Math.min(8, slot - gap))
  return (
    <svg className="viz-spark" viewBox={`0 0 ${width} ${height}`} width={width} height={height} aria-hidden="true">
      {points.map((value, index) => {
        const h = Math.max(value === 0 ? 0 : 1, (Math.abs(value) / max) * (height - 2))
        return <rect key={index} x={index * slot + (slot - thickness) / 2} y={height - h} width={thickness} height={h} rx={1.5} fill={index === points.length - 1 ? 'var(--viz-1)' : 'var(--viz-dim)'} />
      })}
    </svg>
  )
}
