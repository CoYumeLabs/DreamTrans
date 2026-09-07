import { downloadConsoleCSV } from './csv'
import { useCallback, useEffect, useState } from 'react'
import { adminFetch } from '../../admin/api'
import { ErrorBanner, Pagination } from './ui'


interface Code { id: string; code: string; batch_id: string; channel: string; face_value_usd: number; expires_at: string; status: string }
interface Audit { id: string; created_at: string; actor: string; action: string; target_type: string; target_id: string; details: unknown }

export function OperationsPage({ mode, writable = true, canExport = true }: { mode: 'codes' | 'audit'; writable?: boolean; canExport?: boolean }) {
  const [page, setPage] = useState(1), [search, setSearch] = useState(''), [query, setQuery] = useState('')
  const [codes, setCodes] = useState<Code[]>([]), [entries, setEntries] = useState<Audit[]>([]), [total, setTotal] = useState(0)
  const [error, setError] = useState(''), [busy, setBusy] = useState(false), [generated, setGenerated] = useState<string[]>([])
  const [quantity, setQuantity] = useState(10), [amount, setAmount] = useState(10), [days, setDays] = useState(30), [channel, setChannel] = useState(''), [tags, setTags] = useState('')
  const [expiry, setExpiry] = useState(() => new Date(Date.now() + 30 * 86400000).toISOString().slice(0, 10))
  const [generatedTerms, setGeneratedTerms] = useState({ amount: 0, channel: '' })
  const [requestId, setRequestId] = useState(() => crypto.randomUUID())
  const load = useCallback(async () => {
    try {
      const result = await adminFetch<{ codes?: Code[]; entries?: Audit[]; total: number }>(`/api/admin/${mode === 'codes' ? 'redeem-codes' : 'audit'}?page=${page}&search=${encodeURIComponent(query)}`)
      setCodes(result.codes ?? []); setEntries(result.entries ?? []); setTotal(result.total); setError('')
    } catch (reason) { setError(reason instanceof Error ? reason.message : '读取失败') }
  }, [mode, page, query])
  useEffect(() => { void load() }, [load])
  async function create() {
    if (busy) return
    setBusy(true); setError('')
    try {
      const result = await adminFetch<{ codes: string[] }>('/api/admin/redeem-codes', { method: 'POST', body: JSON.stringify({ client_request_id: requestId, quantity, face_value_usd: amount, grant_days: days, channel, tags: tags.split(',').map(tag => tag.trim()).filter(Boolean), expires_at: `${expiry}T23:59:59Z` }) })
      setGenerated(result.codes); setGeneratedTerms({ amount, channel }); setRequestId(crypto.randomUUID()); await load()
    } catch (reason) { setError(reason instanceof Error ? reason.message : '生成失败') }
    finally { setBusy(false) }
  }
  async function revoke(id: string) {
    setBusy(true)
    try { await adminFetch(`/api/admin/redeem-codes/${id}`, { method: 'DELETE' }); await load() }
    catch (reason) { setError(reason instanceof Error ? reason.message : '作废失败') }
    finally { setBusy(false) }
  }
  return <div className="pa-stack">
    <ErrorBanner message={error} onClose={() => setError('')} />
    {mode === 'codes' && writable && <section className="pa-card">
      <h2>批量生成一次性兑换码</h2><p>每个账户仅可兑换一次，需先验证邮箱。赠送额度按标准价计费，不适用训练计划折扣。</p>
      <form className="pa-form-grid" onSubmit={event => { event.preventDefault(); void create() }}>
        <label>数量<input type="number" min="1" max="1000" value={quantity} onChange={event => setQuantity(Number(event.target.value))} required /></label>
        <label>每码面值（USD）<input type="number" min="0.01" max="10000" step="0.01" value={amount} onChange={event => setAmount(Number(event.target.value))} required /></label>
        <label>来源渠道<input maxLength={100} value={channel} onChange={event => setChannel(event.target.value)} required /></label>
        <label>标签（逗号分隔）<input value={tags} onChange={event => setTags(event.target.value)} /></label>
        <label>兑换截止日期（UTC）<input type="date" value={expiry} onChange={event => setExpiry(event.target.value)} required /></label>
        <label>兑换后额度有效天数<input type="number" min="1" max="3650" value={days} onChange={event => setDays(Number(event.target.value))} required /></label>
        <p>总赠送面额：${(quantity * amount).toFixed(2)}</p><button className="pa-button pa-button--primary" disabled={busy} type="submit">生成兑换码</button>
      </form>
      {generated.length > 0 && <div><p role="status">已生成 {generated.length} 个兑换码</p><button className="pa-button" onClick={() => downloadConsoleCSV('redeem-codes.csv', [['兑换码', '面值 USD', '渠道'], ...generated.map(code => [code, generatedTerms.amount, generatedTerms.channel])])}>下载本批 CSV</button><textarea aria-label="生成的兑换码" readOnly value={generated.join('\n')} rows={5} /></div>}
    </section>}
    <section className="pa-card"><form className="pa-toolbar" onSubmit={event => { event.preventDefault(); setPage(1); setQuery(search) }}><input aria-label="搜索" placeholder={mode === 'codes' ? '兑换码或渠道' : '操作、对象或管理员邮箱'} value={search} onChange={event => setSearch(event.target.value)} /><button className="pa-button" type="submit">搜索</button><button className="pa-button" type="button" onClick={() => { void load() }}>刷新</button><button hidden={!canExport} className="pa-button" type="button" onClick={() => downloadConsoleCSV(`${mode}.csv`, mode === 'codes' ? [['兑换码', '渠道', '面值 USD', '截止日期', '状态'], ...codes.map(code => [code.code, code.channel, code.face_value_usd, code.expires_at, code.status])] : [['时间', '管理员', '操作', '对象', '详情'], ...entries.map(entry => [entry.created_at, entry.actor, entry.action, entry.target_id, entry.details])])}>导出当前页 CSV</button></form>
      <div className="pa-table-wrap"><table className="pa-table"><thead><tr>{(mode === 'codes' ? ['兑换码', '渠道', '面值 USD', '截止日期', '状态', '操作'] : ['时间', '管理员', '操作', '对象', '详情']).map(label => <th key={label}>{label}</th>)}</tr></thead><tbody>
        {mode === 'codes' ? codes.map(code => <tr key={code.id}><td><code>{code.code}</code></td><td>{code.channel}</td><td>${code.face_value_usd.toFixed(2)}</td><td>{new Date(code.expires_at).toLocaleDateString()}</td><td>{{ available: '可兑换', redeemed: '已兑换', voided: '已作废', expired: '已过期' }[code.status]}</td><td>{writable && code.status === 'available' && <button disabled={busy} className="pa-button" onClick={() => { void revoke(code.id) }}>作废</button>}</td></tr>) : entries.map(entry => <tr key={entry.id}><td>{new Date(entry.created_at).toLocaleString()}</td><td>{entry.actor || '已删除账户'}</td><td>{entry.action}</td><td>{entry.target_id}</td><td><details><summary>查看详情</summary><pre>{JSON.stringify(entry.details, null, 2)}</pre></details></td></tr>)}
      </tbody></table></div><Pagination page={page} pageSize={50} total={total} onChange={setPage} />
    </section>
  </div>
}
