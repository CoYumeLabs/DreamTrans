import type { DiagnosticCheck, DiagnosticsReport, MoodleContext, RecordingProvider } from '../shared/types'
import { detectRecording, discoverCourse, LIBRARY_PATTERN, listEnrolledCourses, MoodleAjaxError } from './discovery'
import { RateLimiter } from './limits'

// M0 verification: everything the PRD's checklist asks for, measured on the
// page the user has open. Reads only; uploads nothing.

function guessMoodleVersion(doc: Document, ctx: MoodleContext): string {
  if (ctx.release) return ctx.release
  const generator = doc.querySelector<HTMLMetaElement>('meta[name="generator"]')?.content
  if (generator) return generator
  if (doc.querySelector('[data-for="section"]')) return '4.x (data-for markup)'
  if (doc.querySelector('li.section.main')) return '3.x (li.section.main markup)'
  return 'unknown'
}

function recordingEmbeds(doc: Document): Array<{ provider: RecordingProvider; url: string }> {
  const embeds: Array<{ provider: RecordingProvider; url: string }> = []
  for (const frame of Array.from(doc.querySelectorAll<HTMLIFrameElement>('iframe[src]'))) {
    const recording = detectRecording(frame.src)
    if (recording) embeds.push(recording)
  }
  return embeds
}

function hostOf(url: string): string {
  try { return new URL(url).host } catch { return url.slice(0, 60) }
}

export async function runDiagnostics(ctx: MoodleContext, doc: Document): Promise<DiagnosticsReport> {
  const limiter = new RateLimiter(3, 200)
  const checks: DiagnosticCheck[] = []
  const modtypes: Record<string, number> = {}
  const recordings: Array<{ provider: RecordingProvider; url: string }> = []
  let libraryLinks = 0

  checks.push({
    key: 'version', label: 'Moodle 版本',
    ok: null, detail: guessMoodleVersion(doc, ctx),
  })
  checks.push({
    key: 'sesskey', label: 'sesskey 位置',
    ok: ctx.sesskeySource !== 'none',
    detail: ctx.sesskeySource === 'none' ? '页面上找不到 sesskey' : `来源 ${ctx.sesskeySource}`,
  })
  checks.push({
    key: 'course', label: '当前课程',
    ok: ctx.courseId > 0,
    detail: ctx.courseId > 0 ? `${ctx.shortname || '?'} · ${ctx.courseName} (id ${ctx.courseId})` : '不在课程页面上',
  })

  const started = Date.now()
  let discoverySource: DiagnosticsReport['discoverySource'] = null
  let ajaxDetail = ''
  try {
    const { tree, ajaxError } = await discoverCourse(limiter, ctx, doc)
    discoverySource = tree.source
    ajaxDetail = ajaxError ?? `返回 ${tree.sections.length} 个 section`
    for (const section of tree.sections) {
      for (const module of section.modules) {
        modtypes[module.modtype] = (modtypes[module.modtype] ?? 0) + 1
        if (module.recording) recordings.push(module.recording)
        if (module.skipped === 'library') libraryLinks += 1
      }
    }
  } catch (reason) {
    ajaxDetail = reason instanceof Error ? reason.message : String(reason)
  }
  const discoveryMs = Date.now() - started
  checks.push({
    key: 'ajax_state', label: 'core_courseformat_get_state AJAX',
    ok: discoverySource === 'state',
    detail: ajaxDetail,
  })
  checks.push({
    key: 'discovery', label: '课程结构来源',
    ok: discoverySource !== null,
    detail: discoverySource ? `${discoverySource}，${discoveryMs} ms，${limiter.requests} 次请求` : '三层都失败',
  })

  try {
    const courses = await listEnrolledCourses(limiter, ctx)
    checks.push({
      key: 'ajax_courses', label: 'enrolled_courses_by_timeline AJAX',
      ok: true, detail: `${courses.length} 门课：${courses.slice(0, 6).map((c) => c.shortname).join(', ')}`,
    })
  } catch (reason) {
    checks.push({
      key: 'ajax_courses', label: 'enrolled_courses_by_timeline AJAX',
      ok: false, detail: reason instanceof MoodleAjaxError ? `${reason.errorcode}: ${reason.message}` : String(reason),
    })
  }

  const embeds = recordingEmbeds(doc)
  for (const anchor of Array.from(doc.querySelectorAll<HTMLAnchorElement>('a[href]'))) {
    if (LIBRARY_PATTERN.test(anchor.href)) libraryLinks += 1
    // Only real off-site players: skips javascript:/itpc: links and in-page
    // anchors of Moodle's own recording blocks.
    if (!/^https?:/i.test(anchor.href) || hostOf(anchor.href) === ctx.host) continue
    const recording = detectRecording(anchor.href, anchor.textContent ?? '')
    if (recording && !recordings.some((r) => r.url === recording.url)) recordings.push(recording)
  }
  const platforms = new Map<string, Set<string>>()
  for (const r of [...embeds, ...recordings]) {
    const hosts = platforms.get(r.provider) ?? new Set<string>()
    hosts.add(hostOf(r.url))
    platforms.set(r.provider, hosts)
  }
  checks.push({
    key: 'recordings', label: '录播平台',
    ok: platforms.size > 0 ? true : null,
    detail: platforms.size > 0
      ? `${Array.from(platforms, ([provider, hosts]) => `${provider}（${Array.from(hosts).join(', ')}）`).join('；')} · ${embeds.length > 0 ? 'iframe 嵌入' : '链接'}`
      : '这一页上没有录播嵌入或链接',
  })
  checks.push({
    key: 'library', label: 'Leganto / eReserve 链接',
    ok: null, detail: `${libraryLinks} 个（同步时跳过）`,
  })
  checks.push({
    key: 'fetch_estimate', label: '单课全量请求估计',
    ok: null,
    detail: `约 ${Object.entries(modtypes).filter(([type]) => ['resource', 'folder', 'book', 'page', 'cms', 'link'].includes(type)).reduce((sum, [, count]) => sum + count, 0) + 1} 次请求，并发 3、间隔 200ms`,
  })

  const report: DiagnosticsReport = {
    generatedAt: new Date().toISOString(),
    host: ctx.host,
    courseId: ctx.courseId,
    courseName: ctx.courseName,
    checks,
    modtypes,
    recordings,
    libraryLinks,
    discoverySource,
    discoveryMs,
    requestCount: limiter.requests,
    markdown: '',
  }
  report.markdown = renderMarkdown(report)
  return report
}

function renderMarkdown(report: DiagnosticsReport): string {
  const lines = [
    `# Moodle 特征表 · ${report.host}`,
    '',
    `课程：${report.courseName} (id ${report.courseId}) · ${report.generatedAt}`,
    '',
    '| 检查 | 结果 | 说明 |',
    '|---|---|---|',
    ...report.checks.map((check) => `| ${check.label} | ${check.ok === null ? '—' : check.ok ? '✅' : '❌'} | ${check.detail.replace(/\|/g, '/')} |`),
    '',
    '## modtype 分布',
    '',
    ...Object.entries(report.modtypes).sort((a, b) => b[1] - a[1]).map(([type, count]) => `- ${type}: ${count}`),
  ]
  if (report.recordings.length) {
    lines.push('', '## 录播链接', '', ...report.recordings.map((r) => `- ${r.provider}: ${r.url}`))
  }
  return lines.join('\n')
}
