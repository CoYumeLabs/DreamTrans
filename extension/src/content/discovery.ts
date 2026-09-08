import type { CourseFile, CourseModule, CourseSection, CourseTree, MoodleContext, RecordingProvider } from '../shared/types'
import { moodleFetch, type RateLimiter } from './limits'

// Discovery: the course tree. API first, HTML second, bare links last. Every
// layer returns the same CourseTree so nothing downstream knows which one
// answered.

export const LIBRARY_PATTERN = /leganto|alma\.exlibrisgroup|ereserve|readinglist|reading-list|talis|eres\./i

const RECORDING_PATTERNS: Array<[RecordingProvider, RegExp]> = [
  ['echo360', /echo360|echovideo/i],
  ['panopto', /panopto/i],
  ['youtube', /youtube\.com|youtu\.be/i],
  ['kaltura', /kaltura|mediaspace/i],
]

export function detectRecording(url?: string, name?: string): { provider: RecordingProvider; url: string } | undefined {
  const haystack = `${url ?? ''} ${name ?? ''}`
  for (const [provider, pattern] of RECORDING_PATTERNS) {
    if (pattern.test(haystack)) return { provider, url: url ?? '' }
  }
  return undefined
}

export function classifyModule(module: CourseModule): CourseModule {
  const haystack = `${module.url ?? ''} ${module.name} ${module.contents.map((c) => c.fileurl).join(' ')}`
  if (LIBRARY_PATTERN.test(haystack)) {
    module.skipped = 'library'
    return module
  }
  if (module.modtype === 'url' || module.modtype === 'lti') {
    const target = module.contents.find((c) => c.fileurl)?.fileurl ?? module.url
    const recording = detectRecording(target, module.name)
    if (recording) module.recording = recording
  }
  if (module.modtype === 'forum') {
    module.skipped = 'private'
  }
  return module
}

interface AjaxResponse {
  error?: boolean
  exception?: { errorcode?: string; message?: string }
  data?: unknown
}

export class MoodleAjaxError extends Error {
  constructor(readonly errorcode: string, message: string) {
    super(message)
  }
}

/** One call to Moodle's own AJAX endpoint (the one its web UI uses). */
export async function moodleAjax<T>(
  limiter: RateLimiter, ctx: MoodleContext, methodname: string, args: Record<string, unknown>,
): Promise<T> {
  if (!ctx.sesskey) throw new MoodleAjaxError('nosesskey', 'no sesskey on this page')
  const url = `${ctx.wwwroot}/lib/ajax/service.php?sesskey=${encodeURIComponent(ctx.sesskey)}&info=${encodeURIComponent(methodname)}`
  const response = await moodleFetch(limiter, url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify([{ index: 0, methodname, args }]),
  })
  const contentType = response.headers.get('content-type') ?? ''
  if (!contentType.includes('json')) {
    throw new MoodleAjaxError('notjson', `AJAX answered ${response.status} ${contentType}`)
  }
  const payload = (await response.json()) as AjaxResponse[] | AjaxResponse
  const first = Array.isArray(payload) ? payload[0] : payload
  if (!first || first.error || first.exception) {
    const code = first?.exception?.errorcode ?? 'unknown'
    throw new MoodleAjaxError(code, first?.exception?.message ?? 'AJAX error')
  }
  return first.data as T
}

interface AjaxContent {
  type?: string
  filename?: string
  fileurl?: string
  filesize?: number
  mimetype?: string
  timemodified?: number
}

interface AjaxModule {
  id: number
  modname: string
  name: string
  url?: string
  description?: string
  contents?: AjaxContent[]
  dates?: Array<{ label?: string; timestamp?: number; dataid?: string }>
}

interface AjaxSection {
  id: number
  name: string
  section: number
  modules: AjaxModule[]
}

function latestTimemodified(contents: CourseFile[]): number | undefined {
  const values = contents.map((c) => c.timemodified ?? 0).filter((v) => v > 0)
  return values.length ? Math.max(...values) : undefined
}

export async function discoverViaAjax(limiter: RateLimiter, ctx: MoodleContext): Promise<CourseTree> {
  const sections = await moodleAjax<AjaxSection[]>(limiter, ctx, 'core_course_get_contents', {
    courseid: ctx.courseId,
    options: [{ name: 'includestealthmodules', value: 1 }],
  })
  return {
    source: 'ajax',
    courseId: ctx.courseId,
    sections: sections.map((section) => ({
      id: section.id,
      name: (section.name || `Section ${section.section}`).trim(),
      order: section.section,
      modules: (section.modules ?? []).map((module) => {
        const contents: CourseFile[] = (module.contents ?? [])
          .filter((content) => content.fileurl)
          .map((content) => ({
            fileurl: content.fileurl!,
            filename: content.filename ?? '',
            filesize: content.filesize,
            mimetype: content.mimetype,
            timemodified: content.timemodified,
          }))
        const due = module.dates?.find((d) => d.dataid === 'duedate' || /due/i.test(d.label ?? ''))?.timestamp
        return classifyModule({
          cmid: module.id,
          modtype: module.modname,
          name: module.name,
          url: module.url,
          description: module.description ? htmlToText(module.description) : undefined,
          dueAt: due,
          timemodified: latestTimemodified(contents),
          contents,
        })
      }),
    })),
  }
}

export function htmlToText(html: string): string {
  const doc = new DOMParser().parseFromString(html, 'text/html')
  return domToText(doc.body)
}

