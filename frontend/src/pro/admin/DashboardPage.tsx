import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { adminFetch } from '../../admin/api'
import { ErrorBanner } from './ui'
import { downloadConsoleCSV } from './csv'

type Row = Record<string, string | number | null>
interface Credit { configured: boolean; starting_usd: number; remaining_usd?: number; days_remaining: number | null; route: string; started_at: string; daily_usd?: number }
interface Dashboard { financial: boolean; metrics_available: boolean; export_allowed: boolean; credit?: Credit; activity?: Row[]; paths?: Row[]; finance?: Row[]; [key: string]: unknown }

const sections: { key: string; title: string; description: string; measures: string[] }[] = [
  { key: 'activity', title: '活跃与转录时长', description: '按所选粒度统计，每段时间内的用户去重。', measures: ['hours'] },
  { key: 'funnel', title: '渠道转化漏斗', description: '所选期间注册的用户，截至结束日的转化；兑换批次优先归因，其次注册推广渠道。未领码的充值用户也会计入充值阶段。', measures: ['registered', 'redeemed', 'first_session', 'one_hour', 'first_topup', 'second_topup'] },
  { key: 'retention', title: '兑换批次留存', description: '兑换后第 1 / 2 / 4 周仍有转录用量的账户；只计入已完整经过该周的用户。', measures: ['eligible', 'retained'] },
  { key: 'hours_histogram', title: '每人每周使用时长', description: '包含零用量用户周；首尾周按所选日期截取。桶单位为小时。', measures: ['user_weeks'] },
  { key: 'routing', title: '训练、资金来源与组织类型', description: '按每笔转录的路由快照分组。历史缺少快照的记录单独显示 unknown。', measures: ['hours'] },
  { key: 'costs', title: '用量成本与毛利', description: '付费用量毛利 = 扣费减赠送部分、会话结束退差额与上游成本；不等于现金利润，不含会员收入与支付手续费。', measures: ['paid_consumed_usd', 'upstream_usd', 'paid_usage_margin_usd'] },
  { key: 'paths', title: '四条转录路径的每小时毛利', description: '使用发生时的 Free / Pro 与训练路由。没有用量的路径显示空值，赠送消耗不当作收入。', measures: ['margin_per_hour_usd'] },
  { key: 'finance', title: '收入、退款与手续费', description: 'Stripe 实收交易按记账日期统计。pending_fees 表示待取到实际手续费的笔数。', measures: ['topup_usd', 'membership_usd', 'refund_usd', 'known_fee_usd'] },
  { key: 'income_tiers', title: '每周收入与充值档位', description: '按实际支付金额分档，会员付款单独列出。', measures: ['revenue_usd'] },
  { key: 'liabilities', title: '账户余额与未用额度', description: '当前钱包余额与未过期赠送额度；不是所选历史结束日的余额。', measures: ['wallet_usd', 'grants_usd'] },
  { key: 'latency', title: '转录延迟', description: '会话 p50 的中位数与会话 p90 的第 90 百分位，单位毫秒；不将会话百分位误称为全局逐句百分位。长会话保留最近 8192 个样本。', measures: ['median_session_p50_ms', 'p90_session_p90_ms'] },
  { key: 'edits', title: '转录修改次数', description: '只累计定稿后的文本变化。相同内容重传、局部结果更新与仅翻译变化不计入。', measures: ['final_segments', 'edited_segments', 'edits'] },
  { key: 'languages', title: '翻译目标语言与模型成本', description: '按关联会话的目标语言汇总 token 和上游成本，缺失会话的记录显示 unknown。', measures: ['upstream_usd'] },
]

