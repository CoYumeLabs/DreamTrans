import { useEffect, useState } from 'react'
import { authFetch } from '../api/auth'
export interface ConsoleAccess { allowed: boolean; super: boolean; tenant_admin: boolean; role_id: string; role_key: string; name: string; permissions: string[]; channels: string[] }
export const hasConsolePermission = (access: ConsoleAccess | null, permission: string) => !!access && (access.super || access.permissions.includes(permission))
export function useConsoleAccess(userId?: string) {
  const [state, setState] = useState<{ userId: string; access: ConsoleAccess } | null>(null)
  useEffect(() => {
    if (!userId) return
    let active = true
    const refresh = () => { void authFetch<ConsoleAccess>('/api/admin/access').then(access => { if (active) setState({ userId, access }) }).catch(() => { if (active) setState(null) }) }
    refresh(); window.addEventListener('focus', refresh)
    return () => { active = false; window.removeEventListener('focus', refresh) }
  }, [userId])
  return state?.userId === userId ? state?.access ?? null : null
}
