import { sendToBackground, sendToTab, type ContentRequest, type ContentResponse, type ProgressMessage } from '../shared/messages'
import type { DiagnosticsReport, DreamTransProject, DreamTransStatus, MoodleContext, SyncSummary } from '../shared/types'
import { projectOptions, reportContents, summaryContents } from './safeDom'

// The popup: log in to DreamTrans once, pick which DreamTrans course this
// Moodle course maps to, then 诊断 or 同步. The sync itself runs in the
// Moodle tab and survives closing this popup.

const $ = <T extends HTMLElement>(selector: string) => document.querySelector<T>(selector)!

const ui = {
  accountState: $('#account-state'),
  loginForm: $<HTMLFormElement>('#login-form'),
  server: $<HTMLInputElement>('#server'),
  email: $<HTMLInputElement>('#email'),
  password: $<HTMLInputElement>('#password'),
  accountRow: $('#account-row'),
  accountWho: $('#account-who'),
  logout: $<HTMLButtonElement>('#logout'),
  moodleState: $('#moodle-state'),
  moodleCourse: $('#moodle-course'),
  project: $<HTMLSelectElement>('#project'),
  figures: $<HTMLInputElement>('#figures'),
  full: $<HTMLInputElement>('#full'),
  forums: $<HTMLInputElement>('#forums'),
  sync: $<HTMLButtonElement>('#sync'),
  diagnose: $<HTMLButtonElement>('#diagnose'),
  cancel: $<HTMLButtonElement>('#cancel'),
  progress: $('#progress'),
  bar: $('#progress .bar i'),
  progressText: $('#progress-text'),
  summary: $('#summary'),
  reportPanel: $('#report-panel'),
  report: $('#report'),
  copyReport: $<HTMLButtonElement>('#copy-report'),
}

let tabId = 0
let moodle: MoodleContext | null = null
let projects: DreamTransProject[] = []
let lastReport: DiagnosticsReport | null = null

function mappingKey(ctx: MoodleContext): string {
  return `dt.map.${ctx.host}.${ctx.courseId}`
}

async function ensureContentScript(): Promise<boolean> {
  try {
    await sendToTab(tabId, { type: 'moodle.ping' })
    return true
  } catch {
    try {
      await chrome.scripting.executeScript({ target: { tabId }, files: ['content/moodle.js'] })
      await sendToTab(tabId, { type: 'moodle.ping' })
      return true
    } catch {
      return false
    }
  }
}

async function ask<T extends ContentResponse>(request: ContentRequest): Promise<T> {
  const response = await sendToTab<T>(tabId, request)
  if (!response.ok) throw new Error((response as { error: string }).error)
  return response
}

function setAccount(status: DreamTransStatus): void {
  ui.loginForm.hidden = status.connected
  ui.accountRow.hidden = !status.connected
  ui.accountState.textContent = status.connected ? '已登录' : '未登录'
  ui.accountState.className = `muted${status.connected ? ' ok' : ''}`
  ui.accountWho.textContent = status.connected ? `${status.name || status.email || ''} · ${status.server.replace(/^https?:\/\//, '')}` : ''
  if (status.server) ui.server.value = status.server
}

async function loadProjects(): Promise<void> {
  projectOptions(ui.project, '加载课程…')
  try {
    const response = await sendToBackground<{ ok: true; projects: DreamTransProject[] }>({ type: 'dt.projects' })
    projects = response.projects
    projectOptions(ui.project, '选择课程', projects)
    if (moodle) {
      const stored = await chrome.storage.local.get(mappingKey(moodle))
      const saved = stored[mappingKey(moodle)] as string | undefined
      if (saved && projects.some((p) => p.id === saved)) ui.project.value = saved
      else {
        const guess = projects.find((p) => moodle!.shortname && p.name.toUpperCase().includes(moodle!.shortname.toUpperCase()))
        if (guess) ui.project.value = guess.id
      }
    }
  } catch (reason) {
    projectOptions(ui.project, `课程加载失败：${String(reason instanceof Error ? reason.message : reason)}`)
  }
  updateButtons()
}

function updateButtons(): void {
  const ready = Boolean(moodle && moodle.courseId > 0)
  ui.diagnose.disabled = !ready
  ui.sync.disabled = !ready || !ui.project.value
}

function showProgress(message: string, done?: number, total?: number): void {
  ui.progress.hidden = false
  ui.progressText.textContent = message
  if (done !== undefined && total) {
    ui.bar.classList.remove('indeterminate')
    ui.bar.style.setProperty('--pct', `${Math.max(4, Math.round((done / total) * 100))}%`)
  } else {
    ui.bar.classList.add('indeterminate')
  }
}

function showSummary(summary: SyncSummary): void {
  ui.summary.hidden = false
  summaryContents(ui.summary, summary)
}

function renderReport(report: DiagnosticsReport): void {
  lastReport = report
  ui.reportPanel.hidden = false
  reportContents(ui.report, report)
}