const labels: Record<string, string> = { period: '时间', channel: '渠道', registered: '注册', redeemed: '领码', first_session: '首场会话', one_hour: '用满一小时', first_topup: '首充', second_topup: '二充', hours: '小时', users: '用户', active_users: '活跃用户', route: '路由', funding: '资金', tenant_kind: '组织类型', bucket: '小时区间', user_weeks: '用户周', week: '周', eligible: '可观察人数', retained: '留存人数', batch_id: '批次', plan: '会员', samples: '样本数', sessions: '会话数', final_segments: '定稿段数', edited_segments: '修改段数', edits: '修改次数', language: '目标语言', source_language: '源语言', model: '模型', kind: '类型', payments: '笔数', input_tokens: '输入 token', output_tokens: '输出 token', pending_fees: '待补手续费', topup_usd: '充值 USD', membership_usd: '会员 USD', refund_usd: '退款 USD', known_fee_usd: '已知手续费 USD', consumed_usd: '消耗 USD', gift_usd: '赠送消耗 USD', paid_consumed_usd: '付费消耗 USD', upstream_usd: '上游成本 USD', paid_usage_margin_usd: '用量毛利 USD', margin_per_hour_usd: '每小时毛利 USD', revenue_usd: '收入 USD', tier_usd: '档位 USD', wallet_usd: '钱包 USD', grants_usd: '未用额度 USD', median_session_p50_ms: '会话 p50 中位数 ms', p90_session_p90_ms: '会话 p90 的 p90 ms' }

const colors = ['#6366f1', '#10b981', '#f59e0b', '#ef4444', '#0891b2', '#8b5cf6']
const number = (value: unknown) => typeof value === 'number' && Number.isFinite(value) ? value : 0
const display = (value: unknown) => value == null ? '—' : typeof value === 'number' ? value.toLocaleString(undefined, { maximumFractionDigits: 3 }) : String(value)
const isoDate = (offsetDays: number) => new Date(Date.now() + offsetDays * 86400000).toISOString().slice(0, 10)

function MetricChart({ rows, measures, title, compact = false }: { rows: Row[]; measures: string[]; title: string; compact?: boolean }) {
  if (!rows.length) return <p className="pa-chart-empty">所选范围内暂无数据</p>
  const values = rows.flatMap(row => measures.map(key => number(row[key])))
  const max = Math.max(1, ...values.map(Math.abs))
  const negative = values.some(value => value < 0)
  const height = compact ? 90 : 220
  const baseline = negative ? height / 2 : height - 10
  const scale = (negative ? height / 2 - 10 : height - 20) / max
  const width = Math.max(compact ? 240 : 560, rows.length * Math.max(20, measures.length * 9))
  const groupWidth = width / rows.length
  const barWidth = groupWidth / (measures.length + 1)
  return (
    <>
      {!compact && (
        <div className="pa-chart-legend">
          {measures.map((key, index) => <span key={key}><i style={{ background: colors[index % colors.length] }} />{labels[key] ?? key}</span>)}
        </div>
      )}
      <div className={`pa-chart-scroll${compact ? ' pa-chart-scroll--compact' : ''}`}>
        <svg role="img" aria-label={title} viewBox={`0 0 ${width} ${height}`} style={{ minWidth: compact ? 0 : Math.min(width, 1200) }}>
          <title>{title}，详细数值见下表</title>
          <line x1="0" y1={baseline} x2={width} y2={baseline} stroke="currentColor" opacity=".2" />
          {rows.map((row, i) => measures.map((key, j) => {
            const value = number(row[key])
            const rowLabel = Object.values(row).filter(v => typeof v === 'string').join(' / ')
            return (
              <rect
                key={`${i}-${key}`}
                x={i * groupWidth + j * barWidth + 4}
                y={value >= 0 ? baseline - value * scale : baseline}
                width={Math.max(2, barWidth - 2)}
                height={Math.max(value === 0 ? 0 : 1, Math.abs(value * scale))}
                rx={1.5}
                fill={colors[j % colors.length]}
              >
                <title>{rowLabel} · {labels[key] ?? key}: {display(row[key])}</title>
              </rect>
            )
          }))}
        </svg>
      </div>
    </>
  )
}

function Kpi({ label, value, hint, children }: { label: string; value: string; hint?: string; children?: ReactNode }) {
  return (
    <article className="pa-card pa-metric pa-kpi">
      <small>{label}</small>
      <strong>{value}</strong>
      {hint && <p>{hint}</p>}
      {children}
    </article>
  )
}

