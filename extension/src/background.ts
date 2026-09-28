import type { BackgroundRequest, BackgroundResponse } from './shared/messages'
import type { DerivedDocument, DreamTransProject, DreamTransStatus, FigureStats, ServerDerivedRef } from './shared/types'

// The only code that talks to DreamTrans. Holds the DreamTrans session
// (never a Moodle one) and relays uploads from the content script, which
// cannot call a different origin from inside the Moodle page.

interface StoredSession {
  server: string
  accessToken: string
  refreshToken: string
  expiresAt: number
  email?: string
  name?: string
}

const SESSION_KEY = 'dt.session'

async function loadSession(): Promise<StoredSession | null> {
  const stored = await chrome.storage.local.get(SESSION_KEY)
  return (stored[SESSION_KEY] as StoredSession | undefined) ?? null
}

async function saveSession(session: StoredSession | null): Promise<void> {
  if (session) await chrome.storage.local.set({ [SESSION_KEY]: session })
  else await chrome.storage.local.remove(SESSION_KEY)
}

function normalizeServer(server: string): string {
  const trimmed = server.trim().replace(/\/+$/, '')
  if (!/^https?:\/\//.test(trimmed)) return `https://${trimmed}`
  return trimmed
}

class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message)
  }
}

async function refresh(session: StoredSession): Promise<StoredSession> {
  const response = await fetch(`${session.server}/api/auth/refresh`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: session.refreshToken }),
  })
  if (!response.ok) throw new ApiError(response.status, 'DreamTrans 登录已过期，请重新登录')
  const body = (await response.json()) as { access_token: string; refresh_token: string; expires_in: number }
  const next: StoredSession = {
    ...session,
    accessToken: body.access_token,
    refreshToken: body.refresh_token || session.refreshToken,
    expiresAt: Date.now() + (body.expires_in ?? 900) * 1000,
  }
  await saveSession(next)
  return next
}

async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  let session = await loadSession()
  if (!session) throw new ApiError(401, '还没有登录 DreamTrans')
  if (session.expiresAt - Date.now() < 30_000) session = await refresh(session)
  const attempt = async (token: string) => fetch(`${session!.server}${path}`, {
    ...init,
    headers: { ...(init.headers ?? {}), Authorization: `Bearer ${token}` },
  })
  let response = await attempt(session.accessToken)
  if (response.status === 401) {
    session = await refresh(session)
    response = await attempt(session.accessToken)
  }
  if (!response.ok) {
    const text = (await response.text()).slice(0, 300)
    throw new ApiError(response.status, text || `DreamTrans answered ${response.status}`)
  }
  return (await response.json()) as T
}

async function login(server: string, email: string, password: string): Promise<DreamTransStatus> {
  const base = normalizeServer(server)
  const response = await fetch(`${base}/api/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  if (!response.ok) {
    const text = (await response.text()).slice(0, 200)
    throw new ApiError(response.status, text || '登录失败')
  }
  const body = (await response.json()) as {
    access_token: string; refresh_token: string; expires_in: number
    user?: { email?: string; name?: string }
  }
  await saveSession({
    server: base,
    accessToken: body.access_token,
    refreshToken: body.refresh_token,
    expiresAt: Date.now() + (body.expires_in ?? 900) * 1000,
    email: body.user?.email ?? email,
    name: body.user?.name,
  })
  return { connected: true, server: base, email: body.user?.email ?? email, name: body.user?.name }
}

async function status(): Promise<DreamTransStatus> {
  const session = await loadSession()
  if (!session) return { connected: false, server: '' }
  return { connected: true, server: session.server, email: session.email, name: session.name }
}

async function listProjects(): Promise<DreamTransProject[]> {
  const body = await api<{ projects?: DreamTransProject[] } | DreamTransProject[]>('/api/ai/projects')
  const projects = Array.isArray(body) ? body : body.projects ?? []
  return projects.map((project) => ({ id: project.id, name: project.name, description: project.description }))
}

async function listDerived(projectId: string): Promise<ServerDerivedRef[]> {
  const body = await api<{ sources: ServerDerivedRef[] }>(`/api/ai/projects/${encodeURIComponent(projectId)}/sources/derived`)
  return body.sources ?? []
}

async function uploadDerived(projectId: string, document: DerivedDocument): Promise<{ id: string; duplicate: boolean; figures?: FigureStats }> {
  const body = await api<{ source: { id: string }; duplicate: boolean; figures?: FigureStats }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources/derived`,
    { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(document) },
  )
  return { id: body.source.id, duplicate: Boolean(body.duplicate), figures: body.figures }
}

async function uploadOriginal(projectId: string, sourceId: string, mimetype: string, base64: string): Promise<{ id: string; duplicate: boolean }> {
  // Messages are JSON, so the bytes arrive as base64.
  const binary = atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
  const body = await api<{ source: { id: string }; duplicate: boolean }>(
    `/api/ai/projects/${encodeURIComponent(projectId)}/sources/${encodeURIComponent(sourceId)}/original`,
    { method: 'PUT', headers: { 'Content-Type': mimetype }, body: bytes },
  )
  return { id: body.source.id, duplicate: Boolean(body.duplicate) }
}

async function handle(request: BackgroundRequest): Promise<BackgroundResponse> {
  switch (request.type) {
    case 'dt.status':
      return { ok: true, status: await status() }
    case 'dt.login':
      return { ok: true, status: await login(request.server, request.email, request.password) }
    case 'dt.logout':
      await saveSession(null)
      return { ok: true }
    case 'dt.projects':
      return { ok: true, projects: await listProjects() }
    case 'dt.derived.list':
      return { ok: true, sources: await listDerived(request.projectId) }
    case 'dt.derived.upload':
      return { ok: true, uploaded: await uploadDerived(request.projectId, request.document) }
    case 'dt.original.upload':
      return { ok: true, uploaded: await uploadOriginal(request.projectId, request.sourceId, request.mimetype, request.base64) }
    default:
      return { ok: false, error: 'unknown request' }
  }
}

chrome.runtime.onMessage.addListener((request: BackgroundRequest | { type: string }, _sender, sendResponse) => {
  if (!request || typeof request !== 'object' || !('type' in request) || !String(request.type).startsWith('dt.')) {
    return false
  }
  handle(request as BackgroundRequest)
    .then(sendResponse)
    .catch((reason) => sendResponse({ ok: false, error: reason instanceof Error ? reason.message : String(reason) }))
  return true
})
