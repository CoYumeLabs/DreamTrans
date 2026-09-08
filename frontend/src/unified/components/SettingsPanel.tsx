import { useEffect, useState } from 'react'
import {
  getAvailableModels,
  getUserModelPreferences,
  saveUserModelPreferences,
  type AvailableModel,
  type TrainingProgramInfo,
  type RouteDecision,
  type UserModelPreferences,
} from '../../api'
import { listTermDomains, type TermDomain } from '../../learning'
import { intlLocale, useMessages } from '../../i18n'
import { LocaleSwitch } from '../../i18n/LocaleSwitch'
import { resolveAiPrompt, type UnifiedSettings } from '../hooks/useUnifiedSettings'
import { languageLabel, languageOptions } from '../workspace/languageOptions'
import type { RecorderStatus } from './RecorderBar'

interface SettingsPanelProps {
  allowUserApiKey: boolean
  authenticated: boolean
  ragEnabled: boolean
  settings: UnifiedSettings
  trainingRoute?: RouteDecision
  trainingProgram: TrainingProgramInfo
  /** The account's training-programme answer; null until asked. */
  trainingOptIn: boolean | null
  onChange: (patch: Partial<UnifiedSettings>) => void
  /** Replays the first-run setup wizard and interface tour. */
  onReplayOnboarding?: () => void
  onTrainingOptInChange: (optIn: boolean) => Promise<boolean>
  recorderStatus: RecorderStatus
}

const DOMAIN_UI: Record<TermDomain, { mark: string; tone: string }> = {
  ai: { mark: 'AI', tone: 'indigo' },
  internet: { mark: 'Web', tone: 'sky' },
  psychology: { mark: 'Psi', tone: 'violet' },
  geography: { mark: 'Geo', tone: 'teal' },
  biology: { mark: 'Bio', tone: 'green' },
}

