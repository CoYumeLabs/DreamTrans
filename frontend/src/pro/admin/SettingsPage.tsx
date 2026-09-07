import { useCallback, useEffect, useState } from 'react'
import {
  formatUSD,
  getSystemSettings,
  getTrainingProgramStats,
  type TrainingProgramStats,
  patchSystemSettings,
  previewSystemSettingsReset,
  resetSystemSettings,
  type SystemSettingsPatch,
  type SystemSettingsResetPreview,
  type SystemSettingsResponse,
  type SystemSettingsValues,
} from '../../admin/api'
import { errorMessage, formatInteger, type Runner } from './shared'
import { ErrorBanner, Modal } from './ui'

type SettingKey = keyof SystemSettingsValues
type ToggleKey = 'billing_enabled' | 'allow_negative_balance' | 'allow_user_api_key' | 'training_program_enabled' | 'gift_training_discount'
type NumericKey = 'trial_credit_usd' | 'trial_credit_days' | 'training_discount_percent'

const settingCopy: Record<SettingKey, { label: string; description: string; dangerous?: boolean }> = {
  billing_enabled: {
    label: '启用平台计费',
    description: '关闭后，新请求不再从余额扣费。已产生的账单不会改变。',
    dangerous: true,
  },
  allow_negative_balance: {
    label: '允许余额透支',
    description: '启用后，余额不足的用户仍可继续使用由平台承担费用的服务。',
    dangerous: true,
  },
  allow_user_api_key: {
    label: '允许用户自带 Provider Key',
    description: '用户可使用自己的上游密钥；平台只收取服务费，不承担上游成本。',
  },
  trial_credit_usd: {
    label: '新用户试用额度（USD）',
    description: '注册时作为赠送额度发放；只影响之后创建的账户。',
  },
  trial_credit_days: {
    label: '试用额度有效期（天）',
    description: '超过有效期后未用完的试用额度自动作废。',
  },
  training_discount_percent: {
    label: '训练计划转录折扣（%）',
    description: '加入训练计划的用户在转录费用上的折扣，只在配置了 SM_API_KEY_NO_TRAINING 的部署上生效。首页、引导和设置页自动显示当前值；下调折扣对已加入用户属于不利变更，按条款第 13 节需提前 30 天通知。',
    dangerous: true,
  },
  training_program_enabled: {
    label: '训练计划总开关',
    description: '关闭即暂停整个训练计划：前端隐藏训练相关内容，所有人走未开启训练的账户，折扣停止。重新开启后用户需重新选择（清零逻辑后续提供）。',
    dangerous: true,
  },
  gift_training_discount: {
    label: '赠送额度也享受训练折扣',
    description: '默认关闭：赠送额度（试用、活动、邀请）先扣、按标准价、走未开启训练的账户；充值赠送算付费。开启后赠送额度按用户的训练选择计费。',
    dangerous: true,
  },
}

const numericRules: Record<NumericKey, { min: number; max: number; step: string; error: string }> = {
  trial_credit_usd: { min: 0, max: 1_000_000, step: '0.01', error: '试用额度必须在 $0 到 $1,000,000 之间' },
  trial_credit_days: { min: 0, max: 3650, step: '1', error: '有效期必须在 0 到 3650 天之间' },
  training_discount_percent: { min: 0, max: 100, step: '1', error: '折扣必须在 0 到 100 之间' },
}

const toggleKeys: ToggleKey[] = ['billing_enabled', 'allow_negative_balance', 'allow_user_api_key', 'training_program_enabled', 'gift_training_discount']
const numericKeys: NumericKey[] = ['trial_credit_usd', 'trial_credit_days', 'training_discount_percent']

const systemSettingsResetConfirmation = '重置系统设置'

function formatSettingValue(key: SettingKey, value: boolean | number) {
  if (typeof value === 'boolean') return value ? '开启' : '关闭'
  if (key === 'trial_credit_usd') return formatUSD(value)
  if (key === 'trial_credit_days') return `${formatInteger(value)} 天`
  if (key === 'training_discount_percent') return `${value}%`
  return String(value)
}

function numericValid(key: NumericKey, value: number) {
  const rule = numericRules[key]
  return Number.isFinite(value) && value >= rule.min && value <= rule.max
}

