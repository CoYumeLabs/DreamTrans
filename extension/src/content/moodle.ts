import type { ContentRequest, ContentResponse } from '../shared/messages'
import { runDiagnostics } from './diagnostics'
import { readMoodleContext } from './moodle-cfg'
import { cancelSync, currentProgress, runSync } from './sync'

// Injected into the Moodle tab on demand (activeTab + scripting). It answers
// the popup's requests and keeps a sync running after the popup closes.
// Every Moodle request stays same-origin, in this tab, with this user present.

declare global {
  interface Window { __dreamtransMoodleSync?: boolean }
}

if (!window.__dreamtransMoodleSync) {
  window.__dreamtransMoodleSync = true
  let syncing = false

  chrome.runtime.onMessage.addListener((request: ContentRequest, _sender, sendResponse: (response: ContentResponse) => void) => {
    const respond = (response: ContentResponse) => sendResponse(response)
    switch (request.type) {
      case 'moodle.ping':
        respond({ ok: true, pong: true })
        return false
      case 'moodle.context':
        respond({ ok: true, context: readMoodleContext(document, window.location) })
        return false
      case 'moodle.state':
        respond({ ok: true, syncing, progress: syncing ? currentProgress() : null })
        return false
      case 'moodle.cancel':
        cancelSync()
        respond({ ok: true })
        return false
      case 'moodle.diagnose':
        runDiagnostics(readMoodleContext(document, window.location), document)
          .then((report) => respond({ ok: true, report }))
          .catch((reason) => respond({ ok: false, error: reason instanceof Error ? reason.message : String(reason) }))
        return true
      case 'moodle.sync':
        if (syncing) {
          respond({ ok: false, error: '已有一次同步在进行' })
          return false
        }
        syncing = true
        runSync(readMoodleContext(document, window.location), document, request.options)
          .then((summary) => respond({ ok: true, summary }))
          .catch((reason) => respond({ ok: false, error: reason instanceof Error ? reason.message : String(reason) }))
          .finally(() => { syncing = false })
        return true
      default:
        return false
    }
  })
}
