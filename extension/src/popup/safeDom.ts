import type { DiagnosticsReport, DreamTransProject, SyncSummary } from '../shared/types'

export function projectOptions(select: HTMLSelectElement, label: string, projects: DreamTransProject[] = []): void {
  select.replaceChildren(new Option(label, ''))
  for (const project of projects) select.add(new Option(project.name, project.id))
}

export function reportContents(container: HTMLElement, report: DiagnosticsReport): void {
  const table = document.createElement('table')
  for (const check of report.checks) {
    const row = table.insertRow()
    row.insertCell().textContent = check.label
    const mark = document.createElement('span')
    mark.className = check.ok === null ? 'na' : check.ok ? 'ok' : 'bad'
    mark.textContent = check.ok === null ? '—' : check.ok ? 'OK' : 'NO'
    row.insertCell().append(mark)
    row.insertCell().textContent = check.detail
  }
  const title = document.createElement('h4')
  title.textContent = 'MODTYPE 分布'
  const list = document.createElement('ul')
  const entries = Object.entries(report.modtypes).sort((a, b) => b[1] - a[1])
  for (const [type, count] of entries) {
    const item = document.createElement('li')
    item.textContent = `${type}: ${count}`
    list.append(item)
  }
  if (!entries.length) {
    const item = document.createElement('li')
    item.textContent = '无'
    list.append(item)
  }
  container.replaceChildren(table, title, list)
}

function listSection(title: string, lines: string[], className?: string): HTMLElement[] {
  const heading = document.createElement('h4')
  heading.textContent = title
  const list = document.createElement('ul')
  if (className) list.className = className
  for (const line of lines) {
    const item = document.createElement('li')
    item.textContent = line
    list.append(item)
  }
  return [heading, list]
}

export function summaryContents(container: HTMLElement, summary: SyncSummary): void {
  const stats = document.createElement('div')
  stats.className = 'stats'
  const tiles: Array<[string, number, string]> = [
    ['上传', summary.uploaded, 'ok'],
    ['服务器已有', summary.duplicates, ''],
    ['未变', summary.unchanged, ''],
    ['扫描模块', summary.scanned, ''],
    ['跳过', summary.skipped, ''],
    ['失败', summary.failed, 'bad'],
  ]
  for (const [label, value, tone] of tiles) {
    const tile = document.createElement('div')
    tile.className = `stat ${value === 0 ? 'zero' : tone}`
    const number = document.createElement('b')
    number.textContent = String(value)
    const caption = document.createElement('span')
    caption.textContent = label
    tile.append(number, caption)
    stats.append(tile)
  }
  const meta = document.createElement('p')
  meta.className = 'meta'
  meta.textContent = `${summary.stopped ? '已停止 · ' : ''}${summary.originals ? `原文件 ${summary.originals} · ` : ''}${summary.requests} 次请求 · ${(summary.durationMs / 1000).toFixed(1)} s`
  const parts: HTMLElement[] = [stats, meta]
  if (summary.recordings.length) {
    parts.push(...listSection(`录播 ${summary.recordings.length} 个（只记录，不抓取）`,
      summary.recordings.map((r) => `[${r.provider}] ${r.section} / ${r.name}`)))
  }
  if (summary.errors.length) parts.push(...listSection('问题', summary.errors, 'issues'))
  container.replaceChildren(...parts)
}