export function SettingsPanel({
  allowUserApiKey,
  authenticated,
  ragEnabled,
  settings,
  trainingRoute,
  trainingProgram,
  trainingOptIn,
  onChange,
  onReplayOnboarding,
  onTrainingOptInChange,
  recorderStatus,
}: SettingsPanelProps) {
  const m = useMessages()
  const s = m.settings
  const nextSessionLocked = recorderStatus !== 'idle'
  const [trainingStatus, setTrainingStatus] = useState('')

  async function changeTrainingOptIn(optIn: boolean) {
    setTrainingStatus(s.training.saving)
    const saved = await onTrainingOptInChange(optIn)
    setTrainingStatus(saved ? s.training.saved(optIn) : s.training.saveFailed)
  }
  const [availableModels, setAvailableModels] = useState<AvailableModel[]>([])
  const [modelPreferences, setModelPreferences] = useState<UserModelPreferences | null>(null)
  const [modelStatus, setModelStatus] = useState('')

  useEffect(() => {
    if (!authenticated) {
      setAvailableModels([])
      setModelPreferences(null)
      return
    }
    let active = true
    void Promise.all([getAvailableModels(), getUserModelPreferences()])
      .then(([models, preferences]) => {
        if (!active) return
        setAvailableModels(models)
        setModelPreferences(preferences)
        setModelStatus('')
      })
      .catch(() => {
        if (active) setModelStatus(s.ai.modelsLoadFailed)
      })
    return () => { active = false }
  }, [authenticated, s.ai.modelsLoadFailed])

  async function changeAccountModel(
    key: keyof UserModelPreferences,
    model: string,
  ) {
    if (!modelPreferences) return
    const next = { ...modelPreferences, [key]: model }
    setModelPreferences(next)
    setModelStatus(s.ai.saving)
    try {
      const saved = await saveUserModelPreferences(next)
      setModelPreferences(saved)
      setModelStatus(s.ai.saved)
    } catch (reason) {
      setModelPreferences(modelPreferences)
      setModelStatus(reason instanceof Error ? reason.message : s.ai.saveFailed)
    }
  }

  function modelsFor(purpose: AvailableModel['purpose']) {
    return availableModels.filter((model) => model.purpose === purpose)
  }

  return (
    <div className="dt-settings">
      <section className="dt-settings__section">
        <div>
          <h3>{s.interfaceLanguage.title}</h3>
          <p className="dt-muted">{s.interfaceLanguage.body}</p>
        </div>
        <LocaleSwitch />
      </section>

      <section className="dt-settings__section">
        <div>
          <h3>{s.languageSection.title}</h3>
          <p className="dt-muted">
            {nextSessionLocked
              ? s.languageSection.locked
              : s.languageSection.applies}
          </p>
        </div>
        <div className="dt-settings__grid">
          <label className="dt-field">
            <span>{s.languageSection.source}</span>
            <select
              disabled={nextSessionLocked}
              onChange={(event) => onChange({ sourceLanguage: event.target.value })}
              value={settings.sourceLanguage}
            >
              {languageOptions().map((language) => (
                <option key={language.value} value={language.value}>{language.label}</option>
              ))}
            </select>
          </label>
          <label className="dt-field">
            <span>{s.languageSection.target}</span>
            <select
              disabled={nextSessionLocked || !settings.translationEnabled}
              onChange={(event) => onChange({ targetLanguage: event.target.value })}
              value={settings.targetLanguage}
            >
              {languageOptions().map((language) => (
                <option key={language.value} value={language.value}>{language.label}</option>
              ))}
            </select>
          </label>
        </div>
        <p className="dt-muted">
          {s.languageSection.modesHint}
        </p>
        <Toggle
          checked={settings.translationEnabled}
          description={s.languageSection.enableTranslationBody}
          disabled={nextSessionLocked}
          label={s.languageSection.enableTranslation}
          onChange={(translationEnabled) => onChange({ translationEnabled })}
        />
        <label className="dt-field">
          <span>{s.languageSection.engine}</span>
          <select
            disabled={nextSessionLocked || !settings.translationEnabled}
            onChange={(event) => onChange({
              translationEngine: event.target.value === 'speechmatics'
                ? 'speechmatics'
                : 'ai',
            })}
            value={settings.translationEngine}
          >
            <option value="ai">
              {ragEnabled
                ? s.languageSection.engineAi
                : s.languageSection.engineAiUnknown}
            </option>
            <option value="speechmatics">{s.languageSection.engineFast}</option>
          </select>
        </label>
        {!ragEnabled && settings.translationEngine === 'ai' && (
          <p className="dt-muted">
            {s.languageSection.engineAiUnavailable}
          </p>
        )}
        {settings.translationEngine === 'ai' && (
          <label className="dt-field">
            <span>{s.languageSection.prompt}</span>
            <textarea
              disabled={nextSessionLocked || !settings.translationEnabled}
              maxLength={20_000}
              onChange={(event) => onChange({ translatePrompt: event.target.value })}
              placeholder={`${s.languageSection.promptPlaceholder} (${languageLabel(settings.sourceLanguage)} → ${languageLabel(settings.targetLanguage)})`}
              rows={4}
              value={settings.translatePrompt}
            />
          </label>
        )}
      </section>

      <section className="dt-settings__section">
        <div>
          <h3>{s.learning.title}</h3>
          <p className="dt-muted">{s.learning.body}</p>
        </div>
        <label className="dt-field">
          <span>{s.learning.level}</span>
          <select
            onChange={(event) => {
              const value = event.target.value
              onChange({
                learningLevel: value === 'A2' || value === 'B2' ? value : 'B1',
              })
            }}
            value={settings.learningLevel}
          >
            <option value="A2">{s.learning.levels.A2}</option>
            <option value="B1">{s.learning.levels.B1}</option>
            <option value="B2">{s.learning.levels.B2}</option>
          </select>
        </label>

        <div className="dt-domain-picker">
          <div className="dt-domain-picker__head">
            <div>
              <span className="dt-domain-picker__title">{s.learning.domainsTitle}</span>
              <p className="dt-domain-picker__desc">
                {s.learning.domainsBody}
              </p>
            </div>
            <div className="dt-domain-picker__actions">
              <button
                className="dt-domain-picker__link"
                type="button"
                onClick={() => onChange({
                  learningDomains: listTermDomains().map((item) => item.id),
                })}
              >
                {s.learning.selectAll}
              </button>
              <button
                className="dt-domain-picker__link"
                type="button"
                onClick={() => onChange({ learningDomains: [] })}
              >
                {s.learning.clear}
              </button>
            </div>
          </div>
          <div className="dt-domain-picker__grid" role="group" aria-label={s.learning.domainsTitle}>
            {listTermDomains().map((domain) => {
              const checked = settings.learningDomains.includes(domain.id)
              const ui = DOMAIN_UI[domain.id]
              return (
                <label
                  key={domain.id}
                  className={
                    checked
                      ? 'dt-domain-card is-selected'
                      : 'dt-domain-card'
                  }
                  data-tone={ui.tone}
                >
                  <input
                    className="dt-domain-card__input"
                    checked={checked}
                    type="checkbox"
                    onChange={() => {
                      const next: TermDomain[] = checked
                        ? settings.learningDomains.filter((id) => id !== domain.id)
                        : [...settings.learningDomains, domain.id]
                      onChange({ learningDomains: next })
                    }}
                  />
                  <span className="dt-domain-card__mark" aria-hidden="true">
                    {ui.mark}
                  </span>
                  <span className="dt-domain-card__body">
                    <strong>{s.learning.domains[domain.id].title}</strong>
                    <small>{s.learning.domains[domain.id].blurb}</small>
                  </span>
                  <span className="dt-domain-card__meta">
                    <span className="dt-domain-card__count">
                      {domain.termCount.toLocaleString(intlLocale())}
                      <em>{s.learning.words}</em>
                    </span>
                    <span
                      className={
                        checked
                          ? 'dt-domain-card__check is-on'
                          : 'dt-domain-card__check'
                      }
                      aria-hidden="true"
                    >
                      {checked ? '✓' : ''}
                    </span>
                  </span>
                </label>
              )
            })}
          </div>
          <p className="dt-domain-picker__footnote">
            {s.learning.enabledCount(settings.learningDomains.length, listTermDomains().length)}
          </p>
        </div>
      </section>

      <section className="dt-settings__section">
        <div>
          <h3>{s.debug.title}</h3>
          <p className="dt-muted">{s.debug.body}</p>
        </div>
        <Toggle
          checked={settings.debugTransport}
          description={s.debug.transportBody}
          label={s.debug.transport}
          onChange={(debugTransport) => onChange({ debugTransport })}
        />
      </section>

      <section className="dt-settings__section">
        <div>
          <h3>{s.reading.title}</h3>
          <p className="dt-muted">{s.reading.body}</p>
        </div>
        <Toggle
          checked={settings.autoScroll}
          description={s.reading.autoScrollBody}
          label={s.reading.autoScroll}
          onChange={(autoScroll) => onChange({ autoScroll })}
        />
        <Toggle
          checked={settings.reducedEffects}
          description={s.reading.reducedEffectsBody}
          label={s.reading.reducedEffects}
          onChange={(reducedEffects) => onChange({ reducedEffects })}
        />
      </section>

      <section className="dt-settings__section">
        <div>
          <h3>{s.audio.title}</h3>
          <p className="dt-muted">
            {nextSessionLocked
              ? s.audio.locked
              : s.audio.body}
          </p>
        </div>
        <label className="dt-field">
          <span>{s.audio.source}</span>
          <select
            disabled={nextSessionLocked}
            onChange={(event) => {
              const value = event.target.value
              onChange({
                audioSource:
                  value === 'system' || value === 'mixed' || value === 'microphone'
                    ? value
                    : 'microphone',
              })
            }}
            value={settings.audioSource}
          >
            <option value="microphone">{s.audio.microphone}</option>
            <option value="system">{s.audio.system}</option>
            <option value="mixed">{s.audio.mixed}</option>
          </select>
        </label>
        {settings.audioSource !== 'microphone' && (
          <p className="dt-muted">
            {s.audio.shareHintBefore} <strong>{s.audio.shareHintStrong}</strong> {s.audio.shareHintAfter}
          </p>
        )}
      </section>

      <section className="dt-settings__section">
        <div>
          <h3>{s.localData.title}</h3>
          <p className="dt-muted">
            {nextSessionLocked
              ? s.localData.locked
              : s.localData.body}
          </p>
        </div>
        <Toggle
          checked={settings.keepLocalAudio}
          description={s.localData.keepAudioBody}
          disabled={nextSessionLocked}
          label={s.localData.keepAudio}
          onChange={(keepLocalAudio) => onChange({ keepLocalAudio })}
        />
      </section>

      {authenticated && (trainingProgram.available || trainingRoute?.reason === 'program_off' || trainingRoute?.reason === 'single_account') && (
        <section className="dt-settings__section">
          <div>
            <h3>{s.training.title}</h3>
            <p className="dt-muted">
              {trainingRoute?.reason === 'single_account' ? s.training.routeSingleAccount : s.training.body(trainingProgram.discountPercent)}
              {' '}
              <a href="/privacy#share" rel="noreferrer" target="_blank">{s.training.privacyLink}</a>
            </p>
          </div>
          {trainingRoute && <p className="dt-muted" role="status">{
            trainingRoute.reason === 'program_off' ? s.training.routeOff
              : trainingRoute.reason === 'institution' ? s.training.routeInstitution
                : trainingRoute.reason === 'tenant_pinned' || trainingRoute.reason === 'user_pinned' ? s.training.routePinned
                  : trainingRoute.gift_funded ? s.training.routeGift : ''
          }</p>}
          {trainingProgram.available && <Toggle
            checked={trainingOptIn === true}
            description={trainingOptIn === null ? s.training.unanswered : s.training.toggleBody}
            label={s.training.toggle(trainingProgram.discountPercent)}
            onChange={(optIn) => void changeTrainingOptIn(optIn)}
          />}
          {trainingStatus && <p className="dt-muted" role="status">{trainingStatus}</p>}
        </section>
      )}

      <section className="dt-settings__section">
        <div>
          <h3>{s.ai.title}</h3>
          <p className="dt-muted">
            {ragEnabled
              ? s.ai.body
              : s.ai.unavailable}
          </p>
        </div>
        <Toggle
          checked={settings.automaticAiIngest}
          description={s.ai.autoIngestBody}
          disabled={!ragEnabled}
          label={s.ai.autoIngest}
          onChange={(automaticAiIngest) => onChange({ automaticAiIngest })}
        />
        {authenticated && modelPreferences && (
          <div className="dt-settings__section">
            <div>
              <h3>{s.ai.modelsTitle}</h3>
              <p className="dt-muted">{s.ai.modelsBody}</p>
            </div>
            <div className="dt-settings__grid">
              <label className="dt-field">
                <span>{s.ai.translationModel}</span>
                <select
                  disabled={nextSessionLocked}
                  onChange={(event) => void changeAccountModel('translation_model', event.target.value)}
                  value={modelPreferences.translation_model}
                >
                  {modelsFor('translation').map((model) => (
                    <option key={model.model_id} value={model.model_id}>
                      {model.model_id}{model.is_default ? s.ai.defaultSuffix : ''}
                    </option>
                  ))}
                </select>
              </label>
              <label className="dt-field">
                <span>{s.ai.summaryModel}</span>
                <select
                  disabled={nextSessionLocked}
                  onChange={(event) => void changeAccountModel('summary_model', event.target.value)}
                  value={modelPreferences.summary_model}
                >
                  {modelsFor('summary').map((model) => (
                    <option key={model.model_id} value={model.model_id}>
                      {model.model_id}{model.is_default ? s.ai.defaultSuffix : ''}
                    </option>
                  ))}
                </select>
              </label>
              <label className="dt-field">
                <span>{s.ai.chatModel}</span>
                <select
                  onChange={(event) => void changeAccountModel('chat_model', event.target.value)}
                  value={modelPreferences.chat_model}
                >
                  {modelsFor('chat').map((model) => (
                    <option key={model.model_id} value={model.model_id}>
                      {model.model_id}{model.is_default ? s.ai.defaultSuffix : ''}
                    </option>
                  ))}
                </select>
              </label>
            </div>
            {modelStatus && <p className="dt-muted">{modelStatus}</p>}
          </div>
        )}
        {authenticated && !modelPreferences && modelStatus && (
          <p className="dt-muted">{modelStatus}</p>
        )}
        <label className="dt-field">
          <span>{s.ai.prompt}</span>
          <textarea
            disabled={!ragEnabled}
            maxLength={20_000}
            onChange={(event) => onChange({ aiPrompt: event.target.value })}
            rows={4}
            value={resolveAiPrompt(settings.aiPrompt)}
          />
        </label>
        {allowUserApiKey && (
          <>
            <p className="dt-muted">
              {s.ai.byokBody}
            </p>
            <label className="dt-field">
              <span>API Key</span>
              <input
                autoComplete="off"
                maxLength={4_096}
                onChange={(event) => onChange({ aiApiKey: event.target.value })}
                placeholder={s.ai.byokPlaceholder}
                type="password"
                value={settings.aiApiKey}
              />
            </label>
            <div className="dt-settings__grid">
              <label className="dt-field">
                <span>API Base</span>
                <input
                  disabled={!settings.aiApiKey}
                  maxLength={2_048}
                  onChange={(event) => onChange({ aiApiBase: event.target.value })}
                  placeholder="https://api.example.com/v1"
                  type="url"
                  value={settings.aiApiBase}
                />
              </label>
              <label className="dt-field">
                <span>Chat Model</span>
                <select
                  disabled={!settings.aiApiKey}
                  onChange={(event) => onChange({ aiModel: event.target.value })}
                  value={settings.aiModel || modelPreferences?.chat_model || ''}
                >
                  {modelsFor('chat').map((model) => (
                    <option key={model.model_id} value={model.model_id}>
                      {model.model_id}{model.is_default ? s.ai.defaultSuffix : ''}
                    </option>
                  ))}
                </select>
              </label>
            </div>
          </>
        )}
      </section>

      {onReplayOnboarding && (
        <section className="dt-settings__section">
          <div>
            <h3>{s.help.title}</h3>
            <p className="dt-muted">{s.help.body}</p>
          </div>
          <button
            className="dt-button dt-button--secondary"
            disabled={nextSessionLocked}
            onClick={onReplayOnboarding}
            title={nextSessionLocked ? s.help.replayLocked : undefined}
            type="button"
          >
            {s.help.replay}
          </button>
        </section>
      )}
    </div>
  )
}

interface ToggleProps {
  checked: boolean
  description: string
  disabled?: boolean
  label: string
  onChange: (checked: boolean) => void
}

function Toggle({
  checked,
  description,
  disabled = false,
  label,
  onChange,
}: ToggleProps) {
  return (
    <label className={`dt-toggle${disabled ? ' is-disabled' : ''}`}>
      <span>
        <strong>{label}</strong>
        <small>{description}</small>
      </span>
      <input
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
        type="checkbox"
      />
      <span aria-hidden="true" className="dt-toggle__track">
        <span />
      </span>
    </label>
  )
}
