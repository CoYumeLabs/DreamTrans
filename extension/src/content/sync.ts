import { sendToBackground } from '../shared/messages'
import type {
  CourseModule, DerivedDocument, MoodleContext, ServerDerivedRef, SyncModuleState,
  SyncOptions, SyncProgress, SyncState, SyncSummary,
} from '../shared/types'
import { discoverCourse } from './discovery'
import { extractFile, sha256Hex } from './extract'
import { fetchModuleFiles } from './fetcher'
import { RateLimiter } from './limits'

// Sync: incremental by timemodified, idempotent by sha256. One pass over the
// course tree while the user is on the page; nothing runs afterwards.

const FETCHABLE = new Set(['resource', 'folder', 'book', 'page', 'label', 'assign', 'link'])

function stateKey(ctx: MoodleContext): string {
  return `dt.sync.${ctx.host}.${ctx.courseId}`
}

export async function loadSyncState(ctx: MoodleContext): Promise<SyncState> {
  const key = stateKey(ctx)
  const stored = await chrome.storage.local.get(key)
  const value = stored[key] as SyncState | undefined
  return value ?? { modules: {} }
}

async function saveSyncState(ctx: MoodleContext, state: SyncState): Promise<void> {
  await chrome.storage.local.set({ [stateKey(ctx)]: state })
}

export function report(progress: SyncProgress): void {
  try {
    chrome.runtime.sendMessage({ type: 'moodle.progress', progress })
  } catch {
    // Popup closed; the sync keeps going.
  }
}

let activeLimiter: RateLimiter | null = null

export function cancelSync(): void {
  activeLimiter?.cancel()
}

function moduleFingerprint(module: CourseModule): number {
  return module.timemodified ?? 0
}

export async function runSync(ctx: MoodleContext, doc: Document, options: SyncOptions): Promise<SyncSummary> {
  const started = Date.now()
  const limiter = new RateLimiter(3, 200)
  activeLimiter = limiter
  const summary: SyncSummary = {
    scanned: 0, uploaded: 0, duplicates: 0, unchanged: 0, skipped: 0, failed: 0,
    recordings: [], requests: 0, durationMs: 0, errors: [],
  }
  try {
    report({ phase: 'discover', message: '正在读取课程结构…' })
    const { tree, ajaxError } = await discoverCourse(limiter, ctx, doc)
    if (ajaxError) summary.errors.push(`discovery fell back (${tree.source}): ${ajaxError}`)

    const serverRefs = await sendToBackground<{ ok: true; sources: ServerDerivedRef[] }>({
      type: 'dt.derived.list', projectId: options.projectId,
    })
    const known = new Set(serverRefs.sources.map((ref) => ref.sha256))
    const state = options.full ? { modules: {} } : await loadSyncState(ctx)

    const modules = tree.sections.flatMap((section) => section.modules.map((module) => ({ section, module })))
    const total = modules.length
    let done = 0
    for (const { section, module } of modules) {
      if (limiter.cancelled) throw new Error('cancelled')
      done += 1
      summary.scanned += 1
      if (module.recording) {
        summary.recordings.push({ provider: module.recording.provider, name: module.name, url: module.recording.url, section: section.name })
        continue
      }
      if (module.skipped || !FETCHABLE.has(module.modtype)) {
        summary.skipped += 1
        continue
      }
      const previous = state.modules[String(module.cmid)]
      const fingerprint = moduleFingerprint(module)
      if (previous && fingerprint > 0 && previous.timemodified === fingerprint && previous.sha256s.every((sha) => known.has(sha))) {
        summary.unchanged += 1
        continue
      }
      report({ phase: 'fetch', message: `${section.name} / ${module.name}`, done, total })
      const moduleState: SyncModuleState = { timemodified: fingerprint, sha256s: [], uploadedAt: Date.now() }
      try {
        const files = await fetchModuleFiles(limiter, ctx, module)
        for (const file of files) {
          const sha = await sha256Hex(file.bytes)
          moduleState.sha256s.push(sha)
          if (known.has(sha)) {
            summary.duplicates += 1
            continue
          }
          report({ phase: 'extract', message: `抽取 ${file.filename}`, done, total })
          const extracted = await extractFile(file, {
            renderFigures: options.uploadFigures, maxFigures: 60, renderWidth: 1024,
          })
          if (!extracted || extracted.pages.length === 0) {
            summary.skipped += 1
            continue
          }
          const document: DerivedDocument = {
            sha256: sha,
            filename: file.filename,
            media_type: file.mimetype,
            size_bytes: file.bytes.byteLength,
            page_count: extracted.pages.length,
            pages: extracted.pages,
            lms: {
              host: ctx.host,
              course_id: ctx.courseId,
              course_shortname: ctx.shortname,
              course_name: ctx.courseName,
              section: section.name,
              section_order: section.order,
              cmid: module.cmid,
              modtype: module.modtype,
              module_name: module.name,
              url: module.url,
              timemodified: file.timemodified ?? fingerprint,
              extractor: extracted.extractor,
            },
          }
          report({ phase: 'upload', message: `上传 ${file.filename}（${extracted.pages.length} 页）`, done, total })
          const uploaded = await sendToBackground<{ ok: true; uploaded: { id: string; duplicate: boolean } }>({
            type: 'dt.derived.upload', projectId: options.projectId, document,
          })
          known.add(sha)
          if (uploaded.uploaded.duplicate) summary.duplicates += 1
          else summary.uploaded += 1
        }
        state.modules[String(module.cmid)] = moduleState
        await saveSyncState(ctx, state)
      } catch (reason) {
        if (reason instanceof Error && reason.message === 'cancelled') throw reason
        summary.failed += 1
        summary.errors.push(`${module.name}: ${reason instanceof Error ? reason.message : String(reason)}`)
      }
    }
    state.lastSyncedAt = Date.now()
    await saveSyncState(ctx, state)
    summary.requests = limiter.requests
    summary.durationMs = Date.now() - started
    report({ phase: 'done', message: `完成：上传 ${summary.uploaded}，未变 ${summary.unchanged}，重复 ${summary.duplicates}` })
    return summary
  } catch (reason) {
    summary.requests = limiter.requests
    summary.durationMs = Date.now() - started
    const message = reason instanceof Error ? reason.message : String(reason)
    summary.errors.push(message)
    report({ phase: 'error', message })
    return summary
  } finally {
    activeLimiter = null
  }
}
