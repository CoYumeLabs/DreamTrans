import { EdgeRegionSelector } from './EdgeRegionSelector'
import { useEffect, useState } from 'react'
import {
  getAvailableModels,
  getUserModelPreferences,
  saveUserModelPreferences,
  type AvailableModel,
  type UserModelPreferences,
} from '../../api'
import { listTermDomains, type TermDomain } from '../../learning'
import { intlLocale, useMessages } from '../../i18n'
import { LocaleSwitch } from '../../i18n/LocaleSwitch'
import { resolveAiPrompt, type UnifiedSettings } from '../hooks/useUnifiedSettings'
import { languageLabel } from '../workspace/languageOptions'
import type { RecorderStatus } from './RecorderBar'
import { Toggle } from './Toggle'

interface SettingsPanelProps {
  allowUserApiKey: boolean
  authenticated: boolean
  ragEnabled: boolean
  settings: UnifiedSettings
  onChange: (patch: Partial<UnifiedSettings>) => void
  /** Closes the sheet and opens the session setup above the transcript. */
  onOpenSessionSetup: () => void
  recorderStatus: RecorderStatus
}

type SettingsTab = 'transcription' | 'learning' | 'ai' | 'general' | 'advanced'

const SETTINGS_TABS: readonly SettingsTab[] = ['transcription', 'learning', 'ai', 'general', 'advanced']

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
  onChange,
  onOpenSessionSetup,
  recorderStatus,
}: SettingsPanelProps) {
  const m = useMessages()
  const s = m.settings
  const nextSessionLocked = recorderStatus !== 'idle'
  const [tab, setTab] = useState<SettingsTab>('transcription')
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

  function modelSelect(
    key: keyof UserModelPreferences,
    purpose: AvailableModel['purpose'],
    label: string,
    lockWhileRecording: boolean,
  ) {
    if (!modelPreferences) return null
    return (
      <label className="dt-field">
        <span>{label}</span>
        <select
          disabled={lockWhileRecording && nextSessionLocked}
          onChange={(event) => void changeAccountModel(key, event.target.value)}
          value={modelPreferences[key]}
        >
          {modelsFor(purpose).map((model) => (
            <option key={model.model_id} value={model.model_id}>
              {model.model_id}{model.is_default ? s.ai.defaultSuffix : ''}
            </option>
          ))}
        </select>
      </label>
    )
  }

  return (
    <div className="dt-settings">
      <div
        aria-label={s.tabsAria}
        className="dt-segmented dt-segmented--full dt-settings__tabs"
        role="tablist"
      >
        {SETTINGS_TABS.map((id) => (
          <button
            aria-selected={tab === id}
            className={tab === id ? 'is-active' : ''}
            key={id}
            onClick={() => setTab(id)}
            role="tab"
            type="button"
          >
            {s.tabs[id]}
          </button>
        ))}
      </div>

      {tab === 'transcription' && (
        <>
          <div className="dt-settings__moved">
            <p>{s.sessionMoved}</p>
            <button
              className="dt-button dt-button--secondary dt-button--small"
              onClick={onOpenSessionSetup}
              type="button"
            >
              {s.openSessionSetup}
            </button>
          </div>
          <div className="dt-settings__edge">
            <EdgeRegionSelector />
          </div>
          <section className="dt-settings__section">
            <div>
              <h3>{s.translation.title}</h3>
              <p className="dt-muted">
                {nextSessionLocked
                  ? s.translation.locked
                  : settings.translationEnabled
                    ? s.translation.applies
                    : s.translation.translationOff}
              </p>
            </div>
            <label className="dt-field">
              <span>{s.translation.engine}</span>
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
                    ? s.translation.engineAi
                    : s.translation.engineAiUnknown}
                </option>
                <option value="speechmatics">{s.translation.engineFast}</option>
              </select>
            </label>
            {!ragEnabled && settings.translationEngine === 'ai' && (
              <p className="dt-muted">
                {s.translation.engineAiUnavailable}
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
        </>
      )}

      {tab === 'learning' && (
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
      )}

      {tab === 'ai' && (
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
                {modelSelect('translation_model', 'translation', s.ai.translationModel, true)}
                {modelSelect('summary_model', 'summary', s.ai.summaryModel, true)}
                {modelSelect('chat_model', 'chat', s.ai.chatModel, false)}
              </div>
              {modelStatus && <p className="dt-muted">{modelStatus}</p>}
            </div>
          )}
          {authenticated && !modelPreferences && modelStatus && (
            <p className="dt-muted">{modelStatus}</p>
          )}
        </section>
      )}

      {tab === 'general' && (
        <>
          <section className="dt-settings__section">
            <div>
              <h3>{s.interfaceLanguage.title}</h3>
              <p className="dt-muted">{s.interfaceLanguage.body}</p>
            </div>
            <LocaleSwitch />
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
        </>
      )}

      {tab === 'advanced' && (
        <>
          <p className="dt-muted">{s.advanced.body}</p>
          <section className="dt-settings__section">
            <div>
              <h3>{s.advanced.promptsTitle}</h3>
              <p className="dt-muted">{s.advanced.promptsBody}</p>
            </div>
            <label className="dt-field">
              <span>{s.translation.prompt}</span>
              <textarea
                disabled={nextSessionLocked || !settings.translationEnabled || settings.translationEngine !== 'ai'}
                maxLength={20_000}
                onChange={(event) => onChange({ translatePrompt: event.target.value })}
                placeholder={`${s.translation.promptPlaceholder} (${languageLabel(settings.sourceLanguage)} → ${languageLabel(settings.targetLanguage)})`}
                rows={4}
                value={settings.translatePrompt}
              />
            </label>
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
          </section>
          {allowUserApiKey && (
            <section className="dt-settings__section">
              <div>
                <h3>{s.advanced.byokTitle}</h3>
                <p className="dt-muted">{s.ai.byokBody}</p>
              </div>
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
            </section>
          )}
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
        </>
      )}
    </div>
  )
}
