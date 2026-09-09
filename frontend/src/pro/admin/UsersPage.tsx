import { useCallback, useEffect, useState, type FormEvent } from 'react'
import {
  createUser,
  formatUSD,
  getSystemSettings,
  listAllTenants,
  listUsers,
  updateUser,
  type Tenant,
  type User,
} from '../../admin/api'
import { CustomersPage } from './CustomersPage'
import { formatDate, type Runner } from './shared'
import { Modal, Pagination } from './ui'

interface CreateUserDraft {
  email: string
  name: string
  password: string
  role: 'user' | 'admin'
  tenant_id: string
  initial_credit_usd: string
}

const roleLabels: Record<User['role'], string> = {
  user: '用户',
  admin: '管理员',
  super_admin: '超级管理员',
}

/**
 * The single user-administration tab. Super admins get the customers table
 * (accounts and billing merged: balances, plan, ledger, plus account
 * enable/disable inside the detail view); regular admins keep the plain
 * account list, since billing is super-admin only.
 */
export function UsersPage({
  isSuper,
  run,
}: {
  isSuper: boolean
  run: Runner
}) {
  const [showCreate, setShowCreate] = useState(false)
  const [tenants, setTenants] = useState<Tenant[]>([])
  const [trialDefault, setTrialDefault] = useState(1)
  // Remounts the customers list after a create so the new user shows up.
  const [listGeneration, setListGeneration] = useState(0)
  const [draft, setDraft] = useState<CreateUserDraft>({
    email: '',
    name: '',
    password: '',
    role: 'user',
    tenant_id: '',
    initial_credit_usd: '1',
  })

  useEffect(() => {
    if (!isSuper) return
    void run(() => Promise.all([listAllTenants(), getSystemSettings()])).then((result) => {
      if (!result) return
      const [allTenants, settings] = result
      setTenants(allTenants)
      setTrialDefault(settings.values.trial_credit_usd)
      setDraft((current) => ({
        ...current,
        tenant_id: current.tenant_id || allTenants[0]?.id || '',
        initial_credit_usd: String(settings.values.trial_credit_usd),
      }))
    })
  }, [isSuper, run])

  function openCreate() {
    setDraft({
      email: '',
      name: '',
      password: '',
      role: 'user',
      tenant_id: tenants[0]?.id || '',
      initial_credit_usd: String(trialDefault),
    })
    setShowCreate(true)
  }

  async function submitCreate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const initialCredit = Number(draft.initial_credit_usd)
    if (isSuper && (!draft.tenant_id || !Number.isFinite(initialCredit) || initialCredit < 0)) return
    const created = await run(() => createUser({
      email: draft.email.trim(),
      name: draft.name.trim(),
      password: draft.password,
      role: draft.role,
      ...(isSuper ? {
        tenant_id: draft.tenant_id,
        initial_credit_usd: initialCredit,
      } : {}),
    }), '用户已创建')
    if (created) {
      setShowCreate(false)
      setListGeneration((value) => value + 1)
    }
  }

  const createButton = (
    <button className="pa-button pa-button--primary" onClick={openCreate} type="button">创建用户</button>
  )

  return (
    <>
      {isSuper
        ? <CustomersPage actions={createButton} key={listGeneration} run={run} />
        : <BasicUserList actions={createButton} listGeneration={listGeneration} run={run} />}

      {showCreate && (
        <Modal
          footer={(
            <>
              <button className="pa-button pa-button--quiet" onClick={() => setShowCreate(false)} type="button">取消</button>
              <button className="pa-button pa-button--primary" form="pa-create-user" type="submit">创建用户</button>
            </>
          )}
          onClose={() => setShowCreate(false)}
          title="创建用户"
        >
          <form className="pa-dialog-form" id="pa-create-user" onSubmit={submitCreate}>
            <label><span>邮箱</span><input autoFocus onChange={(event) => setDraft({ ...draft, email: event.target.value })} required type="email" value={draft.email} /></label>
            <label><span>姓名</span><input maxLength={100} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="留空时使用邮箱前缀" value={draft.name} /></label>
            <label><span>初始密码</span><input maxLength={72} minLength={10} onChange={(event) => setDraft({ ...draft, password: event.target.value })} placeholder="10–72 个字符" required type="password" value={draft.password} /></label>
            <label><span>角色</span><select onChange={(event) => setDraft({ ...draft, role: event.target.value as CreateUserDraft['role'] })} value={draft.role}>
              <option value="user">用户</option>
              {isSuper && <option value="admin">管理员</option>}
            </select></label>
            {isSuper && (
              <>
                <label><span>所属组织</span><select onChange={(event) => setDraft({ ...draft, tenant_id: event.target.value })} required value={draft.tenant_id}>
                  <option disabled value="">请选择组织</option>
                  {tenants.map((tenant) => <option key={tenant.id} value={tenant.id}>{tenant.name} ({tenant.slug})</option>)}
                </select></label>
                <label>
                  <span>初始试用额度（USD，单次覆盖）</span>
                  <input min="0" onChange={(event) => setDraft({ ...draft, initial_credit_usd: event.target.value })} required step="0.01" type="number" value={draft.initial_credit_usd} />
                  <small>系统注册默认值为 {formatUSD(trialDefault)}；这里只影响本次创建，额度按系统设置的试用有效期过期。</small>
                </label>
              </>
            )}
            {!isSuper && <p className="pa-form-note">初始试用额度使用系统注册默认值，普通管理员不能覆盖。</p>}
          </form>
        </Modal>
      )}
    </>
  )
}

