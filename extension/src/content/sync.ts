import { sendToBackground } from '../shared/messages'
import type {
  CourseModule, DerivedDocument, FetchedFile, FigureStats, MoodleContext, ServerDerivedRef, SyncModuleState,
  SyncOptions, SyncProgress, SyncState, SyncSummary,
} from '../shared/types'
import { discoverCourse, FORUM_MODTYPE } from './discovery'
import { extractFile, sha256Hex } from './extract'
import { fetchForumFiles, fetchModuleFiles } from './fetcher'
import { RateLimiter } from './limits'

// Sync: incremental by timemodified, idempotent by sha256. One pass over the
// course tree while the user is on the page; nothing runs afterwards.

const FETCHABLE = new Set(['resource', 'folder', 'book', 'page', 'cms', 'label', 'assign', 'link'])

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

let lastProgress: SyncProgress | null = null

/** What a reopened popup shows while a sync is still running. */
export function currentProgress(): SyncProgress | null {
  return lastProgress
}

export function report(progress: SyncProgress): void {
  lastProgress = progress
  try {
    chrome.runtime.sendMessage({ type: 'moodle.progress', progress })
  } catch {
    // Popup closed; the sync keeps going.
  }
}

let activeLimiter: RateLimiter | null = null

// 保存原文件 (opt-in): which files the server may keep, and the most a message
// to the background can carry once base64'd. The server's own limit is 50 MB.
const ORIGINAL_TYPES = new Set([
  'application/pdf',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'image/png', 'image/jpeg', 'image/webp',
])
const MAX_ORIGINAL_BYTES = 45 * 1024 * 1024

function toBase64(bytes: ArrayBuffer): string {
  const view = new Uint8Array(bytes)
  let binary = ''
  for (let i = 0; i < view.length; i += 0x8000) {
    binary += String.fromCharCode(...view.subarray(i, i + 0x8000))
  }
  return btoa(binary)
}

interface KnownSource { id: string; hasOriginal: boolean }

/** Modules fetched and extracted at once; Moodle requests still share the limiter. */
const MODULE_WORKERS = 3

interface UploadResult { id: string; duplicate: boolean; figures?: FigureStats }