export function DashboardPage() {
  const [from, setFrom] = useState(() => isoDate(-29))
  const [to, setTo] = useState(() => isoDate(0))
  const [grain, setGrain] = useState('day')
  const [channel, setChannel] = useState('')
  const [query, setQuery] = useState('')
  const [data, setData] = useState<Dashboard | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    setBusy(true)
    try {
      setData(await adminFetch<Dashboard>(`/api/admin/dashboard?${query}`))
      setError('')
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取失败')
      setData(null)
    } finally {
      setBusy(false)
    }
  }, [query])
  useEffect(() => { void load() }, [load])

  const rowsFor = (key: string) => Array.isArray(data?.[key]) ? data[key] as Row[] : []
  const sum = (key: string, field: string) => rowsFor(key).reduce((value, row) => value + number(row[field]), 0)
  const credit = data?.credit

  return (
    <div className="pa-stack">
      <ErrorBanner message={error} onClose={() => setError('')} />

      <form
        className="pa-card pa-filter-bar"
        onSubmit={e => { e.preventDefault(); setQuery(new URLSearchParams({ from, to, granularity: grain, channel }).toString()) }}
      >
        <label><span>开始日期（UTC）</span><input type="date" value={from} onChange={e => setFrom(e.target.value)} required /></label>
        <label><span>结束日期（UTC）</span><input type="date" value={to} onChange={e => setTo(e.target.value)} required /></label>
        <label><span>粒度</span><select value={grain} onChange={e => setGrain(e.target.value)}><option value="day">日</option><option value="week">周</option><option value="month">月</option></select></label>
        <label><span>渠道</span><input value={channel} onChange={e => setChannel(e.target.value)} placeholder="全部可见渠道" /></label>
        <button className="pa-button pa-button--primary" disabled={busy} type="submit">{busy ? '读取中…' : '查询'}</button>
      </form>

      {data && (
        <>
          <div className="pa-dashboard-kpis">
            <Kpi label="活跃用户" value={display(rowsFor('activity').at(-1)?.active_users)} hint="所选范围最后一个有用量的周期">
              <MetricChart compact rows={rowsFor('activity')} measures={['active_users']} title="活跃趋势" />
            </Kpi>
            <Kpi label="转录小时" value={display(sum('activity', 'hours'))} hint="所选期间合计">
              <MetricChart compact rows={rowsFor('activity')} measures={['hours']} title="时长趋势" />
            </Kpi>
            {data.financial && (
              <Kpi label="实收收入 USD" value={display(sum('finance', 'topup_usd') + sum('finance', 'membership_usd') + sum('finance', 'refund_usd'))} hint="所选期间，已扣退款；手续费另列">
                <MetricChart compact rows={rowsFor('finance')} measures={['topup_usd', 'membership_usd']} title="收入趋势" />
              </Kpi>
            )}
            {credit && (
              <Kpi
                label="Speechmatics 额度"
                value={credit.configured ? `$${display(credit.remaining_usd)}` : '待配置'}
                hint={credit.configured ? `预计剩余 ${credit.days_remaining == null ? '—' : display(credit.days_remaining)} 天 · ${credit.route}` : '在分流设置中选择账户与起算日期'}
              >
                {credit.configured && <progress aria-label="Speechmatics 额度剩余比例" max={credit.starting_usd || 1} value={Math.max(0, credit.remaining_usd ?? 0)} />}
              </Kpi>
            )}
          </div>

          {sections.filter(section => Array.isArray(data[section.key])).map(section => {
            const rows = rowsFor(section.key)
            const keys = [...new Set(rows.flatMap(row => Object.keys(row)))]
            return (
              <section className="pa-card pa-section" key={section.key}>
                <div className="pa-section__heading">
                  <div>
                    <h2>{section.title}</h2>
                    <p>{section.description}</p>
                  </div>
                  {data.export_allowed && rows.length > 0 && (
                    <button className="pa-button" type="button" onClick={() => downloadConsoleCSV(`${section.key}-${from}-${to}.csv`, [keys.map(key => labels[key] ?? key), ...rows.map(row => keys.map(key => row[key]))])}>下载 CSV</button>
                  )}
                </div>
                <MetricChart rows={rows} measures={section.measures} title={section.title} />
                {rows.length > 0 && (
                  <details className="pa-details">
                    <summary>查看全部数值（{rows.length} 行）</summary>
                    <div className="pa-table-wrap">
                      <table className="pa-table">
                        <thead><tr>{keys.map(key => <th key={key}>{labels[key] ?? key}</th>)}</tr></thead>
                        <tbody>{rows.map((row, index) => <tr key={index}>{keys.map(key => <td key={key}>{display(row[key])}</td>)}</tr>)}</tbody>
                      </table>
                    </div>
                  </details>
                )}
              </section>
            )
          })}
        </>
      )}
    </div>
  )
}
