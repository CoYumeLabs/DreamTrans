import { expect, test, type Page, type Route } from '@playwright/test'

const now = '2026-09-04T02:00:00.000Z'
const user = {
  id: 'user-signup',
  tenant_id: 'tenant-1',
  email: 'fresh@example.test',
  name: 'Fresh',
  role: 'user',
  is_active: true,
  email_verified: true,
  created_at: now,
  updated_at: now,
}

function accessToken(): string {
  const encode = (value: object) => Buffer.from(JSON.stringify(value)).toString('base64url')
  return `${encode({ alg: 'none', typ: 'JWT' })}.${encode({
    exp: Math.floor(Date.now() / 1_000) + 3_600,
    sub: user.id,
  })}.e2e`
}

function json(route: Route, body: unknown, status = 200) {
  return route.fulfill({ body: JSON.stringify(body), contentType: 'application/json', status })
}

interface Backend {
  calls: Array<{ method: string; path: string; body: Record<string, unknown> | undefined }>
  verified: boolean
  validToken: string
}

async function installBackend(page: Page): Promise<Backend> {
  const backend: Backend = { calls: [], verified: false, validToken: 'tok-valid-123' }
  const session = () => ({
    access_token: accessToken(), expires_in: 3_600, refresh_token: 'e2e-refresh', user,
  })
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const method = request.method()
    let body: Record<string, unknown> | undefined
    try { body = request.postDataJSON() as Record<string, unknown> } catch { body = undefined }
    backend.calls.push({ method, path, body })

    if (path === '/api/system/access') {
      await json(route, {
        anonymous_api_enabled: false,
        authentication_enabled: true,
        registration_enabled: true,
        email_verification_required: true,
        rag_enabled: false,
      })
      return
    }
    if (path === '/api/system/settings') { await json(route, { allow_user_api_key: false }); return }
    if (path === '/api/auth/invite') {
      if (new URL(request.url()).searchParams.get('ref')) {
        await json(route, { kind: 'referral', referrer_name: '老同学' })
        return
      }
      await json(route, { name: '开学季', grant_usd: 2.5, grant_days: 15, plan_code: 'pro', plan_days: 30, usage_discount_percent: 20, discount_days: 10 })
      return
    }
    if (path === '/api/auth/register') {
      await json(route, { verification_required: true, email: body?.email, email_sent: true }, 202)
      return
    }
    if (path === '/api/auth/resend-verification') { await json(route, { accepted: true }); return }
    if (path === '/api/auth/login') {
      if (!backend.verified) {
        await json(route, { error: 'email address not verified', code: 'email_not_verified' }, 403)
        return
      }
      await json(route, session())
      return
    }
    if (path === '/api/auth/verify-email') {
      if (body?.token !== backend.validToken) {
        await json(route, { error: 'verification link is invalid or has expired', code: 'verification_token_invalid' }, 400)
        return
      }
      backend.verified = true
      await json(route, session())
      return
    }
    if (path === '/api/user/profile') { await json(route, { user }); return }
    if (path === '/api/user/balance') {
      await json(route, {
        user_id: user.id, account_id: 'acct', wallet_usd: 0, grant_usd: 3, available_usd: 3,
        lifetime_charged_usd: 0, plan_code: 'free', member_active: false, auto_topup_enabled: false,
      })
      return
    }
    if (path === '/api/user/billing/account') {
      await json(route, { account: null, payments_enabled: false })
      return
    }
    if (path === '/api/sessions') { await json(route, { page: 1, page_size: 60, sessions: [] }); return }
    await json(route, {})
  })
  return backend
}

test('sign-up parks the user on a check-your-inbox screen until the link is clicked', async ({ page }) => {
  const backend = await installBackend(page)
  await page.goto('/pro.html')
  await page.getByRole('button', { name: '没有账户？创建一个' }).click()
  await expect(page.locator('.dt-auth__hint')).toContainText('验证邮件')
  await page.locator('.dt-auth__card input[autocomplete="nickname"]').fill('Fresh')
  await page.locator('.dt-auth__card input[type="email"]').fill(user.email)
  await page.locator('.dt-auth__card input[type="password"]').fill('correct horse battery')
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.getByRole('alert')).toContainText('请先同意')
  expect(backend.calls.some((call) => call.path === '/api/auth/register')).toBe(false)

  await page.getByRole('checkbox').check()
  await page.locator('.dt-auth__card .dt-button--primary').click()

  const pending = page.getByTestId('verification-pending')
  await expect(pending).toBeVisible()
  await expect(pending).toContainText(user.email)
  await expect(page.locator('.dt-sidebar')).toHaveCount(0)
  // No tokens were stored: the browser is not logged in.
  expect(await page.evaluate(() => localStorage.getItem('dt_access_token'))).toBeNull()

  // Resend is on cooldown right after sign-up, since a mail is already on its way.
  const resend = pending.getByRole('button', { name: /重新发送/ })
  await expect(resend).toBeDisabled()
  await expect(resend).toContainText('秒后可用')

  // Going back and logging in before verifying bounces straight back here,
  // this time with resend available immediately.
  await pending.getByRole('button', { name: '返回登录' }).click()
  await page.locator('.dt-auth__card input[type="email"]').fill(user.email)
  await page.locator('.dt-auth__card input[type="password"]').fill('correct horse battery')
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.getByTestId('verification-pending')).toBeVisible()
  const resendNow = page.getByTestId('verification-pending').getByRole('button', { name: '重新发送验证邮件' })
  await expect(resendNow).toBeEnabled()
  await resendNow.click()
  await expect(page.getByTestId('verification-pending')).toContainText('已重新发送')
  expect(backend.calls.some((call) => call.path === '/api/auth/resend-verification' && call.body?.email === user.email)).toBe(true)
})

