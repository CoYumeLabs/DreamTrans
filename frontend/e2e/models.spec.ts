import { expect, test } from '@playwright/test'

const accessToken = () => `${Buffer.from('{"alg":"none"}').toString('base64url')}.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.test`
const user = { id: 'super-user', tenant_id: 'tenant-1', email: 'super@example.test', name: 'Super', role: 'super_admin', is_active: true, email_verified: true }

// A second provider's model carries a "provider::model" id; approving it must
// send that id, and the page must show every provider's sync state.
test('a second provider model is listed under its own status and approved by its qualified id', async ({ page }) => {
  await page.addInitScript(token => localStorage.setItem('dt_access_token', token), accessToken())
  const policies: Array<Record<string, unknown>> = []
  const model = (provider: string, id: string, approved = false) => ({
    provider, model_id: id, qualified_id: provider === 'openai-compatible' ? id : `${provider}::${id}`, source: 'provider', provider_available: true,
    availability_status: 'provider_confirmed', first_seen_at: '2026-09-09T00:00:00Z', last_seen_at: '2026-09-09T00:00:00Z',
    policies: ['translation', 'summary', 'chat'].map(purpose => ({ purpose, model_id: provider === 'openai-compatible' ? id : `${provider}::${id}`, is_approved: approved, is_default: approved, cost_confirmed: true })),
  })
  const catalog = () => ({
    provider: 'openai-compatible', status: 'provider_confirmed', refresh_minutes: 30, last_success_at: '2026-09-09T01:00:00Z', last_attempt_at: '2026-09-09T01:00:00Z',
    providers: [
      { provider: 'openai-compatible', status: 'provider_confirmed', last_success_at: '2026-09-09T01:00:00Z', last_attempt_at: '2026-09-09T01:00:00Z' },
      { provider: 'cerebras', status: 'temporarily_unavailable', last_attempt_at: '2026-09-09T01:00:00Z', last_error: 'provider models request returned status 401' },
    ],
    models: [model('openai-compatible', 'gpt-5.6-sol', true), model('cerebras', 'qwen-3.8-27b', policies.some(p => p.model_id === 'cerebras::qwen-3.8-27b' && p.is_approved))],
  })
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const request = route.request(), path = new URL(request.url()).pathname
    let body: unknown = {}
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/admin/access') body = { allowed: true, super: true, tenant_admin: false, role_id: 'super', role_key: 'super', name: '超级管理员', permissions: [], channels: [] }
    if (path === '/api/admin/models') body = catalog()
    if (path === '/api/admin/models/policies') { policies.push(request.postDataJSON()); body = catalog() }
    if (path === '/api/admin/billing/catalog') body = { rates: [], markup_percent: 0, overrides: [], version: 'test' }
    await route.fulfill({ json: body })
  })
  await page.goto('/pro/admin')
  await page.getByRole('button', { name: '模型与定价', exact: true }).click()
  await expect(page.getByText('cerebras::qwen-3.8-27b')).toBeVisible()
  await expect(page.getByText('provider models request returned status 401')).toBeVisible()
  const row = page.getByRole('row', { name: /cerebras::qwen-3\.8-27b/ })
  await row.getByRole('button', { name: '翻译', exact: true }).click()
  await expect.poll(() => policies.length).toBe(1)
  expect(policies[0]).toMatchObject({ purpose: 'translation', model_id: 'cerebras::qwen-3.8-27b', is_approved: true })
  await expect(row.getByRole('button', { name: '翻译 ✓', exact: true })).toBeVisible()
})
