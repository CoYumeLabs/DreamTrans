import { expect, test } from '@playwright/test'

// Administrative actions must preserve the selected node and pinned release;
// registration material stays separate from the copyable installation command.
test('administrator creates an Edge, drains it and requests a pinned release', async ({ page }) => {
  const token = `${Buffer.from('{"alg":"none"}').toString('base64url')}.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.test`
  await page.addInitScript(value => localStorage.setItem('dt_access_token', value), token)
  const user = { id: 'super-user', tenant_id: 'tenant-1', email: 'super@example.test', name: 'Super', role: 'super_admin', is_active: true, email_verified: true }
  const nodes: Array<Record<string, unknown>> = []
  let installerRequests = 0
  const changes: Array<{ id: string; body: Record<string, unknown> }> = []
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const request = route.request(), path = new URL(request.url()).pathname
    let body: unknown = {}
    if (path === '/api/user/profile') body = { user }
    if (path === '/api/admin/access') body = { allowed: true, super: true, tenant_admin: false, role_id: 'super', role_key: 'super', permissions: [], channels: [] }
    if (path === '/api/admin/edges/setup') body = { control_ready: true, routing_enabled: false, installer_ready: true, missing: [], release_image: 'example/edge@sha256:' + 'b'.repeat(64), tunnel_install_available: false }
    if (path === '/api/admin/edges') {
      if (request.method() === 'POST') {
        const node = request.postDataJSON()
        nodes.push({ ...node, id: 'tokyo-node', mode: 'disabled', version: 'v1', protocol_min: 1, protocol_max: 1, active: 0, heartbeat_at: null, metrics: { queue_bytes: 24 } })
        body = { id: 'tokyo-node', registration_token: 'one-time-hidden-credential' }
      } else body = nodes
    }
    if (path === '/api/admin/edges/installer') {
      expect(new URL(request.url()).searchParams.get('node')).toBe('tokyo-node')
      expect(new URL(request.url()).searchParams.get('tunnel')).toBe('existing')
      if (++installerRequests === 1) { await route.fulfill({ status: 503, json: { error: 'installer temporarily unavailable' } }); return }
      body = { command: 'bash verified-installer.sh pinned-image' }
    }
    if (path === '/api/admin/edges/tokyo-node') {
      const change = request.postDataJSON()
      changes.push({ id: 'tokyo-node', body: change })
      if (change.mode) nodes[0].mode = change.mode
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/pro/admin')
  await page.getByRole('button', { name: '地区转录节点', exact: true }).click()
  const panel = page.getByRole('region', { name: '地区转录节点' })
  await panel.getByRole('button', { name: '添加节点' }).click()
  await panel.getByLabel('名称', { exact: true }).fill('Tokyo One')
  await panel.getByLabel('地区', { exact: true }).selectOption('ap-northeast-1')
  await panel.getByLabel('独立 HTTPS 地址', { exact: true }).fill('https://edge-tokyo-1.example.test')
  await panel.getByLabel('最大并发', { exact: true }).fill('2')
  await panel.getByRole('button', { name: '创建并生成注册凭证' }).click()
  const secret = panel.getByLabel('注册凭证', { exact: true })
  await expect(secret).toHaveValue('one-time-hidden-credential')
  await expect(secret).toHaveAttribute('type', 'password')
  await expect(panel.getByRole('alert')).toContainText('installer temporarily unavailable')
  await panel.getByRole('button', { name: '重新获取安装命令' }).click()
  await expect(panel.locator('pre').filter({ hasText: 'verified-installer' })).toHaveText('bash verified-installer.sh pinned-image')
  await expect(panel.locator('pre').filter({ hasText: 'verified-installer' })).not.toContainText('one-time-hidden-credential')
  const row = panel.getByRole('row', { name: /Tokyo One/ })
  await row.getByRole('button', { name: '排空', exact: true }).click()
  await expect(row).toContainText('正在排空')
  expect(changes[0]).toEqual({ id: 'tokyo-node', body: { mode: 'draining' } })
  const image = 'ghcr.io/example/edge@sha256:' + 'a'.repeat(64)
  await panel.getByText('高级发布设置', { exact: true }).click()
  await panel.getByLabel('地区发布镜像（repository@sha256:digest）').fill(image)
  await row.getByText('更多操作', { exact: true }).click()
  await row.getByRole('button', { name: '请求此节点升级' }).click()
  await expect.poll(() => changes.length).toBe(2)
  expect(changes[1]).toEqual({ id: 'tokyo-node', body: { image } })
  await panel.getByRole('button', { name: '清除显示' }).click()
  await expect(secret).toHaveCount(0)
})

test('unconfigured control shows setup instead of calling missing node routes', async ({ page }) => {
  const token = `${Buffer.from('{"alg":"none"}').toString('base64url')}.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.test`
  await page.addInitScript(value => localStorage.setItem('dt_access_token', value), token)
  let nodeCalls = 0
  await page.route(/^https?:\/\/[^/]+\/api\//, async route => {
    const path = new URL(route.request().url()).pathname
    let body: unknown = {}
    if (path === '/api/user/profile') body = { user: { id: 'admin', email: 'admin@example.test', role: 'super_admin', is_active: true, email_verified: true } }
    if (path === '/api/admin/access') body = { allowed: true, super: true, permissions: [], channels: [] }
    if (path === '/api/admin/edges/setup') body = { control_ready: false, installer_ready: false, routing_enabled: false, missing: ['节点管理尚未初始化'], release_image: '', tunnel_install_available: false }
    if (path === '/api/admin/edges') nodeCalls++
    await route.fulfill({ json: body })
  })
  await page.goto('/pro/admin')
  await page.getByRole('button', { name: '地区转录节点', exact: true }).click()
  await expect(page.getByRole('heading', { name: '先初始化节点管理' })).toBeVisible()
  await expect(page.getByRole('button', { name: '添加节点' })).toBeDisabled()
  await expect(page.getByText('现有转录继续由主站处理。', { exact: false })).toBeVisible()
  expect(nodeCalls).toBe(0)
})
