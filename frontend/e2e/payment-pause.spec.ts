import { expect, test, type WebSocketRoute } from '@playwright/test'

test.use({
  permissions: ['microphone'],
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
})

for (const source of ['transcription', 'translation'] as const) {
  test(`${source} balance exhaustion pauses capture and resumes the same session after top-up`, async ({ page }) => {
    if (source === 'translation') await page.setViewportSize({ width: 390, height: 844 })
    const user = { id: 'payment-user', tenant_id: 'tenant-1', email: 'test@example.test', name: 'Test', role: 'user', is_active: true, email_verified: true }
    const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
    const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600, sub: user.id })}.test`
    await page.addInitScript(({ token, user, source }) => {
      localStorage.setItem('dt_access_token', token)
      localStorage.setItem('dt_user', JSON.stringify(user))
      localStorage.setItem('dt_onboarding_v1_user%3Apayment-user', JSON.stringify({ wizardCompletedAt: 1, tourCompletedAt: 1 }))
      localStorage.setItem('dt_unified_settings_v1', JSON.stringify({ translationEnabled: source === 'translation', keepLocalAudio: true, audioSource: 'microphone' }))
    }, { token, user, source })
    let affordable = true
    let sessionId = ''
    let created = 0
    let audioBytes = 0
    let preflights = 0
    const speechSockets: WebSocketRoute[] = []
    const speechSessionIds: string[] = []
    const translationSockets: WebSocketRoute[] = []
    await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
      const request = route.request(), path = new URL(request.url()).pathname
      let body: unknown = {}
      if (path === '/api/system/access') body = { authentication_enabled: true, anonymous_api_enabled: false, rag_enabled: true }
      if (path === '/api/user/profile') body = { user }
      if (path === '/api/announcements') body = { announcements: [] }
      const balance = { user_id: user.id, available_usd: affordable ? 10 : 0, wallet_usd: affordable ? 10 : 0, grant_usd: 0, plan_code: 'free' }
      if (path === '/api/user/balance') body = balance
      if (path === '/api/user/billing/account') body = { account: { ...balance, grants: [], realtime_hour_usd: 1, training_opt_in: false }, payments_enabled: true }
      if (path === '/api/user/billing/session-costs') body = { session_costs: [] }
      if (path === '/api/speechmatics/preflight') {
        preflights++
        if (!affordable) { await route.fulfill({ status: 402, json: { error: 'insufficient balance' } }); return }
        body = { ready: true }
      }
      if (path === '/api/sessions') {
        if (request.method() === 'POST') {
          const data = request.postDataJSON()
          sessionId = data.client_session_id
          created++
          body = { ...data, id: sessionId, user_id: user.id, status: 'active' }
        } else body = { sessions: [], page: 1, page_size: 60 }
      }
      if (path.endsWith('/transcripts/batch')) body = { saved: request.postDataJSON(), count: request.postDataJSON().length }
      if (path === `/api/sessions/${sessionId}`) body = { session: { id: sessionId, user_id: user.id, status: 'active' }, transcripts: [] }
      if (path === '/api/rag/title') body = { title: 'Payment test' }
      if (path === '/api/ai/projects') body = { projects: [] }
      await route.fulfill({ json: body })
    })
    await page.routeWebSocket(/\/ws\/speechmatics/, socket => {
      speechSockets.push(socket)
      speechSessionIds.push(new URL(socket.url()).searchParams.get('session_id') ?? '')
      socket.onMessage(message => {
        if (typeof message !== 'string') { audioBytes += message.length; return }
        const data = JSON.parse(message)
        if (data.message === 'StartRecognition') socket.send(JSON.stringify({ message: 'RecognitionStarted' }))
        if (data.message === 'EndOfStream') socket.send(JSON.stringify({ message: 'EndOfTranscript' }))
      })
    })
    await page.routeWebSocket(/\/ws\/translate/, socket => {
      translationSockets.push(socket)
      socket.onMessage(message => {
        if (typeof message === 'string' && JSON.parse(message).type === 'init') {
          socket.send(JSON.stringify({ message: 'Info', reason: 'translator initialized', capabilities: { request_ids: true, atomic_transcripts: true } }))
        }
      })
    })
    await page.goto('/pro')
    await page.getByRole('button', { name: '开始新会话', exact: true }).click()
    await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
    await expect.poll(() => audioBytes).toBeGreaterThan(0)
    speechSockets[0].send(JSON.stringify({ message: 'AddTranscript', metadata: { start_time: 0, end_time: 1, transcript: 'Keep this original.' }, results: [{ start_time: 0, end_time: 1, alternatives: [{ content: 'Keep this original.', speaker: 'S1' }] }] }))
    await expect(page.getByText('Keep this original.', { exact: true }).first()).toBeVisible()
    affordable = false
    if (source === 'translation') await expect.poll(() => translationSockets.length).toBe(1)
    const failingSocket = source === 'transcription' ? speechSockets[0] : translationSockets[0]
    failingSocket.send(JSON.stringify({ message: 'Error', type: 'insufficient_balance', reason: 'insufficient balance', retryable: false }))
    const notice = page.getByRole('alert').filter({ hasText: '余额不足 · 已暂停' })
    await expect(notice).toBeVisible()
    await expect(page.getByRole('button', { name: '继续录音', exact: true })).toBeVisible()
    const timer = page.locator('.dt-recorder__time strong')
    const pausedTime = await timer.textContent()
    const pausedBytes = audioBytes
    // Let a reconnect delay and several capture ticks pass: neither may restart work.
    await page.waitForTimeout(2200)
    expect(await timer.textContent()).toBe(pausedTime)
    expect(audioBytes).toBe(pausedBytes)
    expect(speechSockets).toHaveLength(1)
    const checks = preflights
    await notice.getByRole('button', { name: '充值后恢复' }).click()
    await expect.poll(() => preflights).toBe(checks + 1)
    await expect(notice.getByRole('button', { name: '充值后恢复' })).toBeEnabled()
    expect(speechSockets).toHaveLength(1)
    await expect(notice).toBeVisible()
    affordable = true
    await notice.getByRole('button', { name: '充值后恢复' }).click()
    await expect(notice).toHaveCount(0)
    await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
    await expect.poll(() => audioBytes).toBeGreaterThan(pausedBytes)
    expect(created).toBe(1)
    expect(speechSessionIds).toEqual([sessionId, sessionId])
    await expect(page.getByText('Keep this original.', { exact: true }).first()).toBeVisible()
    // Stop from another payment hold must finish locally without attempting payment.
    speechSockets[1].send(JSON.stringify({ message: 'Error', type: 'insufficient_balance', reason: 'insufficient balance' }))
    await expect(notice).toBeVisible()
    await page.getByRole('button', { name: '停止录音', exact: true }).click()
    await expect(page.getByRole('button', { name: '开始新会话', exact: true })).toBeVisible()
    expect(speechSockets).toHaveLength(2)
    const audio = await page.evaluate(async () => {
      const request = indexedDB.open('dreamtrans-db')
      const db = await new Promise<IDBDatabase>((resolve, reject) => { request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error) })
      const chunks = db.transaction('audio-chunks').objectStore('audio-chunks').getAll()
      const records = await new Promise<Array<{ blob: Blob }>>((resolve, reject) => { chunks.onsuccess = () => resolve(chunks.result); chunks.onerror = () => reject(chunks.error) })
      db.close()
      return records.length
    })
    expect(audio).toBeGreaterThan(0)
  })
}
