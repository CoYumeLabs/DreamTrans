import { useCallback, useRef, useState } from 'react'
import { useMessages } from '../../i18n'
import { usePopoverDismiss } from '../hooks/usePopoverDismiss'
import { Icon, type IconName } from './Icon'

type TextMode = 'original' | 'translation' | 'bilingual'

interface ExportMenuProps {
  onDownloadAudio: () => Promise<void>
  onDownloadText: (mode: TextMode) => Promise<void>
}

/** The download button in the top bar and the exports it offers. */
export function ExportMenu({ onDownloadAudio, onDownloadText }: ExportMenuProps) {
  const m = useMessages()
  const w = m.workspace
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const rootRef = useRef<HTMLDivElement>(null)
  const close = useCallback(() => setOpen(false), [])
  usePopoverDismiss(rootRef, open, close)

  const items: Array<{ id: string; icon: IconName; label: string; description: string; run: () => Promise<void> }> = [
    { id: 'bilingual', icon: 'message', ...w.exports.bilingual, run: () => onDownloadText('bilingual') },
    { id: 'original', icon: 'archive', ...w.exports.original, run: () => onDownloadText('original') },
    { id: 'translation', icon: 'language', ...w.exports.translation, run: () => onDownloadText('translation') },
    { id: 'audio', icon: 'download', ...w.exports.audio, run: onDownloadAudio },
  ]

  return (
    <div className="dt-export-menu" ref={rootRef}>
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        aria-label={w.hints.downloads}
        className="dt-icon-button"
        onClick={() => setOpen((value) => !value)}
        title={w.hints.downloads}
        type="button"
      >
        <Icon name="download" />
      </button>
      {open && (
        <div aria-label={w.hints.downloads} className="dt-export-menu__panel" role="menu">
          {items.map((item) => (
            <button
              className="dt-export-item"
              disabled={busy !== null}
              key={item.id}
              onClick={() => {
                setBusy(item.id)
                void item.run().finally(() => {
                  setBusy(null)
                  setOpen(false)
                })
              }}
              role="menuitem"
              type="button"
            >
              <span className="dt-export-item__icon">
                {busy === item.id ? <span className="dt-spinner" /> : <Icon name={item.icon} size={18} />}
              </span>
              <span>
                <strong>{item.label}</strong>
                <small>{item.description}</small>
              </span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
