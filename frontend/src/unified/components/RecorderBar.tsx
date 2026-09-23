import { useMessages } from '../../i18n'
import { Icon } from './Icon'

export type RecorderStatus =
  | 'idle'
  | 'starting'
  | 'recording'
  | 'paused'
  | 'stopping'
  | 'reconnecting'
  | 'error'

interface RecorderBarProps {
  assistantEnabled: boolean
  canContinue: boolean
  durationLabel: string
  onAssistant: () => void
  onContinue: () => void
  onInsights: () => void
  onPauseToggle: () => void
  onStart: () => void
  onStop: () => void
  status: RecorderStatus
}

export function RecorderBar({
  assistantEnabled,
  canContinue,
  durationLabel,
  onAssistant,
  onContinue,
  onInsights,
  onPauseToggle,
  onStart,
  onStop,
  status,
}: RecorderBarProps) {
  const m = useMessages()
  const active = status === 'recording'
    || status === 'paused'
    || status === 'reconnecting'
    || status === 'error'
  const canStop = active || status === 'starting'
  const busy = status === 'stopping'

  return (
    <div className="dt-recorder-shell">
      <div className="dt-recorder" role="toolbar" aria-label={m.recorder.toolbar}>
        <button
          aria-label={m.recorder.openAssistant}
          className="dt-recorder__utility"
          data-tour="assistant"
          disabled={!assistantEnabled}
          onClick={onAssistant}
          title={assistantEnabled ? undefined : m.workspace.hints.aiUnavailable}
          type="button"
        >
          <Icon name="sparkles" />
          <span>{m.recorder.ai}</span>
        </button>

        <div className="dt-recorder__primary">
          {!active && !busy && canContinue && (
            <button
              aria-label={m.recorder.continueSession}
              className="dt-recorder__secondary-action"
              onClick={onContinue}
              title={m.recorder.continueSession}
              type="button"
            >
              <Icon name="play" />
            </button>
          )}
          {active && status !== 'error' && (
            <button
              aria-label={status === 'paused' ? m.recorder.resume : m.recorder.pause}
              className="dt-recorder__secondary-action"
              onClick={onPauseToggle}
              type="button"
            >
              <Icon name={status === 'paused' ? 'play' : 'pause'} />
            </button>
          )}

          <button
            aria-label={
              status === 'starting'
                ? m.recorder.cancelStart
                : active
                  ? m.recorder.stop
                  : m.recorder.start
            }
            className={`dt-record-button${active ? ' is-recording' : ''}`}
            data-tour="record"
            disabled={busy}
            onClick={canStop ? onStop : onStart}
            type="button"
          >
            {busy
              ? <span className="dt-spinner" />
              : <Icon name={canStop ? 'stop' : 'mic'} size={25} />}
            {status === 'recording' && <span aria-hidden="true" className="dt-record-button__pulse" />}
          </button>

          {active && (
            <div className="dt-recorder__time" aria-live="off">
              <span className="dt-live-dot" />
              <strong>{durationLabel}</strong>
            </div>
          )}
        </div>

        <button
          aria-label={m.recorder.openInsights}
          className="dt-recorder__utility"
          onClick={onInsights}
          type="button"
        >
          <Icon name="wave" />
          <span>{m.recorder.insights}</span>
        </button>
      </div>
    </div>
  )
}
