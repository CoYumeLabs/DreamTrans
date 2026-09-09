import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { adminFetch } from '../../admin/api'
import { ErrorBanner } from './ui'
import { downloadConsoleCSV } from './csv'
import { BarList, ColumnChart, Heatmap, Legend, LineChart, Sparkline } from './charts'
import { formatCompact, formatPercent, formatUsd, number, ordinalColor, periodLabel, type Row, type Series } from './chartUtils'

interface Credit { configured: boolean; starting_usd: number; remaining_usd?: number; days_remaining: number | null; route: string; started_at: string; daily_usd?: number }
interface Dashboard { financial: boolean; metrics_available: boolean; export_allowed: boolean; credit?: Credit; [key: string]: unknown }

const labels: Record<string, string> = { period: '时间', channel: '渠道', registered: '注册', redeemed: '领码', first_session: '首场会话', one_hour: '用满一小时', first_topup: '首充', second_topup: '二充', hours: '小时', users: '用户', active_users: '活跃用户', route: '路由', funding: '资金', tenant_kind: '组织类型', bucket: '小时区间', user_weeks: '用户周', week: '周', eligible: '可观察人数', retained: '留存人数', batch_id: '批次', plan: '会员', samples: '样本数', sessions: '会话数', final_segments: '定稿段数', edited_segments: '修改段数', edits: '修改次数', language: '目标语言', source_language: '源语言', model: '模型', kind: '类型', payments: '笔数', input_tokens: '输入 token', output_tokens: '输出 token', pending_fees: '待补手续费', topup_usd: '充值 USD', membership_usd: '会员 USD', refund_usd: '退款 USD', known_fee_usd: '已知手续费 USD', consumed_usd: '消耗 USD', gift_usd: '赠送消耗 USD', paid_consumed_usd: '付费消耗 USD', upstream_usd: '上游成本 USD', paid_usage_margin_usd: '用量毛利 USD', margin_per_hour_usd: '每小时毛利 USD', revenue_usd: '收入 USD', tier_usd: '档位 USD', wallet_usd: '钱包 USD', grants_usd: '未用额度 USD', median_session_p50_ms: '会话 p50 中位数 ms', p90_session_p90_ms: '会话 p90 的 p90 ms' }
const routeLabel: Record<string, string> = { training: '训练', standard: '不训练', unknown: '未知' }
const fundingLabel: Record<string, string> = { gift: '赠送', paid: '付费', unknown: '未知' }
const planLabel: Record<string, string> = { free: 'Free', pro: 'Pro', unknown: '未知' }
const kindLabel: Record<string, string> = { topup: '充值', membership: '会员' }
const funnelStages = ['registered', 'redeemed', 'first_session', 'one_hour', 'first_topup', 'second_topup']
const financeSeries: Series[] = [
  { key: 'topup_usd', label: '充值', slot: 1 },
  { key: 'membership_usd', label: '会员', slot: 2 },
  { key: 'refund_usd', label: '退款', slot: 3 },
]
const latencySeries: Series[] = [
  { key: 'median_session_p50_ms', label: 'p50 中位数', slot: 1 },
  { key: 'p90_session_p90_ms', label: 'p90 的 p90', slot: 2 },
]

const display = (value: unknown) => value == null ? '—' : typeof value === 'number' ? number(value).toLocaleString(undefined, { maximumFractionDigits: 3 }) : String(value)
const text = (value: unknown) => value == null ? '' : String(value)
const isoDate = (offsetDays: number) => new Date(Date.now() + offsetDays * 86400000).toISOString().slice(0, 10)
const sum = (rows: Row[], field: string) => rows.reduce((value, row) => value + number(row[field]), 0)

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

function Section({ title, description, rows, exportName, exportAllowed, children }: { title: string; description: string; rows: Row[]; exportName: string; exportAllowed: boolean; children: ReactNode }) {
  const keys = [...new Set(rows.flatMap(row => Object.keys(row)))]
  return (
    <section className="pa-card pa-section">
      <div className="pa-section__heading">
        <div>
          <h2>{title}</h2>
          <p>{description}</p>
        </div>
        {exportAllowed && rows.length > 0 && (
          <button className="pa-button" type="button" onClick={() => downloadConsoleCSV(exportName, [keys.map(key => labels[key] ?? key), ...rows.map(row => keys.map(key => row[key] ?? ''))])}>下载 CSV</button>
        )}
      </div>
      {children}
      {rows.length > 0 && (
        <details className="pa-details">
          <summary>查看全部数值（{rows.length} 行）</summary>
          <div className="pa-table-wrap">
            <table className="pa-table pa-table--numeric">
              <thead><tr>{keys.map(key => <th key={key}>{labels[key] ?? key}</th>)}</tr></thead>
              <tbody>{rows.map((row, index) => <tr key={index}>{keys.map(key => <td key={key}>{display(row[key])}</td>)}</tr>)}</tbody>
            </table>
          </div>
        </details>
      )}
    </section>
  )
}

