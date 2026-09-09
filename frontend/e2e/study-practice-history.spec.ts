import { test, expect, type Page } from '@playwright/test'

const project = { id: '11111111-1111-4111-8111-111111111111', name: 'Practice course', context_mode: 'smart', max_context_tokens: 64000 }
const skill = 'Causal reasoning'
const state = { skill_key: 'causal reasoning', skill_label: skill, level: 'learner', xp_total: 0, attempts_count: 1, clean_streak: 0 }
const reveal = { format: 'open', model_answer: 'Reference evidence', explanation: 'Compare the control group.', targets: ['Evidence'] }

async function setup(page: Page) {
  const user = { id: 'practice-user', tenant_id: 'tenant', email: 'practice@example.test', name: 'Study', role: 'user', is_active: true }
  const token = `e2e.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.e2e`
  await page.addInitScript(({ user, token }) => {
    localStorage.setItem('dt_user', JSON.stringify(user))
    localStorage.setItem('dt_access_token', token)
    localStorage.setItem('dt_refresh_token', 'refresh')
  }, { user, token })
  const counts = { next: 0, grade: 0, lesson: 0, reveal: 0, history: 0, failNext: false }
  await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    const post = route.request().method() === 'POST'
    let body: unknown = {}
    if (path === '/api/system/access') body = { authentication_enabled: true, rag_enabled: true }
    else if (path === '/api/user/profile') body = { user }
    else if (path === '/api/user/balance') body = { available_usd: 20 }
    else if (path === '/api/ai/projects') body = { projects: [project] }
    else if (path.endsWith('/sources')) body = { sources: [] }
    else if (path.endsWith('/sessions')) body = { sessions: [{ id: 'session', title: 'Class' }] }
    else if (path.endsWith('/skill-map')) body = { artifact: { id: 'map' }, map: { version: 1, skills: [{ id: 'skill', label: skill, prerequisites: [] }] } }
    else if (path.endsWith('/study/state')) body = { states: [state], continue: { skill_label: skill, level: 'learner' } }
    else if (path.endsWith('/study/weeks')) body = { weeks: [], behind_weeks: [], current_week: 1, unassigned: { sources: [], sessions: [], skills: [] } }
    else if (path.endsWith('/study/costs')) body = { items: [], summary: { total_usd: 0, by_feature: {} } }
    else if (path.endsWith('/study/lesson')) { counts.lesson++; body = { lesson: null } }
    else if (path.endsWith('/study/next')) {
      counts.next++
      if (counts.failNext) { counts.failNext = false; await route.fulfill({ status: 500, body: 'Question unavailable' }); return }
      body = { scenario_id: `scenario-${counts.next}`, difficulty: 1, level: 'learner', scenario: { situation: 'An experiment', question: `Question ${counts.next}`, format: 'open' } }
    } else if (path.endsWith('/study/reveal')) { counts.reveal++; body = { reveal } }
    else if (path.endsWith('/study/attempts') && post) {
      counts.grade++
      body = { grade: 'P', feedback: 'Add a control group.', next_step: 'Identify a confounder.', bonuses: [], xp: 40, used_hint: false, leveled_up: false, state, reveal, retry_allowed: true }
    } else if (path.endsWith('/study/attempts')) {
      counts.history++
      const older = Boolean(url.searchParams.get('before'))
      body = { items: [{ id: older ? 'old' : 'recent', skill_label: skill, answer: older ? 'Earlier answer' : 'Saved answer', grade: 'C', xp: 100, feedback: 'Saved feedback', created_at: '2026-09-01T10:00:00Z', scenario: { situation: 'Stored context', question: older ? 'Older question' : 'Saved question' }, reveal }], next_cursor: older ? '' : 'recent' }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) })
  })
  await page.goto('/pro/study')
  await page.getByRole('button', { name: /Practice course/ }).click()
  return counts
}