test('an invalid verification link explains itself and a valid one signs the user in', async ({ page }) => {
  const backend = await installBackend(page)
  await page.goto('/pro.html?verify=tok-expired')
  await expect(page.locator('.dt-form-error')).toContainText('验证链接已失效')
  await expect(page).toHaveURL(/\/pro\.html$/)

  await page.goto(`/pro.html?verify=${backend.validToken}`)
  await expect(page.locator('.dt-sidebar')).toBeVisible()
  await expect(page).toHaveURL(/\/pro\.html$/)
  await expect(page.locator('.dt-account-chip')).toContainText('Fresh')
  expect(backend.calls.filter((call) => call.path === '/api/auth/verify-email')).toHaveLength(2)
})


test('invite links open sign-up and submit the code while the field remains collapsed', async ({ page }) => {
  const backend = await installBackend(page)
  await page.goto('/pro?invite=XHS2026A')
  await expect(page.getByRole('heading', { name: '创建账户' })).toBeVisible()
  await expect(page.getByLabel('昵称', { exact: true })).toBeVisible()
  await expect(page.locator('.dt-auth__invite')).not.toHaveAttribute('open')
  await expect(page.getByText('已填写邀请码（可修改）')).toBeVisible()
  await expect(page.locator('.dt-auth__offer')).toContainText('额外 $2.5')
  await expect(page.locator('.dt-auth__offer')).toContainText('再减 20%')
  await page.getByLabel('昵称', { exact: true }).fill('小梦')
  await page.locator('input[type="email"]').fill(user.email)
  await page.locator('input[type="password"]').fill('correct horse battery')
  await page.getByRole('checkbox').check()
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.getByTestId('verification-pending')).toBeVisible()
  const request = backend.calls.find((call) => call.path === '/api/auth/register')
  expect(request?.body?.invite_code).toBe('XHS2026A')
  expect(request?.body?.name).toBe('小梦')
  expect(request?.body?.browser).toMatchObject({ version: 1, language: 'zh-CN', webdriver: true })
})

test('manual invite entry is collapsed and editable, with the edited code submitted', async ({ page }) => {
  const backend = await installBackend(page)
  await page.goto('/pro')
  await page.getByRole('button', { name: '没有账户？创建一个' }).click()
  await expect(page.locator('.dt-auth__invite')).not.toHaveAttribute('open')
  await page.getByText('有邀请码？', { exact: true }).click()
  await page.getByLabel('邀请码', { exact: false }).fill('CAMPUS2026')
  await expect(page.locator('.dt-auth__offer')).toContainText('开学季')
  await page.getByText('已填写邀请码（可修改）').click()
  await page.getByLabel('昵称', { exact: true }).fill('Campus')
  await page.locator('input[type="email"]').fill(user.email)
  await page.locator('input[type="password"]').fill('correct horse battery')
  await page.getByRole('checkbox').check()
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.getByTestId('verification-pending')).toBeVisible()
  expect(backend.calls.find((call) => call.path === '/api/auth/register')?.body?.invite_code).toBe('CAMPUS2026')
})


test('signup prepares the browser cookie and explains held rewards', async ({ page }) => {
  const backend = await installBackend(page)
  await page.route('**/api/auth/register', async (route) => {
    await json(route, { verification_required: true, email: user.email, email_sent: true, reward_review_required: true }, 202)
  })
  await page.goto('/pro')
  await page.getByRole('button', { name: '没有账户？创建一个' }).click()
  await page.getByLabel('昵称', { exact: true }).fill('同学')
  await page.locator('input[type="email"]').fill(user.email)
  await page.locator('input[type="password"]').fill('correct horse battery')
  await page.getByRole('checkbox').check()
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.getByTestId('verification-pending')).toContainText('注册赠送权益正在审核')
  await expect(page.getByTestId('verification-pending')).toContainText('验证邮箱后仍可登录')
  expect(backend.calls.some((call) => call.path === '/api/auth/signup-context')).toBe(true)
})

test('referral links open sign-up, show the referrer and submit the referral code', async ({ page }) => {
  const backend = await installBackend(page)
  await page.goto('/pro?ref=ABCD2345')
  await expect(page.getByRole('heading', { name: '创建账户' })).toBeVisible()
  await expect(page.locator('.dt-auth__offer')).toContainText('老同学 邀请你加入')
  await expect(page.getByText('有邀请码？', { exact: true })).toBeVisible()
  await page.getByLabel('昵称', { exact: true }).fill('新同学')
  await page.locator('input[type="email"]').fill(user.email)
  await page.locator('input[type="password"]').fill('correct horse battery')
  await page.getByRole('checkbox').check()
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.getByTestId('verification-pending')).toBeVisible()
  const request = backend.calls.find((call) => call.path === '/api/auth/register')
  expect(request?.body?.referral_code).toBe('ABCD2345')
  expect(request?.body?.invite_code).toBeUndefined()
})
