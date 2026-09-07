import { expect, test, type Page } from '@playwright/test'

function audioFile(name: string) {
  const buffer = Buffer.alloc(44 + 32000)
  buffer.write('RIFF'); buffer.writeUInt32LE(buffer.length - 8, 4); buffer.write('WAVEfmt ', 8)
  buffer.writeUInt32LE(16, 16); buffer.writeUInt16LE(1, 20); buffer.writeUInt16LE(1, 22)
  buffer.writeUInt32LE(16000, 24); buffer.writeUInt32LE(32000, 28); buffer.writeUInt16LE(2, 32); buffer.writeUInt16LE(16, 34)
  buffer.write('data', 36); buffer.writeUInt32LE(32000, 40)
  return { name, mimeType: 'audio/wav', buffer }
}

async function setup(page: Page, member = true) {
  const user = { id: 'batch-user', tenant_id: 'tenant-1', email: 'batch@example.test', name: 'Batch', role: 'user', is_active: true, email_verified: true, training_opt_in: false }
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600, sub: user.id })}.test`
  await page.addInitScript(({ token, user }) => {
    localStorage.setItem('dt_access_token', token); localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_onboarding_v1_user%3Abatch-user', JSON.stringify({ wizardCompletedAt: 1, tourCompletedAt: 1 }))
  }, { token, user })
  const state = { uploads: 0, complete: false, affordable: true, reject: false, saveFailures: 0, saves: 0, texts: [] as string[], sessionIds: [] as string[] }
  const sessions: Array<Record<string, unknown>> = []
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const request = route.request(), url = new URL(request.url()), path = url.pathname
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, anonymous_api_enabled: false, rag_enabled: true }
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/announcements') body = { announcements: [] }
    const balance = { user_id: user.id, available_usd: 10, wallet_usd: 10, grant_usd: 0, plan_code: member ? 'pro' : 'free' }
    if (path === '/api/user/balance') body = balance
    if (path === '/api/user/billing/account') body = { account: { ...balance, grants: [], effective_plan: { code: balance.plan_code, features: { batch: member } }, realtime_hour_usd: 1, training_opt_in: false }, payments_enabled: true }
    if (path === '/api/user/billing/session-costs') body = { session_costs: [] }
    if (path === '/api/transcribe/batch/quote') body = { reservation_usd: 0.02, affordable: state.affordable }
    if (path === '/api/transcribe/batch/submit') {
      state.uploads++
      expect(url.searchParams.get('audio_format')).toBe('pcm16')
      expect(request.headers()['content-type']).toContain('multipart/form-data; boundary=')
      if (state.reject) { await route.fulfill({ status: 402, json: { error: 'insufficient balance' } }); return }
      body = { job_id: `job-${state.uploads}`, status: 'running' }
    }
    if (path === '/api/transcribe/batch/status') body = state.complete ? {
      job_id: url.searchParams.get('job_id'), status: 'done', transcript: { metadata: { duration: 1 }, results: [
        { type: 'word', start_time: 0, end_time: 0.4, alternatives: [{ content: 'Hello', speaker: 'S1' }] },
        { type: 'word', start_time: 0.4, end_time: 0.9, alternatives: [{ content: 'world', speaker: 'S1' }] },
        { type: 'punctuation', start_time: 0.9, end_time: 1, alternatives: [{ content: '.' }] },
      ] },
    } : { job_id: url.searchParams.get('job_id'), status: 'running' }
    if (path === '/api/sessions') {
      if (request.method() === 'POST') {
        const data = request.postDataJSON(); state.sessionIds.push(data.client_session_id)
        body = { ...data, id: data.client_session_id, user_id: user.id, status: 'active', created_at: new Date().toISOString(), started_at: new Date().toISOString() }
        if (!sessions.some(session => session.id === data.client_session_id)) sessions.push(body as Record<string, unknown>)
      } else body = { sessions, page: 1, page_size: 60 }
    }
    if (path.endsWith('/transcripts/batch')) {
      state.saves++
      if (state.saveFailures > 0) { state.saveFailures--; await route.fulfill({ status: 503, json: { error: 'save unavailable' } }); return }
      const segments = request.postDataJSON() as Array<{ text: string }>
      state.texts.push(...segments.map(segment => segment.text))
      body = { saved: segments, count: segments.length }
    }
    if (path.startsWith('/api/sessions/') && request.method() === 'PUT') body = { id: path.split('/')[3], ...request.postDataJSON() }
    if (path === '/api/ai/projects') body = { projects: [] }
    await route.fulfill({ json: body })
  })
  await page.goto('/pro')
  return state
}

test('Pro batch upload queues files, resumes after reload and saves readable history', async ({ page }) => {
  const state = await setup(page)
  await page.getByRole('button', { name: '批量转录' }).click()
  const dialog = page.getByRole('dialog', { name: '批量转录' })
  await dialog.getByLabel('音频语言', { exact: true }).selectOption('en')
  await dialog.getByLabel('选择音频文件').setInputFiles([audioFile('lecture.wav'), audioFile('meeting.wav')])
  await expect(dialog.getByText('等待提交', { exact: true })).toHaveCount(2)
  await dialog.getByRole('button', { name: '开始批量转录', exact: true }).click()
  await expect.poll(() => state.uploads).toBe(2)
  await expect(dialog.getByText('正在转录', { exact: true })).toHaveCount(2)
  await page.reload()
  state.complete = true
  await page.getByRole('button', { name: '批量转录' }).click()
  await expect(dialog.getByText('已保存到历史', { exact: true })).toHaveCount(2)
  expect(state.uploads).toBe(2)
  expect(state.texts).toEqual(['Hello world.', 'Hello world.'])
  expect(new Set(state.sessionIds).size).toBe(2)
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await expect(page.locator('.dt-sidebar__history').getByText('lecture.wav', { exact: true })).toBeVisible()
})

test('balance rejection pauses remaining files; retrying a failed save does not submit or create duplicates', async ({ page }) => {
  const state = await setup(page)
  await page.getByRole('button', { name: '批量转录' }).click()
  const dialog = page.getByRole('dialog', { name: '批量转录' })
  await dialog.getByLabel('选择音频文件').setInputFiles([audioFile('first.wav'), audioFile('second.wav')])
  await expect(dialog.getByText('等待提交', { exact: true })).toHaveCount(2)
  state.reject = true
  await dialog.getByRole('button', { name: '开始批量转录', exact: true }).click()
  await expect(dialog.getByRole('alert')).toContainText('余额不足')
  expect(state.uploads).toBe(1)
  state.reject = false; state.complete = true; state.saveFailures = 1
  await dialog.getByRole('button', { name: '开始批量转录', exact: true }).click()
  await expect(dialog.getByRole('button', { name: '重试查询 / 保存' })).toBeVisible()
  await dialog.getByRole('button', { name: '重试查询 / 保存' }).click()
  await expect(dialog.getByText('已保存到历史', { exact: true })).toHaveCount(2)
  expect(state.uploads).toBe(3)
  expect(new Set(state.sessionIds).size).toBe(2)
})

test('mobile menu exposes batch as a locked Pro feature for a free account', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const state = await setup(page, false)
  await page.locator('[data-tour="history-mobile"]').click()
  await page.getByRole('button', { name: '批量转录' }).click()
  await expect(page.getByText('批量转录是 Pro 功能。', { exact: false })).toBeVisible()
  await expect(page.getByLabel('选择音频文件')).toHaveCount(0)
  await expect(page.getByRole('button', { name: '查看会员方案' })).toBeVisible()
  expect(state.uploads).toBe(0)
})
