import { expect, test } from '@playwright/test'

test('administrator creates, shares, inspects and pauses a channel invitation', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  const token = `${encode({ alg: 'none' })}.${encode({ exp: Math.floor(Date.now() / 1000) + 3600, sub: 'admin-1' })}.e2e`
  await page.addInitScript((access) => localStorage.setItem('dt_access_token', access), token)
  const user = { id: 'admin-1', tenant_id: 'tenant-1', email: 'admin@example.test', name: 'Admin', role: 'super_admin', is_active: true, email_verified: true }
  let offer: Record<string, unknown> | null = null
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const url = new URL(route.request().url())
    const method = route.request().method()
    let data: unknown = {}
    if (url.pathname === '/api/admin/stats') data = { basic: { user_count: 1, tenant_count: 1, session_count: 0, transcript_count: 0 } }
    if (url.pathname === '/api/admin/access') data = { allowed: true, super: true, tenant_admin: false, role_id: '', role_key: 'super_admin', name: '超级管理员', permissions: ['*'], channels: [] }
    if (url.pathname === '/api/user/profile') data = { user }
    if ((url.pathname === '/api/admin/billing/plans' || url.pathname === '/api/user/billing/plans')) data = { plans: [{ code: 'pro', name: 'Pro', active: true }] }
    if (url.pathname === '/api/admin/promotions') {
      if (method === 'POST') {
        offer = { ...route.request().postDataJSON(), id: 'invite-1', enabled: true, registrations: 0, verified: 0, rewarded: 0 }
        data = offer
      } else data = { invites: offer ? [offer] : [], total: offer ? 1 : 0 }
    }
    if (url.pathname === '/api/admin/promotions/invite-1') {
      if (method === 'PATCH') { offer = { ...offer, ...route.request().postDataJSON() }; data = { ok: true } }
      else data = { registrations: [{ id: 'receipt-1', user_id: 'user-1', email: 'student@example.test', name: '学生昵称', verified: true, registered_at: '2026-09-06T01:00:00Z', rewarded_at: '2026-09-06T01:05:00Z', plan_until: '2026-10-06T01:05:00Z', discount_until: '2026-09-16T01:05:00Z', topup_rewarded_at: null, session_rewarded_at: '2026-09-06T02:00:00Z', paid_usd: 0 }], total: 1 }
    }
    if (url.pathname === '/api/admin/promotions/invite-1/funnel') {
      data = { invite: { ...offer, visits: 40, registrations: 8, verified: 6, rewarded: 6, paid: 2, revenue_usd: 30 }, sources: [{ source: 'xhs', medium: 'poster', campaign: '', content: '博主A', visits: 25 }], daily: [{ day: '2026-09-06', visits: 40, registrations: 8 }] }
    }
    if (url.pathname === '/api/admin/referrals') data = { referrers: [{ user_id: 'user-2', email: 'senior@example.test', name: '学长', code: 'ABCD2345', visits: 12, registered: 3, verified: 2, last_registered_at: '2026-09-05T01:00:00Z' }], total: 1 }
    await route.fulfill({ json: data })
  })
  await page.goto('/pro/admin')
  await page.getByRole('button', { name: '推广邀请', exact: true }).click()
  await page.getByRole('button', { name: '创建推广邀请', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('活动名称', { exact: true }).fill('开学季')
  await dialog.getByLabel('渠道', { exact: true }).fill('小红书')
  await dialog.getByLabel('用户来源标签').fill('校园, 博主A')
  await dialog.getByLabel('邀请码（留空自动生成）').fill('XHS2026A')
  await dialog.getByLabel('注册截止时间').fill('2027-09-30T23:59')
  await dialog.getByLabel('额外活动余额（USD）').fill('2.50')
  await dialog.getByLabel('赠送套餐', { exact: true }).selectOption('pro')
  await dialog.getByLabel('落地页标题').fill('开学季专属：注册即享 Pro')
  await dialog.getByLabel('实时转录折扣（%）').fill('20')
  await dialog.getByLabel('首次充值加赠（%）').fill('30')
  await dialog.getByLabel('首次转录加送余额（USD）').fill('1')
  await dialog.getByRole('button', { name: '创建邀请', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  await expect(page.getByLabel('新建邀请链接')).toHaveValue(/\/invite\?code=XHS2026A$/)
  expect(offer).toMatchObject({ channel: '小红书', tags: ['校园', '博主A'], grant_usd: 2.5, plan_code: 'pro', headline: '开学季专属：注册即享 Pro', usage_discount_percent: 20, discount_days: 30, topup_bonus_percent: 30, milestone_session_usd: 1 })
  await page.getByLabel('utm_source').fill('xiaohongshu')
  await page.getByLabel('utm_content').fill('博主A')
  await expect(page.getByLabel('新建邀请链接')).toHaveValue(/\/invite\?code=XHS2026A&utm_source=xiaohongshu&utm_content=%E5%8D%9A%E4%B8%BBA$/)
  await page.getByRole('button', { name: '复制邀请链接', exact: true }).click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toMatch(/\/invite\?code=XHS2026A&utm_source=xiaohongshu&utm_content=/)
  await expect(page.getByRole('cell', { name: /转录 -20% \/ 30 天/ })).toBeVisible()
  await page.getByRole('button', { name: '漏斗', exact: true }).click()
  await expect(page.getByRole('dialog')).toContainText('付费转化 33.3%')
  await expect(page.getByRole('dialog')).toContainText('博主A')
  await page.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('button', { name: '文案', exact: true }).click()
  await page.getByRole('dialog').getByLabel('落地页说明').fill('面向新生的限时活动')
  await page.getByRole('button', { name: '保存文案', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(offer).toMatchObject({ description: '面向新生的限时活动', usage_discount_percent: 20 })
  await expect(page.getByText('senior@example.test')).toBeVisible()
  await expect(page.getByRole('cell', { name: '12 / 3 / 2' })).toBeVisible()
  await page.getByRole('button', { name: '注册记录', exact: true }).click()
  await expect(page.getByRole('dialog')).toContainText('student@example.test')
  await expect(page.getByRole('dialog')).toContainText('已领取')
  await expect(page.getByRole('dialog')).toContainText('首次转录已发')
  await page.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('button', { name: '暂停', exact: true }).click()
  await expect(page.getByRole('cell', { name: /已暂停/ })).toBeVisible()
})
