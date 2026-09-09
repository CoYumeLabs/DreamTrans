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

  async function save(body: unknown) {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await adminFetch('/api/admin/routing', { method: 'PUT', body: JSON.stringify(body) })
      setNotice('已保存，新会话按更新后的路由执行')
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
              <p>填写该上游账户在起算时刻的真实余额。系统扣除此后已记账的上游费用；未记录路由的历史用量无法归入账户。</p>
            </div>
          </div>
          <form className="pa-form-grid" onSubmit={e => { e.preventDefault(); void save({ credit_usd: amount, started_at: started ? `${started}:00Z` : '', route }) }}>
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
              <p>机构与赠送余额的保护规则优先于训练账号指定。留空指定可恢复自动选择。</p>
            </div>
          </div>
          <form className="pa-form-grid" onSubmit={e => { e.preventDefault(); void save({ user_id: userId, route: userRoute }) }}>
            <label><span>用户 ID</span><input required value={userId} onChange={e => setUserId(e.target.value)} /></label>
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
