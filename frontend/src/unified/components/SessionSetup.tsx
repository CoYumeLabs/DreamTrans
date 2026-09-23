import { useCallback, useRef } from 'react'
import type { AudioCaptureSource } from '../../core/audio/BrowserAudioCapture'
import { useMessages } from '../../i18n'
import type { UnifiedSettings } from '../hooks/useUnifiedSettings'
import { usePopoverDismiss } from '../hooks/usePopoverDismiss'
import { edgeRegionLabel } from '../workspace/edgeRegions'
import { audioSourceLabel, languageLabel, languageOptions } from '../workspace/languageOptions'
import { Icon } from './Icon'
import { Toggle } from './Toggle'

const AUDIO_SOURCES: readonly AudioCaptureSource[] = ['microphone', 'system', 'mixed']

interface SessionSetupProps {
  /**
   * `toolbar` sits above the transcript and owns the popover. `empty` is the
   * chip row in the empty state; it only opens the toolbar popover, because
   * the empty state scrolls and would clip a panel anchored inside it.
   */
  variant: 'toolbar' | 'empty'
  open: boolean
  /** A session is running, so its capture and language setup are fixed. */
  locked: boolean
  settings: UnifiedSettings
  /** Node regions this account may pick; empty hides the picker. */
  edgeRegions: readonly string[]
  /** `auto` or one of `edgeRegions`. */
  edgeRegion: string
  onEdgeRegionChange: (region: string) => void
  onOpenChange: (open: boolean) => void
  onChange: (patch: Partial<UnifiedSettings>) => void
  onOpenSettings: () => void
}

/**
 * The per-session choices (audio source, spoken language, live translation
 * and its target) edited in place, next to the transcript they affect,
 * instead of inside the settings sheet.
 */
export function SessionSetup({
  variant,
  open,
  locked,
  settings,
  edgeRegions,
  edgeRegion,
  onEdgeRegionChange,
  onOpenChange,
  onChange,
  onOpenSettings,
}: SessionSetupProps) {
  const m = useMessages()
  const setup = m.workspace.setup
  const feed = m.workspace.feed
  const rootRef = useRef<HTMLDivElement>(null)
  const close = useCallback(() => onOpenChange(false), [onOpenChange])
  usePopoverDismiss(rootRef, open && variant === 'toolbar', close)

  const audio = audioSourceLabel(settings.audioSource)
  const languages = settings.translationEnabled
    ? `${languageLabel(settings.sourceLanguage)} → ${languageLabel(settings.targetLanguage)}`
    : `${languageLabel(settings.sourceLanguage)} · ${setup.originalOnly}`
  const pinnedRegion = edgeRegions.length > 0 && edgeRegion !== 'auto' ? edgeRegionLabel(edgeRegion) : ''

  return (
    <div className={`dt-session-setup dt-session-setup--${variant}`} ref={rootRef}>
      {variant === 'toolbar' ? (
        <button
          aria-expanded={open}
          aria-haspopup="dialog"
          className="dt-session-setup__trigger"
          data-tour="session-setup"
          onClick={() => onOpenChange(!open)}
          title={locked ? setup.locked : setup.open}
          type="button"
        >
          <Icon name="mic" size={15} />
          <span className="dt-session-setup__summary">
            <span>{audio}</span>
            <span aria-hidden="true">·</span>
            <span>{languages}</span>
            {pinnedRegion && (
              <>
                <span aria-hidden="true">·</span>
                <span>{pinnedRegion}</span>
              </>
            )}
          </span>
          <Icon className="dt-session-setup__chevron" name="arrow-down" size={14} />
        </button>
      ) : (
        <button
          aria-expanded={open}
          aria-haspopup="dialog"
          className="dt-feed-empty__setup"
          onClick={() => onOpenChange(true)}
          title={setup.open}
          type="button"
        >
          <span className="dt-feed-empty__chip">
            <span>{feed.audio}</span>
            <strong>{audio}</strong>
          </span>
          <span className="dt-feed-empty__chip">
            <span>{feed.language}</span>
            <strong>{languages}</strong>
            <Icon className="dt-feed-empty__chevron" name="arrow-down" size={13} />
          </span>
        </button>
      )}

      {open && variant === 'toolbar' && (
        <div aria-label={setup.title} className="dt-session-setup__panel" role="dialog">
          <header>
            <strong>{setup.title}</strong>
            <small>{locked ? setup.locked : setup.applies}</small>
          </header>
          <label className="dt-field">
            <span>{setup.audio}</span>
            <select
              disabled={locked}
              onChange={(event) => {
                const value = event.target.value
                const source = AUDIO_SOURCES.find((item) => item === value)
                if (source) onChange({ audioSource: source })
              }}
              value={settings.audioSource}
            >
              {AUDIO_SOURCES.map((source) => (
                <option key={source} value={source}>{audioSourceLabel(source)}</option>
              ))}
            </select>
          </label>
          {settings.audioSource !== 'microphone' && (
            <p className="dt-session-setup__hint">
              {setup.shareHintBefore} <strong>{setup.shareHintStrong}</strong> {setup.shareHintAfter}
            </p>
          )}
          <label className="dt-field">
            <span>{setup.source}</span>
            <select
              disabled={locked}
              onChange={(event) => onChange({ sourceLanguage: event.target.value })}
              value={settings.sourceLanguage}
            >
              {languageOptions().map((language) => (
                <option key={language.value} value={language.value}>{language.label}</option>
              ))}
            </select>
          </label>
          <Toggle
            checked={settings.translationEnabled}
            description={setup.translateBody}
            disabled={locked}
            label={setup.translate}
            onChange={(translationEnabled) => onChange({ translationEnabled })}
          />
          <label className="dt-field">
            <span>{setup.target}</span>
            <select
              disabled={locked || !settings.translationEnabled}
              onChange={(event) => onChange({ targetLanguage: event.target.value })}
              value={settings.targetLanguage}
            >
              {languageOptions().map((language) => (
                <option key={language.value} value={language.value}>{language.label}</option>
              ))}
            </select>
          </label>
          {edgeRegions.length > 0 && (
            <label className="dt-field">
              <span>{setup.region}</span>
              <select
                disabled={locked}
                onChange={(event) => onEdgeRegionChange(event.target.value)}
                value={edgeRegion}
              >
                <option value="auto">{setup.regionAuto}</option>
                {edgeRegions.map((region) => (
                  <option key={region} value={region}>{edgeRegionLabel(region)}</option>
                ))}
              </select>
              <small>{setup.regionHint}</small>
            </label>
          )}
          <footer>
            <button
              className="dt-button dt-button--text dt-button--small"
              onClick={() => {
                close()
                onOpenSettings()
              }}
              type="button"
            >
              {setup.moreSettings}
            </button>
            <button
              className="dt-button dt-button--primary dt-button--small"
              onClick={close}
              type="button"
            >
              {setup.done}
            </button>
          </footer>
        </div>
      )}
    </div>
  )
}
