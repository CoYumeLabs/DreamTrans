import { useCallback, useEffect, useRef, useState } from 'react'
import {
  getBillingCatalog,
  getModelCatalog,
  getModelRateCostPerMillion,
  refreshModelCatalog,
  updateModelCost,
  updateModelPolicy,
  type BillingCatalog,
  type CostRate,
  type ModelCatalog,
  type ModelPolicy,
  type ProviderAvailability,
  type ProviderModel,
} from '../../admin/api'
import { formatDate, type Runner } from './shared'
import { Modal } from './ui'

const purposeLabels: Record<ModelPolicy['purpose'], string> = {
  translation: '翻译',
  summary: '摘要',
  chat: '问答',
  embedding: '向量',
}

interface ModelCostDraft {
  model: ProviderModel
  service: 'llm' | 'embedding'
  input: string
  cachedInput: string
  cacheWrite: string
  output: string
  original: [number | null, number | null, number | null, number | null]
}

function modelAvailability(model: ProviderModel): ProviderAvailability {
  if (model.availability_status) return model.availability_status
  if (model.provider_available) return 'confirmed'
  if (model.source === 'builtin') return 'unverified'
  return 'stale'
}

function modelAvailabilityCopy(status: ProviderAvailability) {
  if (status === 'confirmed' || status === 'provider_confirmed') return { label: 'Provider 已确认', className: 'is-good' }
  if (status === 'unverified' || status === 'builtin_unverified') return { label: '内置但未验证', className: 'is-warn' }
  return { label: '暂时不可用', className: 'is-muted' }
}

function isModelUnavailable(model: ProviderModel) {
  const status = modelAvailability(model)
  return !model.provider_available
    || status === 'temporarily_unavailable'
    || status === 'unavailable'
    || status === 'stale'
}

function catalogSyncStatusCopy(state: { status?: string; last_error?: string } | null | undefined) {
  if (state?.status === 'provider_confirmed') {
    return { label: '同步已确认', className: 'is-good' }
  }
  if (state?.status === 'builtin_unverified') {
    return { label: '内置目录尚未验证', className: 'is-warn' }
  }
  if (state?.status === 'temporarily_unavailable') {
    return { label: '暂时不可用', className: 'is-bad' }
  }
  if (state?.last_error) return { label: '最近同步失败', className: 'is-bad' }
  return { label: '等待首次同步', className: 'is-muted' }
}

const modelKey = (model: ProviderModel) => model.qualified_id ?? model.model_id

function costInputValue(value: number | null) {
  return value === null ? '' : String(value)
}

function createModelCostDraft(
  model: ProviderModel,
  service: ModelCostDraft['service'],
  rates: CostRate[],
): ModelCostDraft {
  const values: [number | null, number | null, number | null, number | null] = [
    getModelRateCostPerMillion(rates, service, model.model_id, 'input_token'),
    getModelRateCostPerMillion(rates, service, model.model_id, 'cached_input_token'),
    getModelRateCostPerMillion(rates, service, model.model_id, 'cache_write_token'),
    getModelRateCostPerMillion(rates, service, model.model_id, 'output_token'),
  ]
  return {
    model,
    service,
    input: costInputValue(values[0]),
    cachedInput: costInputValue(values[1]),
    cacheWrite: costInputValue(values[2]),
    output: costInputValue(values[3]),
    original: values,
  }
}