export function SettingsPage({ run }: { run: Runner }) {
  const [response, setResponse] = useState<SystemSettingsResponse | null>(null)
  const [values, setValues] = useState<SystemSettingsValues | null>(null)
  const [loading, setLoading] = useState(true)
  const [localError, setLocalError] = useState('')
  const [confirmSave, setConfirmSave] = useState(false)
  const [resetPreview, setResetPreview] = useState<SystemSettingsResetPreview | null>(null)
  const [resetText, setResetText] = useState('')
  const [trainingStats, setTrainingStats] = useState<TrainingProgramStats | null>(null)

  useEffect(() => {
    let active = true
    void getTrainingProgramStats().then((stats) => { if (active) setTrainingStats(stats) }).catch(() => undefined)
    return () => { active = false }
  }, [])

  const adopt = useCallback((next: SystemSettingsResponse) => {
    setResponse(next)
    setValues(next.values)
  }, [])

  const load = useCallback(async () => {
    setLoading(true)
    setLocalError('')
    try {
      adopt(await getSystemSettings())
    } catch (reason) {
      setLocalError(errorMessage(reason))
    } finally {
      setLoading(false)
    }
  }, [adopt])

  useEffect(() => { void load() }, [load])

  const dirtyKeys = values && response
    ? (Object.keys(values) as SettingKey[]).filter((key) => values[key] !== response.values[key])
    : []
  const dangerousDirtyKeys = dirtyKeys.filter((key) => settingCopy[key].dangerous)
  const numericsValid = values !== null && numericKeys.every((key) => numericValid(key, values[key]))

  function changeSetting<K extends SettingKey>(key: K, value: SystemSettingsValues[K]) {
    setValues((current) => current ? { ...current, [key]: value } : current)
    setResetPreview(null)
    setResetText('')
  }

  async function performSave() {
    if (!values || dirtyKeys.length === 0 || !numericsValid) return
    const patch: SystemSettingsPatch = {}
    for (const key of dirtyKeys) {
      if (key === 'trial_credit_usd' || key === 'trial_credit_days' || key === 'training_discount_percent') patch[key] = values[key]
      else patch[key] = values[key]
    }
    const result = await run(() => patchSystemSettings(patch), '系统设置已保存')
    if (result) {
      adopt(result)
      setConfirmSave(false)
    }
  }

  async function requestSave() {
    if (dangerousDirtyKeys.length > 0) {
      setConfirmSave(true)
      return
    }
    await performSave()
  }

  async function openResetPreview() {
    const result = await run(() => previewSystemSettingsReset())
    if (result) {
      setResetPreview(result)
      setResetText('')
    }
  }

  async function confirmReset() {
    if (!resetPreview || resetText !== systemSettingsResetConfirmation) return
    const result = await run(() => resetSystemSettings(), '系统设置已恢复为安全默认值')
    if (result) {
      adopt(result)
      setResetPreview(null)
      setResetText('')
    }
  }

  return (
    <>
      <div className="pa-stack">
        <section className="pa-card pa-section">
          <div className="pa-section__heading">
            <div><h2>系统行为</h2><p>影响全部组织的新请求；只会提交你实际修改的字段。</p></div>
            {dirtyKeys.length > 0 && <span className="pa-pill pa-pill--accent">{dirtyKeys.length} 项未保存</span>}
          </div>
          {localError && <ErrorBanner message={localError} />}
          {loading && <div className="pa-settings-loading"><span className="pa-skeleton pa-skeleton--panel" /><span className="pa-skeleton pa-skeleton--panel" /></div>}
          {!loading && values && (
            <div className="pa-settings-list">
              {toggleKeys.map((key) => (
                <label className={`pa-switch ${settingCopy[key].dangerous ? 'is-sensitive' : ''}`} key={key}>
                  <span><strong>{settingCopy[key].label}</strong><small>{settingCopy[key].description}</small></span>
                  <input checked={values[key]} onChange={(event) => changeSetting(key, event.target.checked)} type="checkbox" />
                </label>
              ))}
              {numericKeys.map((key) => {
                const rule = numericRules[key]
                const valid = numericValid(key, values[key])
                return (
                  <label className="pa-field" key={key}>
                    <span><strong>{settingCopy[key].label}</strong><small>{settingCopy[key].description}</small></span>
                    <input max={rule.max} min={rule.min} onChange={(event) => changeSetting(key, Number(event.target.value))} step={rule.step} type="number" value={values[key]} />
                    <em className={!valid ? 'pa-field-error' : ''}>
                      {!valid ? rule.error : `默认值：${formatSettingValue(key, response?.defaults[key] ?? 0)}`}
                    </em>
                  </label>
                )
              })}
            </div>
          )}
          <div className="pa-button-row pa-button-row--split">
            <button className="pa-button pa-button--quiet" disabled={loading || !values} onClick={() => void openResetPreview()} type="button">预览系统设置重置</button>
            <button className="pa-button pa-button--primary" disabled={loading || dirtyKeys.length === 0 || !numericsValid} onClick={() => void requestSave()} type="button">保存修改</button>
          </div>
        </section>

        <section className="pa-card pa-section">
          <div className="pa-section__heading"><div><h2>训练计划统计</h2><p>领码时、首充时的训练开关状态，以及两个账号各自的转录小时数。</p></div></div>
          {!trainingStats && <p className="pa-form-note">正在读取…</p>}
          {trainingStats && (
            <div className="pa-summary-grid pa-summary-grid--four">
              <div><small>状态 / 折扣</small><strong>{trainingStats.enabled ? '开启' : '已暂停'} · {trainingStats.discount_percent}%</strong></div>
              <div><small>已加入 / 不加入 / 未选择</small><strong>{trainingStats.users_opted_in} / {trainingStats.users_declined} / {trainingStats.users_unanswered}</strong></div>
              <div><small>领码时开着训练</small><strong>{trainingStats.claimed_opted_in} / {trainingStats.claimed_total}</strong></div>
              <div><small>首充时开着训练</small><strong>{trainingStats.first_topup_opted_in} / {trainingStats.first_topup_total}</strong></div>
              <div><small>首充后改过开关</small><strong>{trainingStats.changed_after_first_topup}</strong></div>
              <div><small>训练 / 不训练账号小时</small><strong>{trainingStats.training_hours.toFixed(1)} / {trainingStats.standard_hours.toFixed(1)}</strong></div>
              <div><small>赠送额度会话小时</small><strong>{trainingStats.gift_routed_hours.toFixed(1)}</strong></div>
              <div><small>折扣差额退回</small><strong>{formatUSD(trainingStats.route_refunds_usd)}</strong></div>
            </div>
          )}
        </section>

        <section className="pa-card pa-section pa-info-section">
          <h2>重置范围说明</h2>
          <p>这里的“系统设置重置”只恢复运行开关和新用户试用额度。成本加价请在“成本与加价”页管理，套餐与充值档位请在“会员与充值”页管理；余额与账本不会被改动。</p>
        </section>
      </div>

      {confirmSave && values && response && (
        <Modal
          danger
          footer={(
            <>
              <button className="pa-button pa-button--quiet" onClick={() => setConfirmSave(false)} type="button">返回检查</button>
              <button className="pa-button pa-button--danger" onClick={() => void performSave()} type="button">确认保存</button>
            </>
          )}
          onClose={() => setConfirmSave(false)}
          title="确认高风险系统设置"
        >
          <div className="pa-dialog-form">
            <div className="pa-callout pa-callout--danger">以下改动会立即影响全部组织的新请求。</div>
            <ul className="pa-change-list">
              {dangerousDirtyKeys.map((key) => (
                <li key={key}><strong>{settingCopy[key].label}</strong><span>{formatSettingValue(key, response.values[key])} → {formatSettingValue(key, values[key])}</span></li>
              ))}
            </ul>
          </div>
        </Modal>
      )}

      {resetPreview && (
        <Modal
          danger
          footer={(
            <>
              <button className="pa-button pa-button--quiet" onClick={() => {
                setResetPreview(null)
                setResetText('')
              }} type="button">取消</button>
              <button className="pa-button pa-button--danger" disabled={resetText !== systemSettingsResetConfirmation} onClick={() => void confirmReset()} type="button">确认重置系统设置</button>
            </>
          )}
          onClose={() => {
            setResetPreview(null)
            setResetText('')
          }}
          title="重置系统设置"
        >
          <div className="pa-dialog-form">
            <p className="pa-form-note">将恢复以下 {resetPreview.changes.length} 项；计费配置与历史数据不受影响。</p>
            <ul className="pa-change-list">
              {resetPreview.changes.map((change) => (
                <li key={change.key}><strong>{settingCopy[change.key]?.label || change.key}</strong><span>{formatSettingValue(change.key, change.from)} → {formatSettingValue(change.key, change.to)}</span></li>
              ))}
            </ul>
            <label><span>输入“{systemSettingsResetConfirmation}”确认</span><input autoFocus onChange={(event) => setResetText(event.target.value)} value={resetText} /></label>
          </div>
        </Modal>
      )}
    </>
  )
}