/** Side-by-side facets for measures that do not share a unit. */
function Pair({ children }: { children: ReactNode }) {
  return <div className="viz-pair">{children}</div>
}

function Facet({ title, children }: { title: string; children: ReactNode }) {
  return <div className="viz-facet"><h3>{title}</h3>{children}</div>
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

  const appliedGrain = typeof data?.granularity === 'string' ? data.granularity : grain
  const rowsFor = (key: string): Row[] => Array.isArray(data?.[key]) ? data[key] as Row[] : []
  const has = (key: string) => Array.isArray(data?.[key])
  const period = (value: unknown) => periodLabel(value, appliedGrain)
  const exportAllowed = data?.export_allowed === true
  const credit = data?.credit

  const activity = rowsFor('activity')
  const finance = rowsFor('finance')
  const funnel = rowsFor('funnel')
  const retention = rowsFor('retention')
  const histogram = rowsFor('hours_histogram')
  const routing = rowsFor('routing')
  const costs = rowsFor('costs')
  const paths = rowsFor('paths')
  const tiers = rowsFor('income_tiers')
  const liabilities = rowsFor('liabilities')[0]
  const latency = rowsFor('latency')
  const edits = rowsFor('edits')
  const languages = rowsFor('languages')

  const funnelChannels = [...funnel].sort((a, b) => number(b.registered) - number(a.registered)).slice(0, 6)
  const retentionBatches = [...new Set(retention.map(row => text(row.batch_id)))]
  const retentionWeeks = [...new Set(retention.map(row => number(row.week)))].sort((a, b) => a - b)
  const latencyRoutes = [...new Set(latency.map(row => text(row.route)))]
  const tierItems = Object.values(tiers.reduce<Record<string, { kind: string; tier: number; revenue: number; payments: number }>>((acc, row) => {
    const key = `${text(row.kind)}:${number(row.tier_usd)}`
    acc[key] ??= { kind: text(row.kind), tier: number(row.tier_usd), revenue: 0, payments: 0 }
    acc[key].revenue += number(row.revenue_usd)
    acc[key].payments += number(row.payments)
    return acc
  }, {})).sort((a, b) => a.kind.localeCompare(b.kind) || a.tier - b.tier)
  const creditRatio = credit?.configured ? Math.min(1, Math.max(0, credit.remaining_usd ?? 0) / (credit.starting_usd || 1)) : 0

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
        <div className={`pa-stack${busy ? ' is-refreshing' : ''}`}>
          <div className="pa-dashboard-kpis">
            <Kpi label="活跃用户" value={display(activity.at(-1)?.active_users)} hint="最后一个有用量的周期">
              <Sparkline values={activity.map(row => number(row.active_users))} />
            </Kpi>
            <Kpi label="转录小时" value={formatCompact(sum(activity, 'hours'))} hint="所选期间合计">
              <Sparkline values={activity.map(row => number(row.hours))} />
            </Kpi>
            {data.financial && (
              <Kpi label="实收收入" value={formatUsd(sum(finance, 'topup_usd') + sum(finance, 'membership_usd') + sum(finance, 'refund_usd'))} hint="所选期间，已扣退款；手续费另列">
                <Sparkline values={finance.map(row => number(row.topup_usd) + number(row.membership_usd) + number(row.refund_usd))} />
              </Kpi>
            )}
            {credit && (
              <Kpi
                label="Speechmatics 额度"
                value={credit.configured ? formatUsd(credit.remaining_usd ?? 0) : '待配置'}
                hint={credit.configured ? `预计剩余 ${credit.days_remaining == null ? '—' : display(credit.days_remaining)} 天 · ${routeLabel[credit.route] ?? credit.route}账户` : '在分流与额度页填写起算余额'}
              >
                {credit.configured && (
                  <span className="viz-meter" role="img" aria-label={`剩余 ${formatPercent(creditRatio)}`}>
                    <i style={{ width: `${creditRatio * 100}%` }} />
                  </span>
                )}
              </Kpi>
            )}
            {liabilities && (
              <Kpi label="用户余额负债" value={formatUsd(number(liabilities.wallet_usd) + number(liabilities.grants_usd))} hint={`钱包 ${formatUsd(number(liabilities.wallet_usd))} · 未过期赠送 ${formatUsd(number(liabilities.grants_usd))}`} />
            )}
          </div>

          {has('activity') && (
            <Section title="活跃与转录时长" description="按所选粒度统计，每段时间内的用户去重。" rows={activity} exportName={`activity-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <Pair>
                <Facet title="转录小时">
                  <ColumnChart rows={activity} series={[{ key: 'hours', label: '小时' }]} xKey="period" xLabel={period} format={value => formatCompact(value)} title="转录小时" />
                </Facet>
                <Facet title="活跃用户">
                  <ColumnChart rows={activity} series={[{ key: 'active_users', label: '活跃用户' }]} xKey="period" xLabel={period} format={value => formatCompact(value)} title="活跃用户" />
                </Facet>
              </Pair>
            </Section>
          )}

          {has('funnel') && (
            <Section title="渠道转化漏斗" description="所选期间注册的用户，截至结束日的转化；兑换批次优先归因，其次注册推广渠道。未领码的充值用户也会计入充值阶段。" rows={funnel} exportName={`funnel-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              {funnelChannels.length === 0 ? <p className="pa-chart-empty">所选范围内暂无数据</p> : (
                <>
                  <div className="viz-grid">
                    {funnelChannels.map(row => {
                      const registered = number(row.registered)
                      return (
                        <Facet key={text(row.channel)} title={`${text(row.channel) || '—'} · ${registered} 人注册`}>
                          <BarList
                            max={registered}
                            format={value => formatCompact(value)}
                            items={funnelStages.map((stage, index) => ({
                              key: stage,
                              label: labels[stage],
                              value: number(row[stage]),
                              display: `${number(row[stage])}${registered ? ` · ${formatPercent(number(row[stage]) / registered)}` : ''}`,
                              color: ordinalColor(index, funnelStages.length),
                            }))}
                          />
                        </Facet>
                      )
                    })}
                  </div>
                  {funnel.length > funnelChannels.length && <p className="pa-form-note">仅显示注册最多的 {funnelChannels.length} 个渠道，其余见表格。</p>}
                </>
              )}
            </Section>
          )}

          {has('retention') && (
            <Section title="兑换批次留存" description="兑换后第 1 / 2 / 4 周仍有转录用量的账户占比；只计入已完整经过该周的用户。" rows={retention} exportName={`retention-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <Heatmap
                caption="兑换批次留存率"
                rowLabels={retentionBatches.map(batch => `${batch.slice(0, 8)} · ${text(retention.find(row => text(row.batch_id) === batch)?.channel)}`)}
                colLabels={retentionWeeks.map(week => `第 ${week} 周`)}
                values={retentionBatches.map(batch => retentionWeeks.map(week => {
                  const row = retention.find(item => text(item.batch_id) === batch && number(item.week) === week)
                  return row && number(row.eligible) > 0 ? number(row.retained) / number(row.eligible) : null
                }))}
                format={formatPercent}
              />
            </Section>
          )}

          {has('hours_histogram') && (
            <Section title="每人每周使用时长" description="包含零用量用户周；首尾周按所选日期截取。桶单位为小时。" rows={histogram} exportName={`hours-histogram-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <BarList
                format={value => `${formatCompact(value)} 用户周`}
                items={histogram.map((row, index) => ({ key: text(row.bucket), label: `${text(row.bucket)} 小时`, value: number(row.user_weeks), color: ordinalColor(index, histogram.length) }))}
              />
            </Section>
          )}

          {has('routing') && (
            <Section title="训练、资金来源与组织类型" description="按每笔转录的路由快照分组的转录小时。历史缺少快照的记录单独显示为未知。" rows={routing} exportName={`routing-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <BarList
                format={value => `${formatCompact(value)} 小时`}
                items={[...routing].sort((a, b) => number(b.hours) - number(a.hours)).map(row => ({
                  key: `${text(row.route)}/${text(row.funding)}/${text(row.tenant_kind)}`,
                  label: <>{routeLabel[text(row.route)] ?? text(row.route)} · {fundingLabel[text(row.funding)] ?? text(row.funding)}<small>{text(row.tenant_kind) || '个人'} · {number(row.users)} 人</small></>,
                  value: number(row.hours),
                }))}
              />
            </Section>
          )}

          {has('costs') && (
            <Section title="用量成本与毛利" description="付费用量毛利 = 扣费减赠送部分、会话结束退差额与上游成本；不等于现金利润，不含会员收入与支付手续费。" rows={costs} exportName={`costs-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <Legend kind="line" series={[{ key: 'paid_consumed_usd', label: '付费消耗' }, { key: 'upstream_usd', label: '上游成本' }]} />
              <LineChart
                rows={costs}
                series={[{ key: 'paid_consumed_usd', label: '付费消耗' }, { key: 'upstream_usd', label: '上游成本' }]}
                xKey="period" xLabel={period} format={formatUsd} title="付费消耗与上游成本"
                extra={row => [{ label: '用量毛利', value: formatUsd(number(row.paid_usage_margin_usd)) }, { label: '赠送消耗', value: formatUsd(number(row.gift_usd)) }]}
              />
            </Section>
          )}

          {has('paths') && (
            <Section title="四条转录路径的每小时毛利" description="使用发生时的 Free / Pro 与训练路由。没有用量的路径显示空值，赠送消耗不当作收入。" rows={paths} exportName={`paths-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <BarList
                format={formatUsd}
                items={paths.map(row => {
                  const margin = row.margin_per_hour_usd
                  return {
                    key: `${text(row.plan)}/${text(row.route)}`,
                    label: <>{planLabel[text(row.plan)] ?? text(row.plan)} · {routeLabel[text(row.route)] ?? text(row.route)}<small>{formatCompact(number(row.hours))} 小时</small></>,
                    value: number(margin),
                    display: margin == null ? '无用量' : `${formatUsd(number(margin))} / 小时`,
                  }
                })}
              />
            </Section>
          )}

          {has('finance') && (
            <Section title="收入、退款与手续费" description="Stripe 实收交易按记账日期统计；退款显示在基线以下。待补手续费表示尚未取到实际手续费的笔数。" rows={finance} exportName={`finance-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <Legend series={financeSeries} />
              <ColumnChart
                stacked rows={finance} series={financeSeries}
                xKey="period" xLabel={period} format={formatUsd} title="收入与退款"
                extra={row => [{ label: '已知手续费', value: formatUsd(number(row.known_fee_usd)) }, { label: '待补手续费', value: `${number(row.pending_fees)} 笔` }]}
              />
            </Section>
          )}

          {has('income_tiers') && (
            <Section title="收入与充值档位" description="所选期间按实际支付金额分档的收入，会员付款单独列出；按周拆分见表格。" rows={tiers} exportName={`income-tiers-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <BarList
                format={formatUsd}
                items={tierItems.map(item => ({
                  key: `${item.kind}:${item.tier}`,
                  label: <>{kindLabel[item.kind] ?? item.kind} {formatUsd(item.tier)}<small>{item.payments} 笔</small></>,
                  value: item.revenue,
                }))}
              />
            </Section>
          )}

          {has('latency') && (
            <Section title="转录延迟" description="会话 p50 的中位数与会话 p90 的第 90 百分位，单位毫秒；这是会话级百分位，不是全局逐句百分位。长会话保留最近 8192 个样本。" rows={latency} exportName={`latency-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              {latencyRoutes.length === 0 ? <p className="pa-chart-empty">所选范围内暂无数据</p> : (
                <>
                  <Legend kind="line" series={latencySeries} />
                  <Pair>
                    {latencyRoutes.map(route => (
                      <Facet key={route} title={`${routeLabel[route] ?? route}账户`}>
                        <LineChart
                          rows={latency.filter(row => text(row.route) === route)} series={latencySeries}
                          xKey="period" xLabel={period} format={value => `${formatCompact(value)} ms`} title={`${routeLabel[route] ?? route} 延迟`}
                          extra={row => [{ label: '会话数', value: display(row.sessions) }]}
                        />
                      </Facet>
                    ))}
                  </Pair>
                </>
              )}
            </Section>
          )}

          {has('edits') && (
            <Section title="转录修改比例" description="定稿后被修改过的段落占定稿段数的比例。相同内容重传、局部结果更新与仅翻译变化不计入。" rows={edits} exportName={`edits-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <BarList
                max={1}
                format={formatPercent}
                items={[...edits].sort((a, b) => number(b.final_segments) - number(a.final_segments)).map(row => {
                  const finals = number(row.final_segments)
                  return {
                    key: text(row.source_language),
                    label: <>{text(row.source_language) || '未知'}<small>{formatCompact(finals)} 段定稿 · {formatCompact(number(row.edits))} 次修改</small></>,
                    value: finals ? number(row.edited_segments) / finals : 0,
                  }
                })}
              />
            </Section>
          )}

          {has('languages') && (
            <Section title="翻译目标语言与模型成本" description="按关联会话的目标语言汇总 token 和上游成本，缺失会话的记录显示为未知。" rows={languages} exportName={`languages-${from}-${to}.csv`} exportAllowed={exportAllowed}>
              <BarList
                format={formatUsd}
                items={[...languages].sort((a, b) => number(b.upstream_usd) - number(a.upstream_usd)).map(row => ({
                  key: `${text(row.language)}/${text(row.model)}`,
                  label: <>{text(row.language) || '未知'}<small>{text(row.model)} · {formatCompact(number(row.input_tokens) + number(row.output_tokens))} token</small></>,
                  value: number(row.upstream_usd),
                }))}
              />
            </Section>
          )}
        </div>
      )}
    </div>
  )
}
