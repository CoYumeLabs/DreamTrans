import { test, expect, type Download, type Page } from '@playwright/test'
import JSZip from 'jszip'

const course = { id: 'download-course', name: '课程 / Biology', context_mode: 'smart', max_context_tokens: 64000 }
const sessions = [
  { id: 's1', title: '同名 / 课堂', started_at: '2026-03-02T10:00:00Z' },
  { id: 's2', title: '同名 / 课堂', started_at: '2026-03-03T10:00:00Z' },
  { id: 's3', title: '下一周课堂', started_at: '2026-03-09T10:00:00Z' },
]

function transcript(id: string, text: string, time: number, extra: Record<string, unknown> = {}) {
  return {
    id, client_segment_id: id, text, start_time: time, end_time: time + 1,
    speaker: 'S1', status: 'confirmed', is_partial: false,
    created_at: '2026-03-02T10:00:00Z', updated_at: '2026-03-02T10:00:00Z', ...extra,
  }
}

async function setup(page: Page) {
  const user = { id: 'download-user', tenant_id: 'tenant', email: 'download@example.test', name: 'Study', role: 'user', is_active: true }
  const token = `e2e.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.e2e`
  await page.addInitScript(({ user, token }) => {
    localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_access_token', token)
    localStorage.setItem('dt_refresh_token', 'refresh')
  }, { user, token })
  const state = { requests: [] as string[], fail: false, invalidCursor: false }
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, rag_enabled: true }
    else if (path === '/api/user/profile') body = { user }
    else if (path === '/api/user/balance') body = { available_usd: 20 }
    else if (path === '/api/ai/projects') body = { projects: [course] }
    else if (path.endsWith('/timetable')) body = { slots: [] }
    else if (path.endsWith('/sources')) body = { sources: [] }
    else if (path.endsWith('/sessions')) body = { sessions }
    else if (path.endsWith('/skill-map')) body = { map: null }
    else if (path.endsWith('/study/state')) body = { states: [], continue: null }
    else if (path.endsWith('/study/costs')) body = { items: [], summary: { total_usd: 0, by_feature: {} } }
    else if (path.endsWith('/study/weeks')) body = {
      current_week: 1, behind_weeks: [], week_start: '2026-03-02',
      weeks: [
        { week: 1, label: '第 1 周', status: 'current', sessions: sessions.slice(0, 2), sources: [], skills: [] },
        { week: 2, label: '第 2 周', status: 'upcoming', sessions: sessions.slice(2), sources: [], skills: [] },
        { week: 3, label: '第 3 周', status: 'empty', sessions: [], sources: [], skills: [] },
      ],
      unassigned: { sessions: [], sources: [], skills: [] },
    }
    else if (path.endsWith('/transcripts')) {
      const session = path.split('/')[3]
      state.requests.push(`${session}:${url.searchParams.get('after_id') || ''}`)
      if (state.fail && session === 's2') {
        await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'Export unavailable' }) })
        return
      }
      if (session === 's1' && !url.searchParams.has('after_id')) {
        body = {
          transcripts: [transcript('a', 'First half', 0, { translation_group_id: 'group-1' })],
          has_more: true,
          next_cursor: state.invalidCursor ? null : { start_time: 0, id: 'a' },
        }
      } else if (session === 's1') {
        expect(url.searchParams.get('after_start_time')).toBe('0')
        body = { transcripts: [
          transcript('b', 'and final page.', 1, { translation_group_id: 'group-1', translation: '跨页的完整译文。' }),
          transcript('partial', 'UNFINISHED', 2, { is_partial: true }),
        ], has_more: false, next_cursor: null }
      } else {
        body = { transcripts: [transcript(session, `${session} original.`, 65,
          session === 's3' ? { translation: '下一周的译文。' } : {})], has_more: false, next_cursor: null }
      }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) })
  })
  await page.goto('/pro/study')
  await page.getByRole('button', { name: /Biology/ }).click()
  await expect(page.getByRole('button', { name: '下载课程全部转录' })).toBeEnabled()
  return state
}

async function readArchive(download: Download) {
  const stream = await download.createReadStream()
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(Buffer.from(chunk))
  const zip = await JSZip.loadAsync(Buffer.concat(chunks))
  const entries = await Promise.all(Object.values(zip.files).map(async (file) => [file.name, await file.async('string')] as const))
  return Object.fromEntries(entries)
}