/** The plain account list for regular admins, who cannot see billing. */
function BasicUserList({
  run,
  actions,
  listGeneration,
}: {
  run: Runner
  actions: React.ReactNode
  listGeneration: number
}) {
  const pageSize = 20
  const [users, setUsers] = useState<User[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    setLoading(true)
    const result = await run(() => listUsers(page, pageSize))
    if (result) {
      setUsers(result.users)
      setTotal(result.total)
    }
    setLoading(false)
  }, [page, run])

  useEffect(() => { void load() }, [load, listGeneration])

  return (
    <section className="pa-card pa-section">
      <div className="pa-section__heading">
        <div><h2>账户与权限</h2><p>普通管理员只能管理自己组织内的普通用户。</p></div>
        {actions}
      </div>
      <div className="pa-table-wrap">
        <table>
          <thead><tr><th>用户</th><th>角色</th><th>状态</th><th>邮箱</th><th>最近登录</th><th>操作</th></tr></thead>
          <tbody>
            {!loading && users.length === 0 && (
              <tr><td className="pa-table-empty" colSpan={6}>当前页没有用户。</td></tr>
            )}
            {loading && (
              <tr><td className="pa-table-empty" colSpan={6}>正在加载用户…</td></tr>
            )}
            {!loading && users.map((user) => (
              <tr key={user.id}>
                <td><strong>{user.name || '未命名'}</strong><small>{user.email}</small></td>
                <td><span className="pa-pill">{roleLabels[user.role] || user.role}</span></td>
                <td><span className={`pa-status ${user.is_active ? 'is-good' : 'is-muted'}`}>{user.is_active ? '启用' : '停用'}</span></td>
                <td>
                  <span
                    className={`pa-status ${user.email_verified ? 'is-good' : 'is-muted'}`}
                    title={user.email_verified ? '邮箱已验证' : '尚未点击验证邮件；未验证的账户不能登录，也不会获得试用额度'}
                  >
                    {user.email_verified ? '已验证' : '未验证'}
                  </span>
                </td>
                <td>{formatDate(user.last_login_at)}</td>
                <td className="pa-actions">
                  <button onClick={() => void run(async () => {
                    await updateUser(user.id, { is_active: !user.is_active })
                    await load()
                  }, user.is_active ? '用户已停用' : '用户已启用')} type="button">
                    {user.is_active ? '停用' : '启用'}
                  </button>
                  {!user.email_verified && (
                    <button onClick={() => void run(async () => {
                      await updateUser(user.id, { email_verified: true })
                      await load()
                    }, '已手动标记为邮箱已验证')} type="button">
                      标记已验证
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <Pagination page={page} pageSize={pageSize} total={total} onChange={setPage} />
    </section>
  )
}
