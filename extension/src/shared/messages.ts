import type {
  DerivedDocument,
  DiagnosticsReport,
  DreamTransProject,
  DreamTransStatus,
  MoodleContext,
  ServerDerivedRef,
  SyncOptions,
  SyncProgress,
  SyncSummary,
} from './types'

// Popup → content script (injected into the Moodle tab).
export type ContentRequest =
  | { type: 'moodle.ping' }
  | { type: 'moodle.context' }
  | { type: 'moodle.diagnose' }
  | { type: 'moodle.sync'; options: SyncOptions }
  | { type: 'moodle.cancel' }
  | { type: 'moodle.state' }

export type ContentResponse =
  | { ok: true; pong: true }
  | { ok: true; context: MoodleContext }
  | { ok: true; report: DiagnosticsReport }
  | { ok: true; summary: SyncSummary }
  | { ok: true; syncing: boolean; progress: SyncProgress | null }
  | { ok: true }
  | { ok: false; error: string }

// Content script → popup (broadcast; the popup may be closed).
export interface ProgressMessage {
  type: 'moodle.progress'
  progress: SyncProgress
}

// Popup / content script → background (DreamTrans API, token storage).
export type BackgroundRequest =
  | { type: 'dt.status' }
  | { type: 'dt.login'; server: string; email: string; password: string }
  | { type: 'dt.logout' }
  | { type: 'dt.projects' }
  | { type: 'dt.derived.list'; projectId: string }
  | { type: 'dt.derived.upload'; projectId: string; document: DerivedDocument }

export type BackgroundResponse =
  | { ok: true; status: DreamTransStatus }
  | { ok: true; projects: DreamTransProject[] }
  | { ok: true; sources: ServerDerivedRef[] }
  | { ok: true; uploaded: { id: string; duplicate: boolean } }
  | { ok: true }
  | { ok: false; error: string }

export function sendToBackground<T extends BackgroundResponse>(request: BackgroundRequest): Promise<T> {
  return new Promise((resolve, reject) => {
    chrome.runtime.sendMessage(request, (response: T | undefined) => {
      const failure = chrome.runtime.lastError
      if (failure) {
        reject(new Error(failure.message ?? 'background unavailable'))
        return
      }
      if (!response) {
        reject(new Error('background returned nothing'))
        return
      }
      if (!response.ok) {
        reject(new Error((response as { error?: string }).error || 'background request failed'))
        return
      }
      resolve(response)
    })
  })
}

export function sendToTab<T extends ContentResponse>(tabId: number, request: ContentRequest): Promise<T> {
  return new Promise((resolve, reject) => {
    chrome.tabs.sendMessage(tabId, request, (response: T | undefined) => {
      const failure = chrome.runtime.lastError
      if (failure) {
        reject(new Error(failure.message ?? 'tab unavailable'))
        return
      }
      if (!response) {
        reject(new Error('content script returned nothing'))
        return
      }
      resolve(response)
    })
  })
}
