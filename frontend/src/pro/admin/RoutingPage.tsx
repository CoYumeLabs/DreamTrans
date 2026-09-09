import { useEffect, useState } from 'react'
import { adminFetch } from '../../admin/api'
import { ErrorBanner } from './ui'

interface Credit { starting_usd: number; started_at: string; route: string }

export function RoutingPage({ writable }: { writable: boolean }) {
  const [amount, setAmount] = useState(2000)
  const [started, setStarted] = useState('')
  const [route, setRoute] = useState('training')
  const [userId, setUserId] = useState('')
  const [userRoute, setUserRoute] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    void adminFetch<{ credit?: Credit }>('/api/admin/routing')
      .then(({ credit }) => {
        if (!credit) return
        setAmount(credit.starting_usd)
        setStarted(credit.started_at ? credit.started_at.slice(0, 16) : '')
        setRoute(credit.route)
      })
      .catch(e => setError(e instanceof Error ? e.message : '读取失败'))
  }, [])

  async function save(body: unknown, saved: string) {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await adminFetch('/api/admin/routing', { method: 'PUT', body: JSON.stringify(body) })
      setNotice(saved)
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="pa-stack">
      <ErrorBanner message={error} onClose={() => setError('')} />
      {notice && <div className="pa-banner pa-banner--success" role="status">{notice}</div>}

      <fieldset className="pa-permission-scope pa-stack" disabled={!writable || busy}>
        <section className="pa-card pa-section">
          <div className="pa-section__heading">
            <div>
              <h2>Speechmatics 额度起点</h2>
              <p>填写该上游账户在起算时刻的真实余额，系统扣除此后已记账的上游费用来估算剩余额度和可用天数，结果显示在概览页。这里只是估算用的参照值，不会改变任何路由。</p>
            </div>
          </div>
          <form className="pa-form-grid" onSubmit={e => { e.preventDefault(); void save({ credit_usd: amount, started_at: started ? `${started}:00Z` : '', route }, '额度起点已保存，只影响概览页的剩余额度估算，不改变任何账户的路由') }}>
            <label><span>起始余额 USD</span><input type="number" min="0" max="10000000" step="0.01" value={amount} onChange={e => setAmount(Number(e.target.value))} required /></label>
            <label><span>起算时间（UTC，留空暂停估算）</span><input type="datetime-local" value={started} onChange={e => setStarted(e.target.value)} /></label>
            <label>
              <span>上游账户</span>
              <select value={route} onChange={e => setRoute(e.target.value)}>
                <option value="training">训练</option>
                <option value="standard">不训练</option>
              </select>
            </label>
            <div className="pa-form-grid__footer">
              <span />
              <button className="pa-button pa-button--primary" type="submit">保存额度配置</button>
            </div>
          </form>
        </section>

        <section className="pa-card pa-section">
          <div className="pa-section__heading">
            <div>
              <h2>指定账户路由</h2>
              <p>机构与赠送余额的保护规则优先于这里的指定；指定为训练时仍需该用户本人已加入训练计划，未同意的用户会按不训练处理。留空可恢复自动选择。</p>
            </div>
          </div>
          <form className="pa-form-grid" onSubmit={e => { e.preventDefault(); void save({ user_id: userId, route: userRoute }, '账户路由已保存，该用户之后开始的会话按新路由执行') }}>
            <label><span>用户邮箱或 ID</span><input required placeholder="name@example.com" value={userId} onChange={e => setUserId(e.target.value)} /></label>
            <label>
              <span>路由</span>
              <select value={userRoute} onChange={e => setUserRoute(e.target.value)}>
                <option value="">自动</option>
                <option value="standard">不训练</option>
                <option value="training">训练</option>
              </select>
            </label>
            <div className="pa-form-grid__footer">
              <span />
              <button className="pa-button pa-button--primary" type="submit">保存账户路由</button>
            </div>
          </form>
        </section>
      </fieldset>
    </div>
  )
}