async function init(): Promise<void> {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true })
  tabId = tab?.id ?? 0
  const status = await sendToBackground<{ ok: true; status: DreamTransStatus }>({ type: 'dt.status' })
  setAccount(status.status)

  if (!tabId || !tab?.url || !/^https?:/.test(tab.url)) {
    ui.moodleState.textContent = '请先打开 Moodle 课程页'
    return
  }
  const injected = await ensureContentScript()
  if (!injected) {
    ui.moodleState.textContent = '无法读取这个页面'
    ui.moodleState.className = 'muted bad'
    return
  }
  const context = await ask<{ ok: true; context: MoodleContext }>({ type: 'moodle.context' })
  moodle = context.context
  if (moodle.courseId > 0) {
    ui.moodleState.textContent = moodle.sesskeySource === 'none' ? '页面上没有 sesskey' : moodle.host
    ui.moodleState.className = `muted${moodle.sesskeySource === 'none' ? ' bad' : ' ok'}`
    const prefix = moodle.shortname && !moodle.courseName.startsWith(moodle.shortname) ? `${moodle.shortname} · ` : ''
    ui.moodleCourse.textContent = `${prefix}${moodle.courseName}`
  } else {
    ui.moodleState.textContent = '不在课程页面上'
    ui.moodleCourse.textContent = '打开 course/view.php?id=… 再点这里'
  }
  if (status.status.connected) await loadProjects()
  updateButtons()
}

ui.loginForm.addEventListener('submit', async (event) => {
  event.preventDefault()
  const server = ui.server.value.trim()
  if (!server) return
  const origin = new URL(/^https?:\/\//.test(server) ? server : `https://${server}`).origin
  // The background needs this host to call DreamTrans without CORS.
  const granted = await chrome.permissions.request({ origins: [`${origin}/*`] })
  if (!granted) {
    ui.accountState.textContent = '需要授权访问 DreamTrans 服务器'
    ui.accountState.className = 'muted bad'
    return
  }
  ui.accountState.textContent = '登录中…'
  try {
    const response = await sendToBackground<{ ok: true; status: DreamTransStatus }>({
      type: 'dt.login', server, email: ui.email.value.trim(), password: ui.password.value,
    })
    ui.password.value = ''
    setAccount(response.status)
    await loadProjects()
  } catch (reason) {
    ui.accountState.textContent = reason instanceof Error ? reason.message : '登录失败'
    ui.accountState.className = 'muted bad'
  }
})

ui.logout.addEventListener('click', async () => {
  await sendToBackground({ type: 'dt.logout' })
  setAccount({ connected: false, server: ui.server.value })
  projectOptions(ui.project, '请先登录')
  updateButtons()
})

ui.project.addEventListener('change', async () => {
  if (moodle && ui.project.value) await chrome.storage.local.set({ [mappingKey(moodle)]: ui.project.value })
  updateButtons()
})

ui.diagnose.addEventListener('click', async () => {
  ui.diagnose.disabled = true
  showProgress('诊断中：读取课程结构、测试 AJAX、扫描录播和图书馆链接…')
  try {
    const response = await ask<{ ok: true; report: DiagnosticsReport }>({ type: 'moodle.diagnose' })
    renderReport(response.report)
    showProgress(`诊断完成 · ${response.report.requestCount} 次请求 · 未上传任何内容`, 1, 1)
  } catch (reason) {
    showProgress(`诊断失败：${reason instanceof Error ? reason.message : String(reason)}`, 1, 1)
  } finally {
    updateButtons()
  }
})

ui.sync.addEventListener('click', async () => {
  if (!ui.project.value) return
  ui.sync.disabled = true
  ui.cancel.hidden = false
  ui.summary.hidden = true
  showProgress('开始同步…')
  try {
    const response = await ask<{ ok: true; summary: SyncSummary }>({
      type: 'moodle.sync',
      options: { projectId: ui.project.value, full: ui.full.checked, uploadFigures: ui.figures.checked, includeForums: ui.forums.checked },
    })
    showSummary(response.summary)
  } catch (reason) {
    showProgress(`同步失败：${reason instanceof Error ? reason.message : String(reason)}`, 1, 1)
  } finally {
    ui.cancel.hidden = true
    updateButtons()
  }
})

ui.cancel.addEventListener('click', () => {
  void ask({ type: 'moodle.cancel' }).catch(() => undefined)
})

ui.copyReport.addEventListener('click', async () => {
  if (!lastReport) return
  await navigator.clipboard.writeText(lastReport.markdown)
  ui.copyReport.textContent = '已复制'
  setTimeout(() => { ui.copyReport.textContent = '复制 Markdown' }, 1500)
})

chrome.runtime.onMessage.addListener((message: ProgressMessage) => {
  if (message?.type !== 'moodle.progress') return
  const { progress } = message
  showProgress(progress.message, progress.done, progress.total)
})

void init()
