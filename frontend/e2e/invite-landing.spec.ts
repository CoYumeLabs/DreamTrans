import { expect, test, type Page } from '@playwright/test'

interface Visit { code?: string; ref?: string; utm_source?: string; utm_content?: string }

async function installBackend(page: Page, options: { valid?: boolean } = {}) {
  const visits: Visit[] = []
  const expiresAt = new Date(Date.now() + 2 * 86_400_000 + 3_600_000).toISOString()
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/auth/invite/visit') {
      visits.push(route.request().postDataJSON() as Visit)
      await route.fulfill({ status: 204 })
      return
    }
    if (url.pathname === '/api/auth/invite') {
      if (options.valid === false) {
        await route.fulfill({ status: 400, json: { error: '邀请码无效、已暂停、已过期或名额已满' } })
        return
      }
      if (url.searchParams.get('ref')) {
        await route.fulfill({ json: { kind: 'referral', referrer_name: '老同学' } })
        return
      }
      await route.fulfill({ json: {
        kind: 'promotion', name: '开学季', headline: '开学季专属：注册即享 Pro 30 天', description: '面向新生的限时活动。',
        grant_usd: 2.5, grant_days: 15, plan_code: 'pro', plan_days: 30, usage_discount_percent: 20, discount_days: 10,
        topup_bonus_percent: 30, topup_bonus_days: 30, milestone_session_usd: 1, milestone_topup_usd: 0,
        expires_at: expiresAt, max_registrations: 100, remaining: 42,
      } })
      return
    }
    await route.fulfill({ json: {} })
  })
  return visits
}

test('channel landing page shows the offer, urgency, QR and records a UTM-tagged visit', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  const visits = await installBackend(page)
  await page.goto('/invite?code=XHS2026A&utm_source=xhs&utm_content=博主A')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('开学季专属：注册即享 Pro 30 天')
  await expect(page.getByText('面向新生的限时活动。')).toBeVisible()
  await expect(page.getByText('额外 $2.5 活动余额')).toBeVisible()
  await expect(page.getByText('实时转录费用再减 20%')).toBeVisible()
  await expect(page.getByText('首次充值加赠 30%')).toBeVisible()
  await expect(page.getByText('完成首次转录再送 $1')).toBeVisible()
  await expect(page.getByText('仅剩 42 个名额')).toBeVisible()
  await expect(page.getByLabel('距活动结束')).toContainText('2天')
  await expect(page.getByRole('link', { name: '立即注册领取' })).toHaveAttribute('href', '/pro?invite=XHS2026A')
  await expect(page.getByLabel('扫码打开同一个链接')).toBeVisible()
  await expect(page.getByRole('textbox', { name: '分享这个活动' })).toHaveValue(/\/invite\?code=XHS2026A$/)
  await page.getByRole('button', { name: '复制链接' }).click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toMatch(/\/invite\?code=XHS2026A$/)
  await expect.poll(() => visits.length).toBe(1)
  expect(visits[0]).toMatchObject({ code: 'XHS2026A', utm_source: 'xhs', utm_content: '博主A' })
  // A reload within the session does not count again.
  await page.reload()
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
  expect(visits).toHaveLength(1)
})

test('referral landing page names the referrer and leads to a ref sign-up', async ({ page }) => {
  const visits = await installBackend(page)
  await page.goto('/invite?ref=ABCD2345')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('老同学 邀请你一起使用 Yufolo')
  await expect(page.getByRole('link', { name: '免费注册' })).toHaveAttribute('href', '/pro?ref=ABCD2345')
  await expect(page.getByText('仅剩')).toHaveCount(0)
  await expect.poll(() => visits.length).toBe(1)
  expect(visits[0]).toMatchObject({ ref: 'ABCD2345' })
  // The path form is for channel codes.
  await page.goto('/invite/XHS2026A')
  await expect(page.getByRole('link', { name: '立即注册领取' })).toHaveAttribute('href', '/pro?invite=XHS2026A')
})

test('an invalid invitation still offers direct sign-up', async ({ page }) => {
  await installBackend(page, { valid: false })
  await page.goto('/invite?code=NOPE-NOPE')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('这个邀请链接已失效')
  await expect(page.getByRole('link', { name: '直接注册' })).toHaveAttribute('href', '/pro')
})
