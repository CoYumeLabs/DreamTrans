import { expect, test, type Page, type Route } from '@playwright/test'

const now = '2026-09-01T02:00:00.000Z'
const user = {
  id: 'user-onboarding',
  tenant_id: 'tenant-1',
  email: 'newcomer@example.test',
  name: 'Newcomer',
  role: 'user',
  is_active: true,
  email_verified: true,
  created_at: now,
  updated_at: now,
}

const freePlan = {
  code: 'free', name: 'Free', is_public: true, active: true, sort: 0,
  price_usd_month: 0, price_usd_year: 0, usage_discount_percent: 0,
  storage_gb: 1, retention_days: 30, max_concurrent_sessions: 1, seats: 1,
  features: {},
}

function accessToken(): string {
  const encode = (value: object) => Buffer
    .from(JSON.stringify(value))
    .toString('base64url')
  return `${encode({ alg: 'none', typ: 'JWT' })}.${encode({
    exp: Math.floor(Date.now() / 1_000) + 3_600,
    sub: user.id,
  })}.e2e`
}

function json(route: Route, body: unknown, status = 200) {
  return route.fulfill({
    body: JSON.stringify(body),
    contentType: 'application/json',
    status,
  })
}

interface MockOptions {
  availableUsd?: number
  sessions?: Array<{ id: string; title: string }>
}

async function installBackend(page: Page, options: MockOptions = {}): Promise<string[]> {
  const unhandled: string[] = []
  const availableUsd = options.availableUsd ?? 5
  const sessions = options.sessions ?? []
  const balance = {
    user_id: user.id,
    account_id: 'account-onboarding',
    wallet_usd: 0,
    grant_usd: availableUsd,
    available_usd: availableUsd,
    lifetime_charged_usd: 0,
    plan_code: 'free',
    member_active: false,
    auto_topup_enabled: false,
  }
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const method = request.method()
    const path = url.pathname
    if (method === 'GET' && path === '/api/system/access') {
      await json(route, {
        anonymous_api_enabled: false,
        authentication_enabled: true,
        registration_enabled: false,
        rag_enabled: true,
      })
      return
    }
    if (method === 'GET' && path === '/api/system/settings') {
      await json(route, { allow_user_api_key: false })
      return
    }
    if (method === 'POST' && path === '/api/auth/login') {
      await json(route, {
        access_token: accessToken(),
        expires_in: 3_600,
        refresh_token: 'e2e-refresh',
        user,
      })
      return
    }
    if (method === 'GET' && path === '/api/admin/access') {
      await route.fulfill({ json: { allowed: false, super: false, permissions: [], channels: [] } })
      return
    }
    if (method === 'GET' && path === '/api/user/profile') {
      await json(route, { user })
      return
    }
    if (method === 'GET' && path === '/api/user/balance') {
      await json(route, balance)
      return
    }
    if (method === 'GET' && path === '/api/user/billing/account') {
      await json(route, {
        account: {
          ...balance,
          email: user.email,
          name: user.name,
          status: 'active',
          plan: freePlan,
          effective_plan: freePlan,
          discount_percent: 0,
          grants: [],
          has_payment_method: false,
          storage_bytes: 0,
          realtime_hour_usd: 0.5,
          estimated_realtime_hours: availableUsd / 0.5,
        },
        payments_enabled: true,
      })
      return
    }
    if (method === 'GET' && path === '/api/user/billing/session-costs') {
      await json(route, { session_costs: [] })
      return
    }
    if (method === 'GET' && path === '/api/sessions') {
      await json(route, {
        page: 1,
        page_size: 60,
        sessions: sessions.map((session) => ({
          id: session.id,
          user_id: user.id,
          tenant_id: user.tenant_id,
          title: session.title,
          source_language: 'en',
          target_language: 'cmn',
          duration_seconds: 60,
          status: 'completed',
          started_at: now,
          created_at: now,
          updated_at: now,
        })),
      })
      return
    }
    if (method === 'GET' && path === '/api/models/available') {
      await json(route, { models: [] })
      return
    }
    if (method === 'GET' && path === '/api/user/model-preferences') {
      await json(route, { translation_model: '', chat_model: '', summary_model: '' })
      return
    }
    unhandled.push(`${method} ${path}`)
    await json(route, {})
  })
  return unhandled
}

async function login(page: Page): Promise<void> {
  await page.goto('/pro.html')
  await page.locator('.dt-auth__card input[type="email"]').fill(user.email)
  await page.locator('.dt-auth__card input[type="password"]').fill('password123')
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.locator('.dt-sidebar')).toBeVisible()
}

