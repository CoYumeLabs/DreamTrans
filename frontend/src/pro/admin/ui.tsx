import type { ReactNode } from 'react'
import { formatInteger } from './shared'

export function ErrorBanner({ message, onClose }: { message: string; onClose?: () => void }) {
  if (!message) return null
  return (
    <div className="pa-banner pa-banner--error" role="alert">
      <span>{message}</span>
      {onClose && <button onClick={onClose} type="button">关闭</button>}
    </div>
  )
}

export function Modal({
  title,
  children,
  footer,
  onClose,
  danger = false,
  wide = false,
}: {
  title: string
  children: ReactNode
  footer: ReactNode
  onClose: () => void
  danger?: boolean
  wide?: boolean
}) {
  return (
    <div className="pa-modal-backdrop" onMouseDown={(event) => {
      if (event.currentTarget === event.target) onClose()
    }}>
      <section
        aria-modal="true"
        className={`pa-modal ${danger ? 'pa-modal--danger' : ''} ${wide ? 'pa-modal--wide' : ''}`}
        role="dialog"
      >
        <header>
          <h2>{title}</h2>
          <button aria-label="关闭" className="pa-modal__close" onClick={onClose} type="button">×</button>
        </header>
        <div className="pa-modal__body">{children}</div>
        <footer>{footer}</footer>
      </section>
    </div>
  )
}

export function Pagination({
  page,
  pageSize,
  total,
  onChange,
}: {
  page: number
  pageSize: number
  total: number
  onChange: (page: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="pa-pagination">
      <span>第 {page} / {pages} 页，共 {formatInteger(total)} 条</span>
      <div>
        <button disabled={page <= 1} onClick={() => onChange(page - 1)} type="button">上一页</button>
        <button disabled={page >= pages} onClick={() => onChange(page + 1)} type="button">下一页</button>
      </div>
    </div>
  )
}

export function Metric({
  label,
  value,
  loading,
  hint,
  children,
}: {
  label: string
  value: ReactNode
  loading?: boolean
  hint?: ReactNode
  children?: ReactNode
}) {
  return (
    <article className={`pa-card pa-metric${children ? ' pa-kpi' : ''}`}>
      <small>{label}</small>
      {loading ? <span className="pa-skeleton pa-skeleton--value" /> : <strong>{value}</strong>}
      {hint && <p>{hint}</p>}
      {children}
    </article>
  )
}

export function SubTabs<T extends string>({
  items,
  value,
  onChange,
}: {
  items: Array<{ id: T; label: string; count?: number }>
  value: T
  onChange: (next: T) => void
}) {
  return (
    <div className="pa-subtabs" role="tablist">
      {items.map((item) => (
        <button
          aria-selected={item.id === value}
          className={item.id === value ? 'is-active' : ''}
          key={item.id}
          onClick={() => onChange(item.id)}
          role="tab"
          type="button"
        >
          {item.label}
          {item.count !== undefined && <span>{formatInteger(item.count)}</span>}
        </button>
      ))}
    </div>
  )
}

export function MemberBadge({
  planCode,
  planName,
  active,
}: {
  planCode: string
  planName?: string
  active: boolean
}) {
  const label = planName || planCode || 'free'
  if (planCode === 'free' || !planCode) return <span className="pa-pill">{label}</span>
  return (
    <span className={`pa-pill pa-pill--member ${active ? 'is-active' : 'is-expired'}`}>
      {label}{active ? '' : ' · 已到期'}
    </span>
  )
}
