import { expect, test, type Page, type WebSocketRoute } from '@playwright/test'

test.use({
  permissions: ['microphone'],
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
})

async function setup(page: Page, transport: 'main' | 'edge', insufficientBalance = false) {
  const user = { id: 'routing-user', tenant_id: 'tenant-1', email: 'routing@example.test', name: 'Routing', role: 'user', is_active: true, email_verified: true }
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600, sub: user.id })}.test`
  await page.addInitScript(({ token, user }) => {
    localStorage.setItem('dt_access_token', token)
    localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_onboarding_v1_user%3Arouting-user', JSON.stringify({ wizardCompletedAt: 1, tourCompletedAt: 1 }))
    localStorage.setItem('dt_unified_settings_v1', JSON.stringify({ translationEnabled: false, keepLocalAudio: false, audioSource: 'microphone' }))
  }, { token, user })
  const authorizations: Array<Record<string, unknown>> = []
  const sockets: WebSocketRoute[] = []
  const mainPreflights: string[] = []
  let sessionId = ''
  let mainProbes = 0
  await page.route('https://edge.example.test/probe', route => route.fulfill({ status: 204, headers: { 'access-control-allow-origin': '*' } }))
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const request = route.request(), url = new URL(request.url()), path = url.pathname
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, anonymous_api_enabled: false, edge_enabled: true, edge_control_enabled: true }
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/announcements') body = { announcements: [] }
    if (path === '/api/edges/probe') { mainProbes++; body = { ready: true } }
    if (path === '/api/user/billing/session-costs') body = { session_costs: [] }
    if (path === '/api/user/billing/account') body = { account: { user_id: user.id, available_usd: 10, wallet_usd: 10 }, payments_enabled: false }
    if (path === '/api/speechmatics/preflight') {
      body = { ready: true }
      if (url.searchParams.get('transport') === 'main') {
        mainPreflights.push(request.url())
        if (insufficientBalance) { await route.fulfill({ status: 402, json: { error: 'insufficient balance' } }); return }
      }
    }
    if (path === '/api/edges') body = [
      { id: 'main', region: 'main', endpoint: '', mode: 'enabled' },
      { id: 'sydney', region: 'ap-southeast-2', endpoint: 'https://edge.example.test', mode: 'enabled' },
    ]
    if (path === '/api/edges/authorize') {
      const data = request.postDataJSON()
      authorizations.push(data)
      body = transport === 'main' ? { transport: 'main' } : {
        endpoint: 'https://edge.example.test', token: 'short-edge-grant',
        grant: { protocol: 2, session_id: sessionId, generation: authorizations.length,
          previous_generation: authorizations.length - 1, previous_audio_sequence: 0,
          durable_audio_sequence: 0, sample_rate: data.sample_rate },
      }
    }
    if (path === '/api/sessions') {
      if (request.method() === 'POST') {
        const data = request.postDataJSON()
        sessionId = data.client_session_id
        body = { ...data, id: sessionId, user_id: user.id, status: 'active' }
      } else body = { sessions: [], page: 1, page_size: 60 }
    }
    if (path.endsWith('/transcripts/batch')) body = { saved: request.postDataJSON(), count: request.postDataJSON().length }
    if (path === `/api/sessions/${sessionId}`) body = { session: { id: sessionId, user_id: user.id, status: 'active' }, transcripts: [] }
    if (path === '/api/ai/projects') body = { projects: [] }
    await route.fulfill({ json: body })
  })
  await page.routeWebSocket(/\/ws\/(speechmatics|edge)/, socket => {
    sockets.push(socket)
    socket.onMessage(message => {
      if (typeof message !== 'string') return
      const data = JSON.parse(message)
      if (data.message === 'StartRecognition') socket.send(JSON.stringify({ message: 'RecognitionStarted' }))
      if (data.message === 'EndOfStream') socket.send(JSON.stringify({ message: 'EndOfTranscript' }))
    })
  })
  await page.goto('/pro')
  return { authorizations, sockets, mainPreflights, mainProbes: () => mainProbes }
}

test('main is selectable while Edge routing is enabled and uses the metered main socket', async ({ page }) => {
  const fixture = await setup(page, 'main')
  await page.locator('[data-tour="session-setup"]').click()
  const node = page.getByLabel('接入节点')
  await expect(node.locator('option')).toHaveText(['自动（按延迟和空闲容量选择）', '主站', '悉尼'])
  await node.selectOption('main')
  await expect(page.locator('[data-tour="session-setup"]')).toContainText('主站')
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: '开始新会话', exact: true }).click()
  await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
  expect(fixture.authorizations[0]).toMatchObject({ allow_main: true, region: 'main' })
  expect(fixture.mainProbes()).toBeGreaterThan(0)
  expect(fixture.mainPreflights.length).toBeGreaterThan(0)
  expect(fixture.sockets[0].url()).toContain('/ws/speechmatics')
})

for (const transport of ['main', 'edge'] as const) {
  test(`automatic selection uses ${transport} and retains that transport on reconnect`, async ({ page }) => {
    const fixture = await setup(page, transport)
    await page.getByRole('button', { name: '开始新会话', exact: true }).click()
    await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
    expect(fixture.authorizations[0]).toMatchObject({ allow_main: true, region: 'auto' })
    expect(fixture.sockets[0].url()).toContain(transport === 'main' ? '/ws/speechmatics' : '/ws/edge')
    await page.evaluate(() => localStorage.setItem('dreamtrans.edge.region', 'main'))
    fixture.sockets[0].close({ code: 1012, reason: 'test restart' })
    await expect.poll(() => fixture.sockets.length).toBe(2)
    expect(fixture.sockets[1].url()).toContain(transport === 'main' ? '/ws/speechmatics' : '/ws/edge')
    if (transport === 'main') expect(fixture.authorizations).toHaveLength(1)
    else {
      expect(fixture.authorizations[1].allow_main).toBeUndefined()
      expect(fixture.authorizations[1].region).toBe('auto')
    }
  })
}

test('main selection reports insufficient balance before opening an audio connection', async ({ page }) => {
  const fixture = await setup(page, 'main', true)
  await page.getByRole('button', { name: '开始新会话', exact: true }).click()
  await expect.poll(() => fixture.mainPreflights.length).toBeGreaterThan(0)
  await expect(page.getByRole('button', { name: '开始新会话', exact: true })).toBeVisible()
  expect(fixture.sockets).toHaveLength(0)
})

test('a stored region whose nodes are gone falls back to automatic instead of being sent unseen', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('dreamtrans.edge.region', 'eu-west-2'))
  const fixture = await setup(page, 'edge')
  await page.locator('[data-tour="session-setup"]').click()
  await expect(page.getByLabel('接入节点')).toHaveValue('auto')
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: '开始新会话', exact: true }).click()
  await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
  expect(fixture.authorizations[0]).toMatchObject({ region: 'auto' })
})
