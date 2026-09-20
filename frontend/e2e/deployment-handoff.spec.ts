import { expect, test, type WebSocketRoute } from '@playwright/test'

test.use({
  permissions: ['microphone'],
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
})

test('deployment handoff retains the recording and transcript while audio moves to a new connection', async ({ page }) => {
  const user = { id: 'handoff-user', tenant_id: 'tenant', email: 'test@example.test', role: 'user', is_active: true, email_verified: true }
  const token = `${Buffer.from('{"alg":"none"}').toString('base64url')}.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, sub: user.id })).toString('base64url')}.test`
  await page.addInitScript(({ user, token }) => {
    localStorage.setItem('dt_access_token', token)
    localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_onboarding_v1_user%3Ahandoff-user', JSON.stringify({ wizardCompletedAt: 1, tourCompletedAt: 1 }))
    localStorage.setItem('dt_unified_settings_v1', JSON.stringify({ translationEnabled: false, keepLocalAudio: true, audioSource: 'microphone' }))
  }, { user, token })
  let session = '', created = 0
  const sockets: WebSocketRoute[] = [], sessionIds: string[] = [], audio: number[] = []
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const request = route.request(), path = new URL(request.url()).pathname
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, anonymous_api_enabled: false, rag_enabled: true }
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/announcements') body = { announcements: [] }
    if (path === '/api/user/balance') body = { available_usd: 10, wallet_usd: 10, grant_usd: 0, plan_code: 'free' }
    if (path === '/api/speechmatics/preflight') body = { ready: true }
    if (path === '/api/sessions') {
      if (request.method() === 'POST') {
        const data = request.postDataJSON()
        session = data.client_session_id; created++
        body = { ...data, id: session, user_id: user.id, status: 'active' }
      } else body = { sessions: [] }
    }
    if (path.endsWith('/transcripts/batch')) body = { saved: request.postDataJSON(), count: request.postDataJSON().length }
    if (path === `/api/sessions/${session}`) body = { session: { id: session, status: 'active' }, transcripts: [] }
    if (path === '/api/ai/projects') body = { projects: [] }
    await route.fulfill({ json: body })
  })
  await page.routeWebSocket(/\/ws\/speechmatics/, socket => {
    const index = sockets.length
    sockets.push(socket); audio.push(0)
    sessionIds.push(new URL(socket.url()).searchParams.get('session_id') ?? '')
    socket.onMessage(message => {
      if (typeof message !== 'string') { audio[index] += message.length; return }
      const data = JSON.parse(message)
      if (data.message === 'StartRecognition') socket.send(JSON.stringify({ message: 'RecognitionStarted' }))
      if (data.message === 'EndOfStream') {
        socket.send(JSON.stringify({ message: 'EndOfTranscript' }))
        socket.close({ code: 1000, reason: 'finalized' })
      }
    })
  })
  await page.goto('/pro')
  await page.getByRole('button', { name: '开始新会话', exact: true }).click()
  await expect.poll(() => audio[0] ?? 0).toBeGreaterThan(0)
  sockets[0].send(JSON.stringify({ message: 'AddTranscript', metadata: { start_time: 0, end_time: 1, transcript: 'Retained through upgrade.' }, results: [{ alternatives: [{ content: 'Retained through upgrade.', speaker: 'S1' }] }] }))
  await expect(page.getByText('Retained through upgrade.', { exact: true }).first()).toBeVisible()
  sockets[0].send(JSON.stringify({ message: 'DeploymentHandoff', version: 1 }))
  await expect.poll(() => sockets.length).toBe(2)
  await expect.poll(() => audio[1]).toBeGreaterThan(0)
  expect(created).toBe(1)
  expect(sessionIds).toEqual([session, session])
  await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
  await expect(page.getByText('Retained through upgrade.', { exact: true }).first()).toBeVisible()
  await page.getByRole('button', { name: '停止录音', exact: true }).click()
  await expect(page.getByRole('button', { name: '开始新会话', exact: true })).toBeVisible()
})
