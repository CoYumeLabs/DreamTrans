import { useCallback, useEffect, useState } from 'react'
import { listTenants, updateTenant, type Tenant } from '../../admin/api'
import { type Runner } from './shared'
import { Pagination } from './ui'

export function TenantsPage({ run }: { run: Runner }) {
  const pageSize = 20
  const [tenants, setTenants] = useState<Tenant[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    setLoading(true)
    const result = await run(() => listTenants(page, pageSize))
    if (result) {
      setTenants(result.tenants)
      setTotal(result.total)
    }
    setLoading(false)
  }, [page, run])

  useEffect(() => { void load() }, [load])

  return (
    <section className="pa-card pa-section">
      <div className="pa-section__heading"><div><h2>组织与配额</h2><p>组织套餐只影响知识库存储配额；用户的计费与会员在“客户”页管理。机构类型的组织永远走未开启训练的 Speechmatics 账号，任何用户设置都盖不过。</p></div></div>
      <div className="pa-table-wrap"><table>
        <thead><tr><th>组织</th><th>套餐</th><th>类型</th><th>Speechmatics 账号</th><th>知识库存储</th></tr></thead>
        <tbody>
          {loading && <tr><td className="pa-table-empty" colSpan={5}>正在加载组织…</td></tr>}
          {!loading && tenants.length === 0 && <tr><td className="pa-table-empty" colSpan={5}>当前页没有组织。</td></tr>}
          {!loading && tenants.map((tenant) => (
            <tr key={tenant.id}>
              <td><strong>{tenant.name}</strong><small>{tenant.slug}</small></td>
              <td><select value={tenant.plan} onChange={(event) => void run(async () => {
                await updateTenant(tenant.id, { plan: event.target.value })
                await load()
              }, '套餐已更新')}>
                <option value="free">Free</option><option value="pro">Pro</option><option value="enterprise">Enterprise</option>
              </select></td>
              <td><select aria-label={`${tenant.name} 类型`} value={tenant.kind ?? 'personal'} onChange={(event) => void run(async () => {
                await updateTenant(tenant.id, { kind: event.target.value as Tenant['kind'] })
                await load()
              }, '组织类型已更新')}>
                <option value="personal">个人</option><option value="institution">机构（不训练）</option>
              </select></td>
              <td><select aria-label={`${tenant.name} Speechmatics 账号`} value={tenant.speechmatics_route ?? ''} onChange={(event) => void run(async () => {
                await updateTenant(tenant.id, { speechmatics_route: event.target.value as Tenant['speechmatics_route'] })
                await load()
              }, '账号路由已更新')}>
                <option value="">按规则</option><option value="standard">强制不训练</option><option value="training">强制训练</option>
              </select></td>
              <td>{tenant.storage_quota_gb} GB</td>
            </tr>
          ))}
        </tbody>
      </table></div>
      <Pagination page={page} pageSize={pageSize} total={total} onChange={setPage} />
    </section>
  )
}
