import { AgentsPage } from './admin/AgentsPage'
import { RolesPage } from './admin/RolesPage'
import { hasConsolePermission, type ConsoleAccess } from './admin/consoleAccess'
import { useCallback, useEffect, useState } from 'react'
import { initAuth, authFetch, AUTH_STATE_CHANGED_EVENT, getStoredUser, type User as AuthUser } from './api/auth'
import { OperationsPage } from './admin/OperationsPage'
import { CostsPage } from './admin/CostsPage'
import { ModelsPage } from './admin/ModelsPage'
import { DashboardPage } from './admin/DashboardPage'
import { RoutingPage } from './admin/RoutingPage'
import { SignupRiskPage } from './admin/SignupRiskPage'
import { AnnouncementsPage } from './admin/AnnouncementsPage'
import { PromotionsPage } from './admin/PromotionsPage'
import { PlansPage } from './admin/PlansPage'
import { SettingsPage } from './admin/SettingsPage'
import { TenantsPage } from './admin/TenantsPage'
import { UsersPage } from './admin/UsersPage'
import { errorMessage } from './admin/shared'
import { AdminIcon } from './admin/AdminIcon'
import { ErrorBanner } from './admin/ui'
import './pro-admin.css'

type Tab =
  | 'agents'
  | 'agent-self'
  | 'routing'
  | 'roles'
  | 'codes'
  | 'audit'
  | 'overview'
  | 'users'
  | 'plans'
  | 'promotions'
  | 'announcements'
  | 'signup-risk'
  | 'models'
  | 'tenants'
  | 'settings'

const nav: Array<{ id: Tab; label: string; permission: string; scoped?: boolean }> = [
  { id: 'overview', label: '概览', permission: 'dashboard.read', scoped: true },
  { id: 'users', label: '用户', permission: 'users.read' },
  { id: 'signup-risk', label: '注册风控', permission: 'risk.read' },
  { id: 'codes', label: '兑换码', permission: 'codes.read', scoped: true },
  { id: 'audit', label: '审计日志', permission: 'audit.read', scoped: true },
  { id: 'agents', label: '代理与结算', permission: 'finance.read' },
  { id: 'agent-self', label: '我的代理账户', permission: 'agent.self' },
  { id: 'routing', label: '分流与额度', permission: 'routing.read' },
  { id: 'roles', label: '角色权限', permission: 'roles.read' },
  { id: 'promotions', label: '推广邀请', permission: 'promotions.read', scoped: true },
  { id: 'announcements', label: '站内公告', permission: 'announcements.read' },
  { id: 'plans', label: '会员与充值', permission: 'pricing.read' },
  { id: 'models', label: '模型与定价', permission: 'models.read' },
  { id: 'tenants', label: '组织', permission: 'routing.read' },
  { id: 'settings', label: '系统设置', permission: 'settings.read' },
]

