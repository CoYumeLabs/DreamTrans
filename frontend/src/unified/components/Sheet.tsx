import { useEffect, useRef, type ReactNode } from 'react'
import { useMessages } from '../../i18n'
import { Icon } from './Icon'

interface SheetProps {
  children: ReactNode
  description?: string
  eyebrow?: string
  onClose: () => void
  open: boolean
  title: string
  wide?: boolean
}

export function Sheet({
  children,
  description,
  eyebrow,
  onClose,
  open,
  title,
  wide = false,
}: SheetProps) {
  const m = useMessages()
  const dialogRef = useRef<HTMLElement>(null)

  useEffect(() => {
    if (!open) return
    const previous = document.body.style.overflow
    const previouslyFocused = document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null
    document.body.style.overflow = 'hidden'
    const focusableElements = () => {
      const dialog = dialogRef.current
      if (!dialog) return []
      return [...dialog.querySelectorAll<HTMLElement>(
        'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      )].filter((element) => (
        element.getClientRects().length > 0
        && window.getComputedStyle(element).visibility !== 'hidden'
      ))
    }
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onClose()
        return
      }
      if (event.key !== 'Tab') return
      const dialog = dialogRef.current
      if (!dialog) return
      const focusable = focusableElements()
      if (focusable.length === 0) {
        event.preventDefault()
        dialog.focus()
        return
      }
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last?.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first?.focus()
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    const focusFrame = window.requestAnimationFrame(() => {
      const dialog = dialogRef.current
      const first = focusableElements()[0]
      ;(first ?? dialog)?.focus()
    })
    return () => {
      window.cancelAnimationFrame(focusFrame)
      document.body.style.overflow = previous
      window.removeEventListener('keydown', handleKeyDown)
      previouslyFocused?.focus()
    }
  }, [onClose, open])

  if (!open) return null

  return (
    <div className="dt-sheet-backdrop" onMouseDown={(event) => {
      if (event.currentTarget === event.target) onClose()
    }}>
      <section
        ref={dialogRef}
        aria-label={title}
        aria-modal="true"
        className={`dt-sheet${wide ? ' dt-sheet--wide' : ''}`}
        role="dialog"
        tabIndex={-1}
      >
        <div className="dt-sheet__handle" />
        <header className="dt-sheet__header">
          <div>
            {eyebrow && <p className="dt-eyebrow">{eyebrow}</p>}
            <h2>{title}</h2>
            {description && <p className="dt-muted">{description}</p>}
          </div>
          <button
            aria-label={m.common.close}
            className="dt-icon-button"
            onClick={onClose}
            type="button"
          >
            <Icon name="close" />
          </button>
        </header>
        <div className="dt-sheet__body">{children}</div>
      </section>
    </div>
  )
}
