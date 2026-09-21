import { expect, test, type Page, type Route, type WebSocketRoute } from '@playwright/test'

test.use({
  permissions: ['microphone'],
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'] },
})

const user = { id: 'balance-user', tenant_id: 'tenant-1', email: 'balance@example.test', name: 'Balance', role: 'user', is_active: true, email_verified: true }

function balance(available: number, owner = user.id) {
  return {
    user_id: owner, account_id: 'balance-account', available_usd: available,
    wallet_usd: available, grant_usd: 0, lifetime_charged_usd: 0,
    plan_code: 'pro', member_active: true, auto_topup_enabled: false,
  }
}

function account(available: number, name = 'Initial plan', owner = user.id) {
  return {
    ...balance(available, owner), grants: [], realtime_hour_usd: 1,
    estimated_realtime_hours: available, training_opt_in: false,
    effective_plan: { name }, has_payment_method: true,
  }
}

async function setup(page: Page) {
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600, sub: user.id })}.test`
  await page.addInitScript(({ token, user }) => {
    localStorage.setItem('dt_access_token', token)
    localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_onboarding_v1_user%3Abalance-user', JSON.stringify({ wizardCompletedAt: 1, tourCompletedAt: 1 }))
    localStorage.setItem('dt_unified_settings_v1', JSON.stringify({ translationEnabled: false, keepLocalAudio: false, audioSource: 'microphone' }))
  }, { token, user })
  let holdAccount = false
  const pendingAccounts: Route[] = []
  const pendingBalances: Route[] = []
  const sockets: WebSocketRoute[] = []
  let sessionId = ''
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const request = route.request(), path = new URL(request.url()).pathname
    if (path === '/api/user/balance') { pendingBalances.push(route); return }
    if (path === '/api/user/billing/account' && holdAccount) { pendingAccounts.push(route); return }
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, anonymous_api_enabled: false, rag_enabled: true }
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/announcements') body = { announcements: [] }
    if (path === '/api/user/billing/account') body = { account: account(10), payments_enabled: false }
    if (path === '/api/user/billing/plans') body = { plans: [], payments_enabled: true }
    if (path === '/api/user/billing/session-costs') body = { session_costs: [] }
    if (path === '/api/speechmatics/preflight') body = { ready: true }
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
  await page.routeWebSocket(/\/ws\/speechmatics/, socket => {
    sockets.push(socket)
    socket.onMessage(message => {
      if (typeof message !== 'string') return
      const data = JSON.parse(message)
      if (data.message === 'StartRecognition') socket.send(JSON.stringify({ message: 'RecognitionStarted' }))
      if (data.message === 'EndOfStream') socket.send(JSON.stringify({ message: 'EndOfTranscript' }))
    })
  })
  await page.goto('/pro')
  await page.getByRole('button', { name: '开始新会话', exact: true }).click()
  await expect(page.getByRole('button', { name: '暂停录音', exact: true })).toBeVisible()
  await expect(page.locator('.dt-account-chip')).toContainText('US$10.00')
  return {
    pendingAccounts, pendingBalances,
    costOnly: () => sockets[0].send(JSON.stringify({ message: 'BalanceUpdated', cost_usd: 0.25 })),
    push: (available: number) => sockets[0].send(JSON.stringify({ message: 'BalanceUpdated', balance: balance(available) })),
    refreshAccount: async () => {
      await page.locator('.dt-account-chip').click()
      await expect(page.getByText('当前方案：Initial plan', { exact: true })).toBeVisible()
      holdAccount = true
      await page.locator('.dt-redeem input').fill('REFRESH-ACCOUNT')
      await page.locator('.dt-redeem button').click()
      await expect.poll(() => pendingAccounts.length).toBe(1)
    },
  }
}

async function completeResponse(page: Page, route: Route, json: unknown, status = 200) {
  const responsePromise = page.waitForResponse(response => response.request() === route.request())
  await route.fulfill({ status, json })
  await (await responsePromise).finished()
  // Let the response's promise handlers and React commit run before asserting
  // that an obsolete response did not replace an already-visible snapshot.
  await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
}

for (const outcome of ['success', 'failure', 'wrong owner'] as const) {
  test(`late balance HTTP ${outcome} cannot replace a newer WebSocket balance`, async ({ page }) => {
    const fixture = await setup(page)
    fixture.costOnly()
    await expect.poll(() => fixture.pendingBalances.length).toBe(1)
    fixture.push(7.25)
    await expect(page.locator('.dt-account-chip')).toContainText('US$7.25')
    await completeResponse(page, fixture.pendingBalances[0], outcome === 'failure'
      ? { error: 'temporary balance failure' }
      : balance(9, outcome === 'wrong owner' ? 'other-user' : user.id), outcome === 'failure' ? 500 : 200)
    await expect(page.locator('.dt-account-chip')).toContainText('US$7.25')

    // A refresh started after the push is still allowed to update the balance.
    fixture.costOnly()
    await expect.poll(() => fixture.pendingBalances.length).toBe(2)
    await completeResponse(page, fixture.pendingBalances[1], balance(6.5))
    await expect(page.locator('.dt-account-chip')).toContainText('US$6.50')
  })

  for (const concurrentCost of [false, true]) {
    test(`late account HTTP ${outcome} preserves a newer WebSocket balance (concurrent cost refresh: ${concurrentCost})`, async ({ page }) => {
      const fixture = await setup(page)
      await fixture.refreshAccount()
      if (concurrentCost) {
        fixture.costOnly()
        await expect.poll(() => fixture.pendingBalances.length).toBe(1)
      }
      fixture.push(7.25)
      await expect(page.locator('.dt-account-chip')).toContainText('US$7.25')
      if (concurrentCost) await completeResponse(page, fixture.pendingBalances[0], balance(9))
      await completeResponse(page, fixture.pendingAccounts[0], outcome === 'failure'
        ? { error: 'temporary account failure' }
        : { account: account(9, 'Updated plan', outcome === 'wrong owner' ? 'other-user' : user.id), payments_enabled: true }, outcome === 'failure' ? 500 : 200)
      await expect(page.locator('.dt-account-chip')).toContainText('US$7.25')
      if (outcome === 'success') {
        await expect(page.getByText('当前方案：Updated plan', { exact: true })).toBeVisible()
        await expect(page.locator('.dt-redeem input')).toBeEnabled()
        await expect(page.locator('.dt-billing-amount strong')).toHaveText('US$7.25')
        await expect(page.getByRole('button', { name: '管理会员 / 发票', exact: true })).toBeEnabled()
      }
    })
  }
}

test('auth changes invalidate both pending account and balance requests', async ({ page }) => {
  const fixture = await setup(page)
  await fixture.refreshAccount()
  fixture.costOnly()
  await expect.poll(() => fixture.pendingBalances.length).toBe(1)
  // Even a same-owner auth refresh must invalidate the old requests. A changed
  // owner would also be rejected by the existing user ID guards.
  await page.evaluate(() => window.dispatchEvent(new Event('dt-auth-changed')))
  await expect.poll(() => fixture.pendingAccounts.length).toBe(2)
  await completeResponse(page, fixture.pendingAccounts[1], { account: account(4, 'Reauthenticated plan'), payments_enabled: true })
  await expect(page.locator('.dt-account-chip')).toContainText('US$4.00')
  await completeResponse(page, fixture.pendingBalances[0], balance(9))
  await completeResponse(page, fixture.pendingAccounts[0], { account: account(8, 'Obsolete plan'), payments_enabled: false })
  await expect(page.locator('.dt-account-chip')).toContainText('US$4.00')
  await expect(page.getByText('当前方案：Reauthenticated plan', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '管理会员 / 发票', exact: true })).toBeEnabled()
})
