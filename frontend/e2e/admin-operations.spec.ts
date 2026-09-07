import { expect, test } from '@playwright/test'

const accessToken = () => `${Buffer.from('{"alg":"none"}').toString('base64url')}.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.test`
const user = { id: 'console-user', tenant_id: 'console-tenant', email: 'operator@example.test', name: 'Operator', role: 'user', is_active: true, email_verified: true }

test('dynamic marketing role generates codes only after money confirmation and downloads the original batch', async ({ page }) => {
 await page.addInitScript(token => localStorage.setItem('dt_access_token', token), accessToken())
 let writes = 0, challenges = 0
 await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
  const path = new URL(route.request().url()).pathname
  let data: unknown = {}
  if (path === '/api/user/profile') data = { user }
  if (path === '/api/admin/access') data = { allowed: true, super: false, tenant_admin: false, role_id: 'marketing', role_key: 'scoped_marketing', name: '渠道运营', permissions: ['codes.read', 'codes.write'], channels: ['校园'] }
  if (path === '/api/admin/redeem-codes') {
   if (route.request().method() === 'POST') {
    if (route.request().headers()['x-admin-confirm'] !== 'true') { challenges++; await route.fulfill({ status: 428, json: { code: 'confirmation_required', error: '请确认' } }); return }
    writes++; data = { batch_id: 'batch', codes: ['AAAA1111BBBB2222CCCC3333'] }
   } else data = { codes: [], total: 0 }
  }
  await route.fulfill({ json: data })
 })
 await page.goto('/pro/admin')
 await expect(page.getByRole('heading', { name: '批量生成一次性兑换码' })).toBeVisible()
 await expect(page.getByRole('button', { name: '会员与充值', exact: true })).toHaveCount(0)
 await expect(page.getByRole('button', { name: '角色权限', exact: true })).toHaveCount(0)
 await page.getByLabel('来源渠道', { exact: true }).fill('校园')
 await page.getByLabel('数量', { exact: true }).fill('1')
 await page.getByLabel('每码面值（USD）', { exact: true }).fill('12')
 page.once('dialog', dialog => dialog.dismiss())
 await page.getByRole('button', { name: '生成兑换码', exact: true }).click()
 await expect(page.getByText('已取消，未作任何修改')).toBeVisible()
 expect(writes).toBe(0)
 page.once('dialog', dialog => dialog.accept())
 await page.getByRole('button', { name: '生成兑换码', exact: true }).click()
 await expect(page.getByLabel('生成的兑换码', { exact: true })).toHaveValue('AAAA1111BBBB2222CCCC3333')
 expect(writes).toBe(1); expect(challenges).toBe(2)
 await page.getByLabel('每码面值（USD）', { exact: true }).fill('99')
 await page.getByLabel('来源渠道', { exact: true }).fill('另一个渠道')
 const downloadPromise = page.waitForEvent('download')
 await page.getByRole('button', { name: '下载本批 CSV', exact: true }).click()
 const download = await downloadPromise
 const stream = await download.createReadStream(); if (!stream) throw new Error('CSV download missing')
 let text = ''; for await (const chunk of stream) text += chunk.toString()
 expect(text).toContain('"12","校园"'); expect(text).not.toContain('"99"')
})

test('agent portal exposes own codes and settlement request without administrative controls', async ({ page }) => {
 await page.addInitScript(token => localStorage.setItem('dt_access_token', token), accessToken())
 let requests = 0
 await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
  const path = new URL(route.request().url()).pathname
  let data: unknown = {}
  if (path === '/api/user/profile') data = { user }
  if (path === '/api/admin/access') data = { allowed: true, super: false, tenant_admin: false, role_id: 'agent-role', role_key: 'agent', name: '代理', permissions: ['agent.self'], channels: [] }
  if (path === '/api/agent/portal') data = { profile: [{ user_id: user.id, channel: '校园代理', commission_percent: 10, settle_threshold_usd: 100, code_value_usd: 10, daily_code_limit: 100, grant_days: 30, status: 'active' }], balance: { earned_usd: 20, eligible_usd: 20, reserved_usd: requests ? 20 : 0, paid_usd: 0, available_usd: requests ? 0 : 20 }, codes: [], settlements: requests ? [{ id: 'settlement-1', amount_usd: 20, method: 'credit', status: 'requested', requested_at: '2026-09-07T12:00:00Z', review_note: '', payment_reference: '' }] : [], flags: [], summary: [{ registered: 5, first_topup: 2, revenue_12_month_usd: 200, hours: 12 }], retention: [] }
  if (path === '/api/agent/settlements') { if (route.request().headers()['x-admin-confirm'] !== 'true') { await route.fulfill({ status: 428, json: { code: 'confirmation_required', error: '请确认' } }); return }; requests++; data = { id: 'settlement-1' } }
  await route.fulfill({ json: data })
 })
 await page.goto('/pro/admin')
 await expect(page.getByRole('heading', { name: '我的代理账户', level: 1 })).toBeVisible()
 await expect(page.getByRole('button', { name: '用户', exact: true })).toHaveCount(0)
 await expect(page.getByRole('button', { name: '代理与结算', exact: true })).toHaveCount(0)
 page.once('dialog', dialog => dialog.accept())
 await page.getByRole('button', { name: '申请结算全部可结金额' }).click()
 await expect(page.getByRole('cell', { name: '待审核', exact: true })).toBeVisible()
 await expect(page.getByRole('button', { name: '申请结算全部可结金额' })).toBeDisabled()
 await expect(page.getByRole('button', { name: '审核通过', exact: true })).toHaveCount(0)
 expect(requests).toBe(1)
})