function addFigureStats(summary: SyncSummary, figures?: FigureStats): void {
  if (!figures) return
  summary.visionPages = (summary.visionPages ?? 0) + (figures.vision ?? 0)
  summary.figureUSD = (summary.figureUSD ?? 0) + (figures.charged_usd ?? 0)
  if (figures.vision_stopped === 'insufficient_balance' && !summary.errors.some((e) => e.startsWith('余额不足'))) {
    summary.errors.push('余额不足：之后的图片改用普通 OCR（只认字，不描述图表）。充值后开「全量重检」可重新读图。')
  } else if (figures.vision_stopped === 'billing_unavailable' && !summary.errors.some((e) => e.startsWith('计费服务'))) {
    summary.errors.push('计费服务暂时不可用：部分图片改用普通 OCR。')
  }
}

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
    recordings: [], requests: 0, durationMs: 0, errors: [], originals: 0,
  }
  try {
    report({ phase: 'discover', message: '正在读取课程结构…' })
    const { tree, ajaxError } = await discoverCourse(limiter, ctx, doc)
    if (ajaxError) summary.errors.push(`discovery fell back (${tree.source}): ${ajaxError}`)

    const serverRefs = await sendToBackground<{ ok: true; sources: ServerDerivedRef[] }>({
      type: 'dt.derived.list', projectId: options.projectId,
    })
    const known = new Map<string, KnownSource>(
      serverRefs.sources.map((ref) => [ref.sha256, { id: ref.id, hasOriginal: Boolean(ref.has_original) }]),
    )

    /** Attaches the file itself when the user asked; true when nothing is left to keep. */
    const keepOriginal = async (sha: string, file: FetchedFile): Promise<boolean> => {
      const ref = known.get(sha)
      if (!options.keepOriginals || !ref || ref.hasOriginal || !ORIGINAL_TYPES.has(file.mimetype)) return true
      if (file.bytes.byteLength > MAX_ORIGINAL_BYTES) {
        summary.errors.push(`${file.filename}: 原文件超过 45 MB，只保存了文字`)
        return false
      }
      if (limiter.cancelled) throw new Error('cancelled')
      report({ phase: 'upload', message: `保存原文件 ${file.filename}` })
      try {
        await sendToBackground({
          type: 'dt.original.upload', projectId: options.projectId, sourceId: ref.id,
          mimetype: file.mimetype, base64: toBase64(file.bytes),
        })
      } catch (reason) {
        summary.errors.push(`${file.filename}: 原文件未保存（${reason instanceof Error ? reason.message : String(reason)}）`)
        return false
      }
      ref.hasOriginal = true
      summary.originals = (summary.originals ?? 0) + 1
      return true
    }
    const state = options.full ? { modules: {} } : await loadSyncState(ctx)

    const modules = tree.sections.flatMap((section) => section.modules.map((module) => ({ section, module })))
    const total = modules.length
    let done = 0
    // Uploads go one at a time: the server reads one material per user at a
    // time. Fetching and extracting the next modules overlaps with it.
    let uploadChain: Promise<unknown> = Promise.resolve()
    const serialUpload = <T>(task: () => Promise<T>): Promise<T> => {
      const run = uploadChain.then(task, task)
      uploadChain = run.catch(() => undefined)
      return run
    }

    const processModule = async ({ section, module }: typeof modules[number]): Promise<void> => {
      if (limiter.cancelled) throw new Error('cancelled')
      summary.scanned += 1
      if (module.recording) {
        summary.recordings.push({ provider: module.recording.provider, name: module.name, url: module.recording.url, section: section.name })
        return
      }
      // Forums are private unless the user opted in for this sync.
      const optedForum = module.skipped === 'private' && Boolean(options.includeForums) && FORUM_MODTYPE.test(module.modtype)
      if (!optedForum && (module.skipped || !FETCHABLE.has(module.modtype))) {
        summary.skipped += 1
        return
      }
      const previous = state.modules[String(module.cmid)]
      const fingerprint = moduleFingerprint(module)
      if (
        previous && fingerprint > 0 && previous.timemodified === fingerprint
        && previous.sha256s.every((sha) => known.has(sha))
        && (!options.keepOriginals || previous.originalsKept === true)
      ) {
        summary.unchanged += 1
        return
      }
      report({ phase: 'fetch', message: `${section.name} / ${module.name}`, done, total })
      const moduleState: SyncModuleState = { timemodified: fingerprint, sha256s: [], uploadedAt: Date.now() }
      let originalsKept = true
      try {
        const files = optedForum ? await fetchForumFiles(limiter, ctx, module) : await fetchModuleFiles(limiter, ctx, module)
        for (const file of files) {
          const sha = await sha256Hex(file.bytes)
          moduleState.sha256s.push(sha)
          if (known.has(sha)) {
            summary.duplicates += 1
            originalsKept = (await keepOriginal(sha, file)) && originalsKept
            continue
          }
          if (limiter.cancelled) throw new Error('cancelled')
          report({ phase: 'extract', message: `抽取 ${file.filename}`, done, total })
          const extracted = await extractFile(file, {
            renderFigures: options.uploadFigures, maxFigures: 60, renderWidth: 2048,
          })
          if (!extracted || extracted.pages.length === 0) {
            summary.skipped += 1
            continue
          }
          const figureCount = extracted.pages.reduce((sum, page) => sum + (page.figures?.length ?? 0), 0)
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
          const uploaded = await serialUpload(async () => {
            if (limiter.cancelled) throw new Error('cancelled')
            report({
              phase: 'upload',
              message: figureCount
                ? `上传 ${file.filename}（${extracted.pages.length} 页，服务器读图 ${figureCount} 张）`
                : `上传 ${file.filename}（${extracted.pages.length} 页）`,
              done, total,
            })
            return sendToBackground<{ ok: true; uploaded: UploadResult }>({
              type: 'dt.derived.upload', projectId: options.projectId, document,
            })
          })
          known.set(sha, { id: uploaded.uploaded.id, hasOriginal: false })
          if (uploaded.uploaded.duplicate) summary.duplicates += 1
          else summary.uploaded += 1
          addFigureStats(summary, uploaded.uploaded.figures)
          originalsKept = (await keepOriginal(sha, file)) && originalsKept
        }
        moduleState.originalsKept = Boolean(options.keepOriginals) && originalsKept
        state.modules[String(module.cmid)] = moduleState
        await saveSyncState(ctx, state)
      } catch (reason) {
        // An aborted download surfaces as AbortError: that is a stop, not a failure.
        if (limiter.cancelled) throw new Error('cancelled')
        summary.failed += 1
        summary.errors.push(`${module.name}: ${reason instanceof Error ? reason.message : String(reason)}`)
      } finally {
        done += 1
      }
    }

    let next = 0
    const worker = async (): Promise<void> => {
      while (next < modules.length) {
        const item = modules[next]
        next += 1
        await processModule(item)
      }
    }
    await Promise.all(Array.from({ length: Math.min(MODULE_WORKERS, modules.length) }, worker))
    state.lastSyncedAt = Date.now()
    await saveSyncState(ctx, state)
    summary.requests = limiter.requests
    summary.durationMs = Date.now() - started
    report({ phase: 'done', message: `完成：上传 ${summary.uploaded}，未变 ${summary.unchanged}，重复 ${summary.duplicates}` })
    return summary
  } catch (reason) {
    summary.requests = limiter.requests
    summary.durationMs = Date.now() - started
    if (limiter.cancelled) {
      summary.stopped = true
      report({ phase: 'done', message: `已停止：上传 ${summary.uploaded}，重复 ${summary.duplicates}` })
      return summary
    }
    const message = reason instanceof Error ? reason.message : String(reason)
    summary.errors.push(message)
    report({ phase: 'error', message })
    return summary
  } finally {
    activeLimiter = null
  }
}
