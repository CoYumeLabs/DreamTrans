import JSZip from 'jszip'
import type { CourseModule, FetchedFile, MoodleContext } from '../shared/types'
import { domToText } from './discovery'
import { moodleFetch, type RateLimiter } from './limits'

// Fetch: per module type, what to pull and how. Only reads. Submissions,
// grades and ordinary discussions are never requested.

const TEXT_MEDIA = 'text/plain'

function filenameFromResponse(response: Response, fallback: string): string {
  const disposition = response.headers.get('content-disposition') ?? ''
  const star = disposition.match(/filename\*=(?:UTF-8'')?([^;]+)/i)
  if (star) {
    try { return decodeURIComponent(star[1].replace(/^"|"$/g, '')) } catch { /* fall through */ }
  }
  const plain = disposition.match(/filename="?([^";]+)"?/i)
  if (plain) return plain[1]
  const fromUrl = decodeURIComponent(response.url.split('?')[0].split('/').pop() ?? '')
  return fromUrl || fallback
}

async function fetchBinary(limiter: RateLimiter, url: string, fallbackName: string, timemodified?: number): Promise<FetchedFile | null> {
  const response = await moodleFetch(limiter, url)
  const mimetype = (response.headers.get('content-type') ?? 'application/octet-stream').split(';')[0].trim().toLowerCase()
  if (mimetype === 'text/html') {
    // resource/view.php can answer with a "click to open" page instead of a
    // redirect; follow the pluginfile link on it once.
    const html = await response.text()
    const doc = new DOMParser().parseFromString(html, 'text/html')
    const link = doc.querySelector<HTMLAnchorElement>('.resourceworkaround a[href*="pluginfile.php"], a[href*="pluginfile.php"]')
    if (link?.href && link.href !== url) return fetchBinary(limiter, link.href, fallbackName, timemodified)
    return null
  }
  const bytes = await response.arrayBuffer()
  return { filename: filenameFromResponse(response, fallbackName), mimetype, bytes, url: response.url, timemodified }
}

function textFile(name: string, text: string, url: string, timemodified?: number): FetchedFile {
  return { filename: name, mimetype: TEXT_MEDIA, bytes: new TextEncoder().encode(text).buffer as ArrayBuffer, url, timemodified }
}

async function unzipFiles(bytes: ArrayBuffer, url: string, timemodified?: number): Promise<FetchedFile[]> {
  const zip = await JSZip.loadAsync(bytes)
  const files: FetchedFile[] = []
  for (const entry of Object.values(zip.files)) {
    if (entry.dir) continue
    const lower = entry.name.toLowerCase()
    if (!/\.(pdf|pptx|docx|txt|md|png|jpe?g|webp)$/.test(lower)) continue
    const content = await entry.async('arraybuffer')
    files.push({
      filename: entry.name.split('/').pop() ?? entry.name,
      mimetype: mimeForName(lower),
      bytes: content,
      url: `${url}#${encodeURIComponent(entry.name)}`,
      timemodified,
    })
  }
  return files
}

export function mimeForName(name: string): string {
  const lower = name.toLowerCase()
  if (lower.endsWith('.pdf')) return 'application/pdf'
  if (lower.endsWith('.pptx')) return 'application/vnd.openxmlformats-officedocument.presentationml.presentation'
  if (lower.endsWith('.docx')) return 'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
  if (lower.endsWith('.png')) return 'image/png'
  if (lower.endsWith('.jpg') || lower.endsWith('.jpeg')) return 'image/jpeg'
  if (lower.endsWith('.webp')) return 'image/webp'
  if (lower.endsWith('.md')) return 'text/markdown'
  return TEXT_MEDIA
}

/** Everything one module contributes, already fetched into memory. */
export async function fetchModuleFiles(
  limiter: RateLimiter, ctx: MoodleContext, module: CourseModule,
): Promise<FetchedFile[]> {
  if (module.skipped || module.recording) return []
  const base = ctx.wwwroot
  switch (module.modtype) {
    case 'resource':
    case 'link': {
      const content = module.contents.find((c) => c.fileurl)
      const url = content?.fileurl ?? `${base}/mod/resource/view.php?id=${module.cmid}`
      const file = await fetchBinary(limiter, url, content?.filename || `${module.name}.bin`, content?.timemodified ?? module.timemodified)
      if (!file) return []
      if (file.mimetype === 'application/zip' || file.filename.toLowerCase().endsWith('.zip')) {
        return unzipFiles(file.bytes, file.url, file.timemodified)
      }
      return [file]
    }
    case 'folder': {
      const response = await moodleFetch(limiter, `${base}/mod/folder/download_folder.php?id=${module.cmid}`)
      const contentType = (response.headers.get('content-type') ?? '').toLowerCase()
      if (!contentType.includes('zip') && !contentType.includes('octet')) {
        // Folder download disabled: fall back to the listed files.
        const files: FetchedFile[] = []
        for (const content of module.contents) {
          const file = await fetchBinary(limiter, content.fileurl, content.filename, content.timemodified)
          if (file) files.push(file)
        }
        return files
      }
      return unzipFiles(await response.arrayBuffer(), response.url, module.timemodified)
    }
    case 'book': {
      const response = await moodleFetch(limiter, `${base}/mod/book/tool/print/index.php?id=${module.cmid}`)
      const doc = new DOMParser().parseFromString(await response.text(), 'text/html')
      const text = domToText(doc.querySelector('#page-content, [role="main"], body'))
      return text ? [textFile(`${module.name}.txt`, text, response.url, module.timemodified)] : []
    }
    case 'page': {
      const response = await moodleFetch(limiter, `${base}/mod/page/view.php?id=${module.cmid}`)
      const doc = new DOMParser().parseFromString(await response.text(), 'text/html')
      const text = domToText(doc.querySelector('[role="main"] .box.generalbox, [role="main"], #region-main'))
      return text ? [textFile(`${module.name}.txt`, text, response.url, module.timemodified)] : []
    }
    case 'label': {
      const text = module.description?.trim()
      return text && text.length > 40 ? [textFile(`${module.name || 'label'}.txt`, text, module.url ?? base, module.timemodified)] : []
    }
    case 'assign': {
      // Title, description and due date only. Never the submission.
      const lines = [module.name]
      if (module.dueAt) lines.push(`Due: ${new Date(module.dueAt * 1000).toISOString()}`)
      if (module.description) lines.push(module.description)
      const text = lines.join('\n')
      return text.length > 20 ? [textFile(`${module.name}.txt`, text, module.url ?? base, module.timemodified)] : []
    }
    case 'forum':
      // A forum name cannot prove its contents are public announcements.
      return []
    default:
      return []
  }
}
