import { useEffect, type RefObject } from 'react'

/**
 * Closes an anchored popover on Escape or on a pointer press outside `ref`,
 * which wraps both the trigger and the panel so the trigger can toggle.
 */
export function usePopoverDismiss(
  ref: RefObject<HTMLElement | null>,
  open: boolean,
  onClose: () => void,
): void {
  useEffect(() => {
    if (!open) return
    const handlePointer = (event: PointerEvent) => {
      if (ref.current && event.target instanceof Node && !ref.current.contains(event.target)) {
        onClose()
      }
    }
    const handleKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.stopPropagation()
        onClose()
      }
    }
    document.addEventListener('pointerdown', handlePointer)
    document.addEventListener('keydown', handleKey)
    return () => {
      document.removeEventListener('pointerdown', handlePointer)
      document.removeEventListener('keydown', handleKey)
    }
  }, [ref, open, onClose])
}