test('a brand-new account is walked through audio, language and the interface tour once', async ({ page }) => {
  await installBackend(page)
  await login(page)

  const wizard = page.locator('.dt-onboarding')
  await expect(wizard).toBeVisible()
  await expect(wizard).toHaveAttribute('data-step', 'welcome')
  await expect(wizard.locator('.dt-onboarding__credit')).toContainText('$5.00')
  await expect(wizard.locator('.dt-onboarding__credit')).toContainText('10')

  await wizard.getByRole('button', { name: '开始设置' }).click()
  await expect(wizard).toHaveAttribute('data-step', 'audio')
  await expect(wizard.getByRole('radio', { name: /麦克风/ }).first()).toHaveAttribute('aria-checked', 'true')
  await wizard.getByRole('radio', { name: /电脑里的声音/ }).click()
  await expect(wizard.locator('.dt-onboarding__note')).toContainText('分享音频')

  await wizard.getByRole('button', { name: '下一步' }).click()
  await expect(wizard).toHaveAttribute('data-step', 'language')
  await wizard.getByLabel('原始语言').selectOption('ja')
  await wizard.getByLabel('翻译成').selectOption('en')
  await expect(wizard.locator('.dt-onboarding__preview')).toContainText('ニューラルネットワーク')
  await expect(wizard.locator('.dt-onboarding__preview-translation')).toContainText('neural networks')

  await wizard.getByRole('button', { name: '下一步' }).click()
  await expect(wizard).toHaveAttribute('data-step', 'ready')
  await expect(wizard.locator('.dt-onboarding__summary')).toContainText('电脑里的声音')
  await expect(wizard.locator('.dt-onboarding__summary')).toContainText('日本語 → English')

  await wizard.getByRole('button', { name: '带我看看界面' }).click()
  await expect(wizard).toBeHidden()

  const tour = page.locator('.dt-tour__card')
  await expect(tour).toBeVisible()
  await expect(tour).toHaveAttribute('data-step', 'record')
  await expect(tour).toContainText('第 1 / 6 步')
  await expect(page.locator('.dt-tour__spot')).toBeVisible()
  const spot = await page.locator('.dt-tour__spot').boundingBox()
  const record = await page.locator('[data-tour="record"]').boundingBox()
  expect(spot && record && Math.abs(spot.x + spot.width / 2 - (record.x + record.width / 2)) < 2).toBe(true)

  await tour.getByRole('button', { name: '下一步' }).click()
  await expect(tour).toHaveAttribute('data-step', 'mode-switch')
  await page.keyboard.press('ArrowRight')
  await expect(tour).toHaveAttribute('data-step', 'assistant')
  await tour.getByRole('button', { name: '上一步' }).click()
  await expect(tour).toHaveAttribute('data-step', 'mode-switch')
  for (const expected of ['assistant', 'history', 'settings', 'account']) {
    await tour.getByRole('button', { name: '下一步' }).click()
    await expect(tour).toHaveAttribute('data-step', expected)
  }
  await tour.getByRole('button', { name: '完成' }).click()
  await expect(tour).toBeHidden()

  // The empty state now reflects what the wizard configured.
  const setup = page.locator('.dt-feed-empty__setup')
  await expect(setup).toContainText('电脑声音')
  await expect(setup).toContainText('日本語 → English')
  const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('dt_unified_settings_v1') ?? '{}'))
  expect(stored).toMatchObject({ audioSource: 'system', sourceLanguage: 'ja', targetLanguage: 'en', translationEnabled: true })

  // A reload must not greet the same account twice.
  await page.reload()
  await expect(page.locator('.dt-sidebar')).toBeVisible()
  await expect(page.locator('.dt-feed-empty')).toBeVisible()
  await page.waitForTimeout(500)
  await expect(page.locator('.dt-onboarding')).toHaveCount(0)
  await expect(page.locator('.dt-tour')).toHaveCount(0)

  // ...but it stays reachable from the settings panel and the empty state.
  await page.getByRole('button', { name: '设置', exact: true }).click()
  await page.getByRole('button', { name: '重新查看新手引导' }).click()
  await expect(page.locator('.dt-onboarding')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('.dt-onboarding')).toBeHidden()
  await page.locator('.dt-feed-empty__links').getByRole('button', { name: '新手引导' }).click()
  await expect(page.locator('.dt-onboarding')).toBeVisible()
})

test('an account with existing sessions is never interrupted by the wizard', async ({ page }) => {
  await installBackend(page, { sessions: [{ id: 'session-1', title: 'Week 1 lecture' }] })
  await login(page)
  await expect(page.locator('.dt-history-item__main', { hasText: 'Week 1 lecture' })).toBeVisible()
  await page.waitForTimeout(500)
  await expect(page.locator('.dt-onboarding')).toHaveCount(0)
  const record = await page.evaluate(() => localStorage.getItem('dt_onboarding_v1_user%3Auser-onboarding'))
  expect(record).not.toBeNull()
  expect(JSON.parse(record ?? '{}')).toHaveProperty('wizardCompletedAt')
})

test('an empty wallet is called out with a top-up shortcut before the first recording', async ({ page }) => {
  await installBackend(page, { availableUsd: 0 })
  await login(page)
  const wizard = page.locator('.dt-onboarding')
  await expect(wizard.locator('.dt-onboarding__credit')).toContainText('还没有余额')
  await wizard.getByRole('button', { name: '去充值' }).click()
  await expect(wizard).toBeHidden()
  await expect(page.locator('.dt-sheet')).toContainText('Newcomer')
})