test('course downloads include every page and session in all three content formats', async ({ page }) => {
  const state = await setup(page)
  for (const mode of ['bilingual', 'original', 'translation']) {
    await page.getByLabel('转录下载内容').selectOption(mode)
    const downloadPromise = page.waitForEvent('download')
    await page.getByRole('button', { name: '下载课程全部转录' }).click()
    const download = await downloadPromise
    expect(download.suggestedFilename()).toMatch(/^课程 - Biology-.+\.zip$/)
    const files = await readArchive(download)
    const names = Object.keys(files)
    expect(names).toHaveLength(3)
    expect(names.every((name) => !name.includes('/'))).toBe(true)
    expect(names[0]).toContain('001-2026-03-02-同名 - 课堂')
    expect(names[1]).toContain('002-2026-03-03-同名 - 课堂')
    const [first, second, third] = Object.values(files)
    expect(first).toContain('[00:00:00] S1')
    expect(first).not.toContain('UNFINISHED')
    if (mode !== 'translation') {
      expect(first).toContain('First half and final page.')
      expect(second).toContain('[00:01:05] S1')
      expect(third).toContain('s3 original.')
    } else {
      expect(first).not.toContain('First half')
      expect(second).toContain('暂无已同步到云端的译文')
    }
    if (mode !== 'original') {
      expect(first.match(/跨页的完整译文。/g)).toHaveLength(1)
      expect(third).toContain('下一周的译文。')
    } else {
      expect(first).not.toContain('跨页的完整译文。')
      expect(third).not.toContain('下一周的译文。')
    }
  }
  expect(state.requests).toEqual(Array.from({ length: 3 }, () => ['s1:', 's1:a', 's2:', 's3:']).flat())
})

test('week downloads use the selected course week and disable empty weeks on mobile', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const state = await setup(page)
  await page.getByRole('tab', { name: /02/ }).click()
  await page.getByLabel('转录下载内容').selectOption('original')
  const downloadPromise = page.waitForEvent('download')
  await page.getByRole('button', { name: '下载本周转录' }).click()
  const download = await downloadPromise
  expect(download.suggestedFilename()).toContain('第 2 周')
  const files = await readArchive(download)
  expect(Object.values(files)).toHaveLength(1)
  expect(Object.values(files)[0]).toContain('s3 original.')
  expect(state.requests).toEqual(['s3:'])
  await page.getByRole('tab', { name: /03/ }).click()
  await expect(page.getByRole('button', { name: '下载本周转录' })).toBeDisabled()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('failed pages never download a partial archive and can be retried', async ({ page }) => {
  const state = await setup(page)
  let downloads = 0
  page.on('download', () => { downloads++ })
  for (const failure of ['fail', 'invalidCursor'] as const) {
    state[failure] = true
    await page.getByRole('button', { name: '下载课程全部转录' }).click()
    await expect(page.getByRole('alert')).toContainText('未生成压缩包')
    await expect(page.getByRole('button', { name: '下载课程全部转录' })).toBeEnabled()
    expect(downloads).toBe(0)
    state[failure] = false
  }
  const downloadPromise = page.waitForEvent('download')
  await page.getByRole('button', { name: '下载课程全部转录' }).click()
  expect(Object.keys(await readArchive(await downloadPromise))).toHaveLength(3)
})

test('cancelling an in-flight download stops subsequent pages and resets controls', async ({ page }) => {
  const state = await setup(page)
  let release!: () => void
  const blocked = new Promise<void>((resolve) => { release = resolve })
  let started = false
  await page.route('**/api/sessions/s1/transcripts?*', async (route) => {
    started = true
    await blocked
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({
      transcripts: [transcript('a', 'Cancelled text', 0)],
      has_more: true, next_cursor: { start_time: 0, id: 'a' },
    }) })
  })
  let downloads = 0
  page.on('download', () => { downloads++ })
  await page.getByRole('button', { name: '下载课程全部转录' }).click()
  await expect.poll(() => started).toBe(true)
  await expect(page.getByLabel('转录下载内容')).toBeDisabled()
  await expect(page.getByRole('button', { name: '下载本周转录' })).toBeDisabled()
  await page.locator('.dt-study__downloads').getByRole('button', { name: '取消' }).click()
  release()
  await expect(page.getByRole('button', { name: '下载课程全部转录' })).toBeEnabled()
  await expect(page.getByLabel('转录下载内容')).toBeEnabled()
  await expect(page.locator('.dt-study__downloads').getByRole('status')).toHaveCount(0)
  expect(state.requests).toEqual([])
  expect(downloads).toBe(0)
})
