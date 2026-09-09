import { useCallback, useEffect, useState } from 'react'
import { adminFetch } from '../../admin/api'
import { ErrorBanner } from './ui'

interface Role { id: string; key: string; name: string; permissions: string[]; channels: string[]; builtin: boolean }

const emptyRole: Role = { id: '', key: '', name: '', permissions: [], channels: [], builtin: false }

export function RolesPage({ writable }: { writable: boolean }) {
  const [roles, setRoles] = useState<Role[]>([])
  const [permissions, setPermissions] = useState<string[]>([])
  const [draft, setDraft] = useState<Role>(emptyRole)
  const [userId, setUserId] = useState('')
  const [roleId, setRoleId] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try {
      const result = await adminFetch<{ roles?: Role[]; permissions?: string[] }>('/api/admin/roles')
      setRoles(result.roles ?? [])
      setPermissions(result.permissions ?? [])
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取失败')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  async function mutate(path: string, method: string, body?: unknown) {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await adminFetch(path, { method, body: body ? JSON.stringify(body) : undefined })
      setNotice('已保存，权限在下次请求时生效')
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  const togglePermission = (permission: string, checked: boolean) => setDraft({
    ...draft,
    permissions: checked ? [...draft.permissions, permission] : draft.permissions.filter(value => value !== permission),
  })

  return (
    <div className="pa-stack">
      <ErrorBanner message={error} onClose={() => setError('')} />
      {notice && <div className="pa-banner pa-banner--success" role="status">{notice}</div>}

      <section className="pa-card pa-section">
        <div className="pa-section__heading">
          <div>
            <h2>角色与渠道范围</h2>
            <p>内置角色可复制。渠道留空代表全部渠道；有限渠道账号仅能访问支持渠道过滤的功能。动态角色不会授予超级管理员身份。</p>
          </div>
        </div>
        <div className="pa-table-wrap">
          <table className="pa-table">
            <thead><tr><th>角色</th><th>权限</th><th>渠道</th><th>操作</th></tr></thead>
            <tbody>
              {roles.length === 0 && <tr><td className="pa-table-empty" colSpan={4}>还没有角色</td></tr>}
              {roles.map(role => (
                <tr key={role.id}>
                  <td>{role.name}{role.builtin && <small className="pa-table-sub">内置</small>}</td>
                  <td><div className="pa-feature-pills">{role.permissions.map(permission => <span className="pa-pill" key={permission}>{permission}</span>)}</div></td>
                  <td>{role.channels.join(', ') || '全部'}</td>
                  <td>
                    {writable && role.key !== 'super_admin' && (
                      <div className="pa-toolbar">
                        <button className="pa-button pa-button--quiet" type="button" onClick={() => setDraft({ ...role, id: role.builtin ? '' : role.id, key: role.builtin ? `${role.key}_custom` : role.key, builtin: false })}>{role.builtin ? '复制' : '编辑'}</button>
                        {!role.builtin && <button className="pa-button pa-button--danger-quiet" disabled={busy} type="button" onClick={() => { if (window.confirm(`删除角色「${role.name}」？`)) void mutate(`/api/admin/roles/${role.id}`, 'DELETE') }}>删除</button>}
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      {writable && (
        <>
          <section className="pa-card pa-section">
            <div className="pa-section__heading">
              <div>
                <h2>{draft.id ? '编辑角色' : '创建角色'}</h2>
                <p>标识只能在创建时设置；权限勾选后在该角色的下一次请求生效。</p>
              </div>
              {(draft.id || draft.key) && <button className="pa-button" type="button" onClick={() => setDraft(emptyRole)}>新建</button>}
            </div>
            <form onSubmit={e => { e.preventDefault(); void mutate('/api/admin/roles', draft.id ? 'PUT' : 'POST', draft) }}>
              <fieldset className="pa-permission-scope" disabled={busy}>
                <div className="pa-form-grid">
                  <label><span>标识</span><input required maxLength={60} disabled={!!draft.id} value={draft.key} onChange={e => setDraft({ ...draft, key: e.target.value })} /></label>
                  <label><span>名称</span><input required maxLength={100} value={draft.name} onChange={e => setDraft({ ...draft, name: e.target.value })} /></label>
                  <label><span>允许的渠道（逗号分隔）</span><input value={draft.channels.join(',')} onChange={e => setDraft({ ...draft, channels: e.target.value.split(',').map(v => v.trim()).filter(Boolean) })} /></label>
                </div>
                <div className="pa-subsection">
                  <h3>权限</h3>
                  {permissions.length === 0 && <p className="pa-form-note">没有可分配的权限。</p>}
                  <div className="pa-checkbox-grid">
                    {permissions.map(permission => (
                      <label className="pa-checkbox" key={permission}>
                        <input type="checkbox" checked={draft.permissions.includes(permission)} onChange={e => togglePermission(permission, e.target.checked)} />
                        {permission}
                      </label>
                    ))}
                  </div>
                </div>
                <div className="pa-toolbar pa-button-row">
                  <button className="pa-button pa-button--primary" type="submit">保存角色</button>
                </div>
              </fieldset>
            </form>
          </section>

          <section className="pa-card pa-section">
            <div className="pa-section__heading">
              <div>
                <h2>分配或撤销后台角色</h2>
                <p>选择「撤销动态后台权限」会移除该账户的全部动态角色。</p>
              </div>
            </div>
            <form className="pa-form-grid" onSubmit={e => { e.preventDefault(); void mutate('/api/admin/roles/assign', 'POST', { user_id: userId, role_id: roleId }) }}>
              <label><span>账户 ID</span><input required value={userId} onChange={e => setUserId(e.target.value)} /></label>
              <label>
                <span>角色</span>
                <select value={roleId} onChange={e => setRoleId(e.target.value)}>
                  <option value="">撤销动态后台权限</option>
                  {roles.filter(role => role.key !== 'super_admin').map(role => <option key={role.id} value={role.id}>{role.name}</option>)}
                </select>
              </label>
              <div className="pa-form-grid__footer">
                <span />
                <button className="pa-button pa-button--primary" disabled={busy} type="submit">保存分配</button>
              </div>
            </form>
          </section>
        </>
      )}
    </div>
  )
}
