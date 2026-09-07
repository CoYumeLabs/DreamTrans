import { expect, test } from '@playwright/test'

test('super admin reviews signup rewards and can adjust thresholds', async ({ page }) => {
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600 })}.e2e`
  await page.addInitScript((access) => localStorage.setItem('dt_access_token', access), token)
  const user = { id: 'admin-1', role: 'super_admin', name: 'Admin', email: 'admin@example.test' }
  let settings = { strict_mode: true, network_burst_limit: 3, prefix_hourly_limit: 10, daily_reward_budget_cents: 10000, enabled: true, device_limit: 1, network_daily_limit: 5, automatic_daily_limit: 100 }
  let profile = { score: 90, budget_blocked: true, evidence: { browser: 'Chrome', platform: 'Linux', network_burst: 3, prefix_hourly: 4, fingerprint_count: 3, linked_denied: 1 }, id: 'risk-1', user_id: 'user-1', name: '学生', email: 'student@example.test', verified: true, decision: 'review', reasons: ['device_accounts'], device_count: 1, network_count: 2, daily_count: 3, rules: settings, created_at: '2026-09-06T01:00:00Z', reviewed_at: null, review_note: '', promotion: '开学季', channel: '校园社团' }
  let reviewBody: unknown
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const url = new URL(route.request().url())
    const method = route.request().method()
    let data: unknown = {}
    if (url.pathname === '/api/admin/access') data = { allowed: true, super: true, tenant_admin: false, role_id: '', role_key: 'super_admin', name: '超级管理员', permissions: ['*'], channels: [] }
    if (url.pathname === '/api/user/profile') data = { user }
    if (url.pathname === '/api/admin/stats') data = { basic: { user_count: 1, tenant_count: 1, session_count: 0, transcript_count: 0 } }
    if (url.pathname === '/api/admin/signup-risk/budget') data = { limit_cents: settings.daily_reward_budget_cents, spent_usd: '2.500000', blocked: 1 }
    if (url.pathname === '/api/admin/signup-risk/settings') {
      if (method === 'PUT') settings = route.request().postDataJSON()
      data = settings
    }
    if (url.pathname === '/api/admin/signup-risk') {
      const profiles = profile.decision === url.searchParams.get('decision') ? [profile] : []
      data = { profiles, total: profiles.length }
    }
    if (url.pathname === '/api/admin/signup-risk/risk-1' && method === 'POST') {
      reviewBody = route.request().postDataJSON()
      profile = { ...profile, ...route.request().postDataJSON() }
      data = { ok: true }
    }
    if (url.pathname === '/api/admin/signup-risk/risk-1' && method === 'GET') data = { audit: [] }
    await route.fulfill({ json: data })
  })
  await page.goto('/pro/admin')
  await page.getByRole('button', { name: '注册风控', exact: true }).click()
  await expect(page.getByRole('cell', { name: /同一浏览器重复注册/ })).toBeVisible()
  await page.getByRole('button', { name: '查看 / 审核' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('风险分数 90 / 100')
  await expect(dialog).toContainText('赠送因预算上限暂缓')
  await expect(dialog).toContainText('关联拒绝记录 1')
  await expect(dialog.getByRole('button', { name: '放行赠送权益' })).toBeDisabled()
  await dialog.getByLabel('审核备注', { exact: true }).fill('已核实为社团共用电脑，不同学生')
  await dialog.getByRole('button', { name: '放行赠送权益' }).click()
  await expect(dialog).toHaveCount(0)
  expect(reviewBody).toEqual({ decision: 'approved', note: '已核实为社团共用电脑，不同学生' })
  await expect(page.getByText('暂无匹配记录')).toBeVisible()
  await page.getByText('风控规则与阈值', { exact: true }).click()
  await expect(page.getByLabel('严格模式：所有新注册赠送先人工审核')).toBeChecked()
  await page.getByLabel('滚动 24 小时赠送余额预算（美元）').fill('50')
  await page.getByLabel('同一网络 24 小时注册数阈值').fill('10')
  await page.getByRole('button', { name: '保存风控设置' }).click()
  await expect(page.getByText('风控设置已保存；已待审账号仍需单独审核')).toBeVisible()
  expect(settings.network_daily_limit).toBe(10)
  expect(settings.daily_reward_budget_cents).toBe(5000)
})