/** textContent with paragraph breaks preserved. */
export function domToText(root: Element | null): string {
  if (!root) return ''
  const lines: string[] = []
  const walk = (node: Node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      lines.push(node.textContent ?? '')
      return
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return
    const element = node as Element
    const tag = element.tagName.toLowerCase()
    if (['script', 'style', 'noscript', 'nav', 'template'].includes(tag)) return
    const block = /^(p|div|li|h[1-6]|tr|br|section|article|blockquote|pre|td|th|dt|dd|table|ul|ol|hr)$/.test(tag)
    if (block) lines.push('\n')
    element.childNodes.forEach(walk)
    if (block) lines.push('\n')
  }
  walk(root)
  return lines.join('').replace(/[ \t ]+/g, ' ').replace(/\n\s*\n\s*\n+/g, '\n\n').trim()
}

const MODTYPE_CLASS = /(?:^|\s)modtype_([a-z0-9_]+)/

export function discoverViaHtml(doc: Document, ctx: MoodleContext): CourseTree {
  const sectionNodes = Array.from(doc.querySelectorAll<HTMLElement>('[data-for="section"], li.section.main'))
  const sections: CourseSection[] = []
  sectionNodes.forEach((node, index) => {
    const titleNode = node.querySelector('[data-for="section_title"], .sectionname, h3.sectionname, .section-title')
    const name = titleNode?.textContent?.trim() || `Section ${index}`
    const idMatch = node.id.match(/section-(\d+)/)
    const modules: CourseModule[] = []
    for (const activity of Array.from(node.querySelectorAll<HTMLElement>('[data-for="cmitem"], li.activity'))) {
      const cmid = Number(activity.id.match(/module-(\d+)/)?.[1] ?? activity.getAttribute('data-id') ?? 0)
      if (!cmid) continue
      const modtype = activity.className.match(MODTYPE_CLASS)?.[1] ?? 'other'
      const link = activity.querySelector<HTMLAnchorElement>('a.aalink, .activityname a, .activity-item a[href*="/mod/"], a[href*="/mod/"]')
      const name = (activity.querySelector('.instancename, .activityname, .activity-name')?.textContent ?? link?.textContent ?? '').replace(/\s+/g, ' ').trim()
      modules.push(classifyModule({ cmid, modtype, name, url: link?.href, contents: [] }))
    }
    sections.push({ id: Number(idMatch?.[1] ?? index), name, order: index, modules })
  })
  return { source: 'html', courseId: ctx.courseId, sections }
}

const FILE_LINK = /pluginfile\.php|\.(pdf|pptx?|docx?|xlsx?|txt|md)(\?|$)/i

export function discoverViaLinks(doc: Document, ctx: MoodleContext): CourseTree {
  const seen = new Set<string>()
  const modules: CourseModule[] = []
  let synthetic = 1
  for (const anchor of Array.from(doc.querySelectorAll<HTMLAnchorElement>('[role="main"] a[href], #region-main a[href]'))) {
    const href = anchor.href.split('#')[0]
    if (!href || seen.has(href) || !FILE_LINK.test(href)) continue
    seen.add(href)
    const filename = decodeURIComponent(href.split('?')[0].split('/').pop() ?? '') || anchor.textContent?.trim() || `file-${synthetic}`
    modules.push(classifyModule({
      cmid: 900_000_000 + synthetic,
      modtype: 'link',
      name: anchor.textContent?.trim() || filename,
      url: href,
      contents: [{ fileurl: href, filename }],
    }))
    synthetic += 1
  }
  return { source: 'links', courseId: ctx.courseId, sections: [{ id: 0, name: 'Linked files', order: 0, modules }] }
}

export interface DiscoveryOutcome {
  tree: CourseTree
  ajaxError?: string
}

/** Runs the three layers in order and returns the first that yields modules. */
export async function discoverCourse(
  limiter: RateLimiter, ctx: MoodleContext, doc: Document,
): Promise<DiscoveryOutcome> {
  let ajaxError: string | undefined
  try {
    const tree = await discoverViaAjax(limiter, ctx)
    if (tree.sections.some((section) => section.modules.length > 0)) return { tree }
    ajaxError = 'AJAX returned no modules'
  } catch (reason) {
    ajaxError = reason instanceof Error ? `${reason.name}: ${reason.message}` : String(reason)
  }
  let pageDoc = doc
  if (!ctx.onCoursePage) {
    const response = await moodleFetch(limiter, `${ctx.wwwroot}/course/view.php?id=${ctx.courseId}`)
    pageDoc = new DOMParser().parseFromString(await response.text(), 'text/html')
  }
  const html = discoverViaHtml(pageDoc, ctx)
  if (html.sections.some((section) => section.modules.length > 0)) return { tree: html, ajaxError }
  return { tree: discoverViaLinks(pageDoc, ctx), ajaxError }
}

export interface EnrolledCourse {
  id: number
  fullname: string
  shortname: string
  viewurl?: string
}

export async function listEnrolledCourses(limiter: RateLimiter, ctx: MoodleContext): Promise<EnrolledCourse[]> {
  const data = await moodleAjax<{ courses?: EnrolledCourse[] }>(
    limiter, ctx, 'core_course_get_enrolled_courses_by_timeline_classification',
    { classification: 'all', limit: 0, offset: 0, sort: 'fullname' },
  )
  return data.courses ?? []
}