test('previous question preserves drafts and distinct retry answers without generating or grading', async ({ page }) => {
  const counts = await setup(page)
  await page.getByRole('button', { name: '开始行动', exact: true }).click()
  const practice = page.getByRole('dialog', { name: `练习 ${skill}`, exact: true })
  await expect(practice.getByText('Question 1', { exact: true })).toBeVisible()
  await expect(practice.getByRole('button', { name: '上一题', exact: true })).toBeDisabled()
  await practice.locator('textarea').fill('My first answer')
  await practice.getByRole('button', { name: '提交', exact: true }).click()
  await expect(practice.getByText('Add a control group.', { exact: true })).toBeVisible()
  await practice.getByRole('button', { name: '再试一次', exact: true }).click()
  await practice.locator('textarea').fill('My corrected answer')
  await practice.getByRole('button', { name: '提交', exact: true }).click()
  await expect.poll(() => counts.grade).toBe(2)
  await expect(practice.getByRole('button', { name: '换一题', exact: true })).toBeEnabled()
  counts.failNext = true
  await practice.getByRole('button', { name: '换一题', exact: true }).click()
  await expect(practice.getByRole('alert')).toContainText('Question unavailable')
  await expect(practice.getByText('Add a control group.', { exact: true })).toBeVisible()
  await practice.getByRole('button', { name: '换一题', exact: true }).click()
  await expect(practice.getByText('Question 3', { exact: true })).toBeVisible()
  await practice.locator('textarea').fill('Unfinished current answer')
  await practice.getByRole('button', { name: '上一题', exact: true }).click()
  const review = page.getByRole('dialog', { name: '回顾本次练习', exact: true })
  await expect(review.getByText('My corrected answer', { exact: true })).toBeVisible()
  await review.getByRole('button', { name: '上一题', exact: true }).click()
  await expect(review.getByText('My first answer', { exact: true })).toBeVisible()
  await expect(review.getByText('Compare the control group.', { exact: true })).toBeVisible()
  await review.getByRole('button', { name: '返回当前题', exact: true }).click()
  await expect(practice.locator('textarea')).toHaveValue('Unfinished current answer')
  expect(counts.next).toBe(3)
  expect(counts.grade).toBe(2)
  expect(counts.reveal).toBe(0)
  await practice.getByRole('button', { name: '收工', exact: true }).click()
  await page.getByRole('button', { name: '回顾本次练习', exact: true }).click()
  await expect(review.getByText('My first answer', { exact: true })).toBeVisible()
  await review.getByRole('button', { name: '返回报告', exact: true }).click()
  await page.getByRole('button', { name: '再来几道', exact: true }).click()
  await expect(practice.locator('textarea')).toHaveValue('Unfinished current answer')
  expect(counts.next).toBe(3)
})

test('course history survives reload, paginates and never invokes paid practice endpoints', async ({ page }) => {
  const counts = await setup(page)
  await page.setViewportSize({ width: 390, height: 844 })
  await page.getByRole('button', { name: '做题记录', exact: true }).click()
  await page.getByRole('button', { name: /Saved question/ }).click()
  await expect(page.getByText('Saved answer', { exact: true })).toBeVisible()
  const initialReads = counts.history
  await page.getByRole('button', { name: '加载更早记录', exact: true }).click()
  await page.getByRole('button', { name: '较早记录', exact: true }).click()
  await expect(page.getByText('Earlier answer', { exact: true })).toBeVisible()
  expect(counts.history).toBe(initialReads + 1)
  await page.reload()
  await page.getByRole('button', { name: /Practice course/ }).click()
  await page.getByRole('button', { name: '做题记录', exact: true }).click()
  await page.getByRole('button', { name: /Saved question/ }).click()
  await expect(page.getByText('Saved feedback', { exact: true })).toBeVisible()
  expect(counts.next).toBe(0)
  expect(counts.grade).toBe(0)
  expect(counts.lesson).toBe(0)
  expect(counts.reveal).toBe(0)
})

test('review pauses automatic advance and keeps revealed free-practice answers', async ({ page }) => {
  const counts = await setup(page)
  await page.getByRole('button', { name: '随便练练', exact: true }).click()
  const practice = page.getByRole('dialog', { name: `练习 ${skill}`, exact: true })
  const skip = practice.getByRole('button', { name: '直接做题', exact: true })
  if (await skip.isVisible()) await skip.click()
  await expect(practice.getByText('Question 1', { exact: true })).toBeVisible()
  await practice.locator('textarea').fill('Free practice answer')
  await practice.getByRole('button', { name: '提交', exact: true }).click()
  await expect(practice.getByText('Reference evidence', { exact: true })).toBeVisible()
  await practice.getByRole('button', { name: '回顾本次练习', exact: true }).click()
  const review = page.getByRole('dialog', { name: '回顾本次练习', exact: true })
  await expect(review.getByText('Free practice answer', { exact: true })).toBeVisible()
  await page.waitForTimeout(4500)
  expect(counts.next).toBe(1)
  expect(counts.grade).toBe(0)
  expect(counts.reveal).toBe(1)
  await review.getByRole('button', { name: '返回当前题', exact: true }).click()
  await expect(practice.getByText('Question 1', { exact: true })).toBeVisible()
  await expect(practice.locator('.dt-practice__ring')).toHaveCount(0)
})