export function ModelsPage({ run, canPrice = true }: { run: Runner; canPrice?: boolean }) {
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null)
  const [billingCatalog, setBillingCatalog] = useState<BillingCatalog | null>(null)
  const [costDraft, setCostDraft] = useState<ModelCostDraft | null>(null)
  const [loading, setLoading] = useState(true)
  const refreshingRef = useRef(false)
  const [refreshing, setRefreshing] = useState(false)
  const purposes = Object.keys(purposeLabels) as ModelPolicy['purpose'][]

  const load = useCallback(async () => {
    setLoading(true)
    const result = await run(() => Promise.all([getModelCatalog(), getBillingCatalog()]))
    if (result) {
      setCatalog(result[0])
      setBillingCatalog(result[1])
    }
    setLoading(false)
  }, [run])

  useEffect(() => { void load() }, [load])

  async function refreshCatalog() {
    if (refreshingRef.current) return
    refreshingRef.current = true
    setRefreshing(true)
    try {
      await run(async () => {
        try {
          await refreshModelCatalog()
        } finally {
          setCatalog(await getModelCatalog())
        }
      }, '模型目录已刷新')
    } finally {
      refreshingRef.current = false
      setRefreshing(false)
    }
  }

  async function changePolicy(
    modelId: string,
    purpose: ModelPolicy['purpose'],
    patch: Partial<ModelPolicy>,
  ) {
    const model = catalog?.models.find((item) => item.model_id === modelId)
    if (!model || isModelUnavailable(model)) return
    const existing = model.policies.find((policy) => policy.purpose === purpose)
    const next: ModelPolicy = {
      purpose,
      model_id: modelId,
      is_approved: existing?.is_approved ?? false,
      is_default: existing?.is_default ?? false,
      cost_confirmed: existing?.cost_confirmed ?? false,
      ...patch,
    }
    const result = await run(() => updateModelPolicy(next), '模型策略已更新')
    if (result) setCatalog(result)
  }

  function openModelCost(model: ProviderModel) {
    const rates = billingCatalog?.rates || []
    const embedding = model.policies.some((policy) => policy.purpose === 'embedding')
      && !model.policies.some((policy) => policy.purpose !== 'embedding')
    setCostDraft(createModelCostDraft(model, embedding ? 'embedding' : 'llm', rates))
  }

  async function saveModelCost() {
    if (!costDraft) return
    const values = [
      Number(costDraft.input),
      costDraft.cachedInput === '' ? 0 : Number(costDraft.cachedInput),
      costDraft.cacheWrite === '' ? 0 : Number(costDraft.cacheWrite),
      costDraft.output === '' ? 0 : Number(costDraft.output),
    ]
    if (costDraft.input === '' || values.some((value) => !Number.isFinite(value) || value < 0)) return
    const result = await run(async () => {
      const nextBilling = await updateModelCost({
        provider: costDraft.model.provider,
        model: costDraft.model.model_id,
        service: costDraft.service,
        input_per_million: values[0],
        cached_input_per_million: values[1],
        cache_write_per_million: values[2],
        output_per_million: values[3],
      })
      return [await getModelCatalog(), nextBilling] as const
    }, '模型成本已保存')
    if (result) {
      setCatalog(result[0])
      setBillingCatalog(result[1])
      setCostDraft(null)
    }
  }

  const costChanged = costDraft ? [
    Number(costDraft.input || 0),
    Number(costDraft.cachedInput || 0),
    Number(costDraft.cacheWrite || 0),
    Number(costDraft.output || 0),
  ].some((value, index) => value !== (costDraft.original[index] ?? 0)) : false
  const providerStates = catalog?.providers?.length ? catalog.providers : catalog ? [catalog] : []

  return (
    <>
      <section className="pa-card pa-section">
        <div className="pa-section__heading">
          <div><h2>供应商模型目录</h2><p>每家供应商各自同步；非默认供应商的模型以「供应商::模型」标识。新模型默认不开放，缺少有效成本时不能审批。</p></div>
          <button className="pa-button pa-button--primary" disabled={refreshing} onClick={() => void refreshCatalog()} type="button">
            {refreshing ? '正在刷新…' : '立即刷新'}
          </button>
        </div>
        {providerStates.map((state) => {
          const syncStatus = catalogSyncStatusCopy(state)
          return (
            <div className="pa-provider-status" key={state.provider}>
              <strong>{state.provider}</strong>
              <span className={`pa-status ${syncStatus.className}`}>{syncStatus.label}</span>
              <span>最近尝试：{formatDate(state.last_attempt_at)}</span>
              <span>最近成功：{formatDate(state.last_success_at)}</span>
              {state.last_error && <span className="pa-provider-error">{state.last_error}</span>}
            </div>
          )
        })}
        <div className="pa-table-wrap"><table>
          <thead><tr><th>模型</th><th>Provider</th><th>状态</th><th>允许用途</th></tr></thead>
          <tbody>
            {loading && <tr><td className="pa-table-empty" colSpan={4}>正在加载模型目录…</td></tr>}
            {!loading && catalog?.models.length === 0 && <tr><td className="pa-table-empty" colSpan={4}>模型目录为空。</td></tr>}
            {!loading && catalog?.models.map((model) => {
              const availability = modelAvailabilityCopy(modelAvailability(model))
              const unavailable = isModelUnavailable(model)
              const costConfirmed = model.policies.some((policy) => policy.cost_confirmed)
              return (
                <tr key={modelKey(model)}>
                  <td>
                    <strong>{modelKey(model)}</strong>
                    <small>{model.source} · {costConfirmed ? '有效成本已配置' : '缺少有效成本'}</small>
                  </td>
                  <td>{model.provider}</td>
                  <td>
                    <span className={`pa-status ${availability.className}`}>{availability.label}</span>
                    <button className="pa-link-button" onClick={() => openModelCost(model)} type="button">
                      {!canPrice ? '查看成本' : costConfirmed ? '查看或修改成本' : '配置成本'}
                    </button>
                  </td>
                  <td><div className="pa-policy-grid">
                    {purposes.map((purpose) => {
                      const policy = model.policies.find((item) => item.purpose === purpose)
                      const approved = policy?.is_approved ?? false
                      return (
                        <div className="pa-policy" key={purpose}>
                          <button
                            className={approved ? 'is-approved' : ''}
                            disabled={unavailable || (!policy?.cost_confirmed && !approved)}
                            onClick={() => void changePolicy(modelKey(model), purpose, {
                              is_approved: !approved,
                              is_default: approved ? false : policy?.is_default ?? false,
                            })}
                            title={unavailable
                              ? '模型当前不可用，不能更改审批状态'
                              : !policy?.cost_confirmed ? '请先配置该模型的有效上游成本' : ''}
                            type="button"
                          >{purposeLabels[purpose]}{approved ? ' ✓' : ''}</button>
                          {approved && (
                            <button
                              className={policy?.is_default ? 'is-default' : ''}
                              disabled={unavailable}
                              onClick={() => void changePolicy(modelKey(model), purpose, { is_default: true })}
                              title={unavailable ? '模型当前不可用，不能设为默认' : ''}
                              type="button"
                            >{policy?.is_default ? '默认' : '设为默认'}</button>
                          )}
                        </div>
                      )
                    })}
                  </div></td>
                </tr>
              )
            })}
          </tbody>
        </table></div>
      </section>

      {costDraft && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" onClick={() => setCostDraft(null)} type="button">取消</button>
              <button className="pa-button pa-button--primary" disabled={!canPrice || !costChanged || costDraft.input === ''} onClick={() => void saveModelCost()} type="button">保存成本</button>
            </>
          )}
          onClose={() => setCostDraft(null)}
          title={`模型成本 · ${modelKey(costDraft.model)}`}
        >
          <div className="pa-dialog-form">
            <p className="pa-form-note">
              以下数值均为 USD / 百万 token，并按当前服务类型从有效目录预填；填 0 表示清除对应价格。
            </p>
            <label><span>服务类型</span><select onChange={(event) => setCostDraft(createModelCostDraft(
              costDraft.model,
              event.target.value as ModelCostDraft['service'],
              billingCatalog?.rates || [],
            ))} value={costDraft.service}>
              <option value="llm">LLM</option><option value="embedding">Embedding</option>
            </select></label>
            <div className="pa-dialog-grid">
              <label><span>输入</span><input autoFocus min="0" onChange={(event) => setCostDraft({ ...costDraft, input: event.target.value })} required step="0.000001" type="number" value={costDraft.input} /></label>
              {costDraft.service === 'llm' && (
                <>
                  <label><span>缓存输入</span><input min="0" onChange={(event) => setCostDraft({ ...costDraft, cachedInput: event.target.value })} placeholder="0 表示清除" step="0.000001" type="number" value={costDraft.cachedInput} /></label>
                  <label><span>缓存写入</span><input min="0" onChange={(event) => setCostDraft({ ...costDraft, cacheWrite: event.target.value })} placeholder="0 表示清除" step="0.000001" type="number" value={costDraft.cacheWrite} /></label>
                  <label><span>输出</span><input min="0" onChange={(event) => setCostDraft({ ...costDraft, output: event.target.value })} step="0.000001" type="number" value={costDraft.output} /></label>
                </>
              )}
            </div>
            {costChanged && <div className="pa-callout pa-callout--warning">保存后将以这些数值覆盖该模型当前成本，并影响之后请求的售价。</div>}
          </div>
        </Modal>
      )}
    </>
  )
}