export default function ProAdmin() {
  const [viewer, setViewer] = useState<AuthUser | null>(null)
  const [ready, setReady] = useState(false)
  const [tab, setTab] = useState<Tab>('overview')
  const [busyCount, setBusyCount] = useState(0)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [access, setAccess] = useState<ConsoleAccess | null>(null)
  const can = (permission: string) => hasConsolePermission(access, permission)
  const visibleNav = nav.filter(item => can(item.permission) && (!access?.channels.length || item.scoped))
  const isSuper = viewer?.role === 'super_admin'

  useEffect(() => {
    let active = true
    let identity = getStoredUser()?.id
    void initAuth().then(async (user) => {
      if (!user) { window.location.href = '/pro'; return }
      const result = await authFetch<ConsoleAccess>('/api/admin/access')
      if (!active) return
      if (!result.allowed) { window.location.href = '/pro'; return }
      const first = nav.find(item => hasConsolePermission(result, item.permission) && (!result.channels.length || item.scoped))
      identity = user.id; setAccess(result); setViewer(user); setTab(first?.id ?? 'overview'); setReady(true)
    }).catch(() => { window.location.href = '/pro' })
    const onChange = () => { if (getStoredUser()?.id !== identity) window.location.reload() }
    window.addEventListener(AUTH_STATE_CHANGED_EVENT, onChange)
    return () => { active = false; window.removeEventListener(AUTH_STATE_CHANGED_EVENT, onChange) }
  }, [])

  const run = useCallback(async <T,>(
    operation: () => Promise<T>,
    success?: string,
    onError?: (message: string) => void,
  ) => {
    setBusyCount((value) => value + 1)
    setError('')
    try {
      const value = await operation()
      if (success) {
        setNotice(success)
        window.setTimeout(() => setNotice(''), 3000)
      }
      return value
    } catch (reason) {
      const message = errorMessage(reason)
      setError(message)
      onError?.(message)
      return undefined
    } finally {
      setBusyCount((value) => Math.max(0, value - 1))
    }
  }, [])

  function navigate(next: Tab) {
    setError('')
    setTab(next)
  }

  if (!ready || !viewer) {
    return <div className="pa-loading">正在验证管理员身份…</div>
  }

  return (
    <div className="pa-shell">
      <aside className="pa-sidebar">
        <a className="pa-brand" href="/pro">
          <span className="pa-brand__mark"><AdminIcon name="brand" /></span>
          <span><strong>Yufolo</strong><small>管理控制台</small></span>
        </a>
        <nav aria-label="管理导航">
          {visibleNav.map((item) => (
            <button
              className={tab === item.id ? 'is-active' : ''}
              aria-current={tab === item.id ? 'page' : undefined}
              key={item.id}
              onClick={() => navigate(item.id)}
              type="button"
            >
              <AdminIcon name={item.id} /><span>{item.label}</span>
            </button>
          ))}
        </nav>
        <div className="pa-sidebar__account">
          <span>{viewer.name?.slice(0, 1).toUpperCase() || 'A'}</span>
          <div><strong>{viewer.name || viewer.email}</strong><small>{access?.name}</small></div>
        </div>
      </aside>

      <main className="pa-main">
        <header className="pa-header">
          <div>
            <p>Yufolo <span>/</span> 管理后台</p>
            <h1>{nav.find((item) => item.id === tab)?.label}</h1>
          </div>
          <div className="pa-header__actions">
            {busyCount > 0 && <span className="pa-busy">正在处理…</span>}
            <a className="pa-button pa-button--quiet" href="/pro"><AdminIcon name="back" />返回工作台</a>
          </div>
        </header>
        <ErrorBanner message={error} onClose={() => setError('')} />
        {notice && <div className="pa-banner pa-banner--success">{notice}</div>}

        {(tab === 'codes' || tab === 'audit') && <OperationsPage key={tab} mode={tab} writable={can('codes.write')} canExport={can('export')} />}
        {tab === 'agents' && <AgentsPage mode="admin" writable={can('agents.write')} canReview={can('settlements.review')} canPay={isSuper} canExport={can('export')} />}
        {tab === 'agent-self' && <AgentsPage mode="self" />}
        {tab === 'routing' && <RoutingPage writable={can('routing.write')} />}
        {tab === 'roles' && <RolesPage writable={can('roles.write')} />}
        {tab === 'overview' && <DashboardPage />}
        {tab === 'users' && <fieldset className="pa-permission-scope" disabled={!can('users.write')}><UsersPage isSuper={isSuper} run={run} /></fieldset>}
        {tab === 'signup-risk' && <fieldset className="pa-permission-scope" disabled={!can('risk.write')}><SignupRiskPage run={run} /></fieldset>}
        {tab === 'promotions' && <fieldset className="pa-permission-scope" disabled={!can('promotions.write')}><PromotionsPage run={run} scoped={!!access?.channels.length} /></fieldset>}
        {tab === 'announcements' && <fieldset className="pa-permission-scope" disabled={!can('announcements.write')}><AnnouncementsPage run={run} /></fieldset>}
        {tab === 'plans' && <fieldset className="pa-permission-scope" disabled={!can('pricing.write')}><PlansPage onOpenSettings={() => navigate('settings')} run={run} /></fieldset>}
        {tab === 'models' && (
          <div className="pa-stack">
            <fieldset className="pa-permission-scope" disabled={!can('models.write')}><ModelsPage run={run} canPrice={can('pricing.write')} /></fieldset>
            {can('pricing.read') && <fieldset className="pa-permission-scope" disabled={!can('pricing.write')}><CostsPage run={run} /></fieldset>}
          </div>
        )}
        {tab === 'tenants' && <fieldset className="pa-permission-scope" disabled={!can('routing.write')}><TenantsPage run={run} /></fieldset>}
        {tab === 'settings' && <fieldset className="pa-permission-scope" disabled={!can('settings.write')}><SettingsPage run={run} /></fieldset>}
      </main>
    </div>
  )
}
