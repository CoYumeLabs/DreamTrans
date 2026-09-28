import { expect, test } from '@playwright/test'

const user = { id: 'history-user', tenant_id: 'tenant-1', email: 'history@example.test', name: 'History', role: 'user', is_active: true, email_verified: true }

// Newest first, matching the server's created_at DESC order.
const cloudSessions = Array.from({ length: 130 }, (_, index) => {
  const created = new Date(Date.UTC(2026, 0, 1) + (130 - index) * 3_600_000).toISOString()
  return {
    id: `cloud-${index}`, user_id: user.id, title: `云端会话 ${index}`, status: 'completed',
    duration_seconds: 60, created_at: created, updated_at: created,
  }
})

test('history sidebar loads older cloud sessions beyond the first page', async ({ page }) => {
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600, sub: user.id })}.test`
  await page.addInitScript(({ token, user }) => {
    localStorage.setItem('dt_access_token', token)
    localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_onboarding_v1_user%3Ahistory-user', JSON.stringify({ wizardCompletedAt: 1, tourCompletedAt: 1 }))
  }, { token, user })
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const url = new URL(route.request().url()), path = url.pathname
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, anonymous_api_enabled: false, rag_enabled: false }
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/announcements') body = { announcements: [] }
    if (path === '/api/user/billing/account') body = { account: { user_id: user.id, available_usd: 10, grants: [], effective_plan: { name: 'Pro' } }, payments_enabled: false }
    if (path === '/api/user/billing/session-costs') body = { session_costs: [] }
    if (path === '/api/sessions') {
      const page = Number(url.searchParams.get('page')), size = Number(url.searchParams.get('page_size'))
      body = { sessions: cloudSessions.slice((page - 1) * size, page * size), total: cloudSessions.length, page, page_size: size }
    }
    await route.fulfill({ json: body })
  })

  await page.goto('/pro')
  const history = page.locator('.dt-sidebar__history')
  const items = history.locator('.dt-history-item')
  const more = history.getByRole('button', { name: '加载更早的会话', exact: true })

  await expect(items).toHaveCount(60)
  await expect(items.first()).toContainText('云端会话 0')
  await more.click()
  await expect(items).toHaveCount(120)
  await more.click()
  await expect(items).toHaveCount(130)
  await expect(items.last()).toContainText('云端会话 129')
  await expect(more).toHaveCount(0)
})
