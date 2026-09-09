import { expect, test, type Page, type Route } from '@playwright/test'


const e2eFreePlan = {
  code: 'free', name: 'Free', is_public: true, active: true, sort: 0,
  price_usd_month: 0, price_usd_year: 0, usage_discount_percent: 0,
  storage_gb: 1, retention_days: 30, max_concurrent_sessions: 1, seats: 1,
  features: {},
}

function e2eAccountBalance(userId: string) {
  return {
    user_id: userId,
    account_id: 'account-e2e',
    wallet_usd: 100,
    grant_usd: 0,
    available_usd: 100,
    lifetime_charged_usd: 1,
    plan_code: 'free',
    member_active: false,
    auto_topup_enabled: false,
  }
}

interface APIRecord {
  body?: Record<string, unknown>
  method: string
  path: string
  postData?: string
}

interface MockSession {
  id: string
  sourceLanguage: string
  title: string
}

interface MockProject {
  context_mode: 'smart'
  description: string
  id: string
  max_context_tokens: number
  name: string
}

interface MockSource {
  chunk_count: number
  id: string
  index_status: 'unindexed'
  name: string
  ocr_languages: string[]
  source_type: 'file'
  status: 'ready'
}

interface MockArtifact {
  artifact_type: 'summary' | 'notes' | 'action_items'
  content: string
  context_tokens: number
  created_at: string
  id: string
  model: string
  title: string
}

const now = '2026-07-31T02:00:00.000Z'
const user = {
  id: 'user-1',
  tenant_id: 'tenant-1',
  email: 'e2e@example.test',
  name: 'E2E User',
  role: 'user',
  is_active: true,
  email_verified: true,
  created_at: now,
  updated_at: now,
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

function jsonBody(route: Route): Record<string, unknown> | undefined {
  try {
    return route.request().postDataJSON() as Record<string, unknown>
  } catch {
    return undefined
  }
}

class MockAIBackend {
  readonly records: APIRecord[] = []
  readonly unhandled: string[] = []
  readonly sessions: MockSession[]
  readonly unavailableSessionIds = new Set<string>()
  readonly responseDelayMs = new Map<string, number>()
  readonly projectsBySession = new Map<string, {
    linkedProjectId: string
    projects: MockProject[]
  }>()
  readonly artifactsBySession = new Map<string, MockArtifact[]>()
  readonly noRetrievalSessions = new Set<string>()
  artifacts: MockArtifact[] = []
  emptyProjectFallsThroughToSession = false
  indexJobCanFinish = false
  indexJobReturnsError = false
  indexRetryReturnsConflict = false
  linkedProjectId = ''
  readonly linkedSessionIds = new Set<string>()
  project: MockProject | undefined
  skillMapDoc: Record<string, unknown> | null = null
  skillMapJob: Record<string, unknown> | null = null
  skillMapPolls = 0
  studyServes = 0
  studyStates: Array<Record<string, unknown>> = []
  sessionCosts: Array<Record<string, unknown>> = []
  projectIndexReady = false
  sessionIndexReady = false
  sources: MockSource[] = []

  constructor(sessions: MockSession[]) {
    this.sessions = sessions
  }

  private async delay(key: string): Promise<void> {
    const milliseconds = this.responseDelayMs.get(key) ?? 0
    if (milliseconds <= 0) return
    await new Promise((resolve) => globalThis.setTimeout(resolve, milliseconds))
  }

  async install(page: Page): Promise<void> {
    await page.route(/^https?:\/\/[^/]+\/api\//, async (route) => {
      const request = route.request()
      const url = new URL(request.url())
      const method = request.method()
      const body = jsonBody(route)
      const record: APIRecord = {
        method,
        path: url.pathname,
        ...(body ? { body } : {}),
        ...(request.postData() ? { postData: request.postData() ?? undefined } : {}),
      }
      this.records.push(record)

      if (method === 'GET' && url.pathname === '/api/system/access') {
        await json(route, {
          anonymous_api_enabled: false,
          authentication_enabled: true,
          registration_enabled: false,
          rag_enabled: true,
        })
        return
      }
      if (method === 'GET' && url.pathname === '/api/system/settings') {
        await json(route, { allow_user_api_key: false })
        return
      }
      if (method === 'GET' && url.pathname === '/api/announcements') {
        await json(route, { announcements: [] })
        return
      }
      if (method === 'POST' && url.pathname === '/api/auth/login') {
        await json(route, {
          access_token: accessToken(),
          expires_in: 3_600,
          refresh_token: 'e2e-refresh',
          user,
        })
        return
      }
      if (method === 'GET' && url.pathname === '/api/admin/access') {
        await json(route, { allowed: false, super: false, permissions: [], channels: [] })
        return
      }
      if (method === 'GET' && url.pathname === '/api/user/profile') {
        await json(route, { user })
        return
      }
      if (method === 'GET' && url.pathname === '/api/user/balance') {
        await json(route, e2eAccountBalance(user.id))
        return
      }
      if (method === 'GET' && url.pathname === '/api/user/referral') {
        await json(route, { code: 'ABCD2345', path: '/invite?ref=ABCD2345', visits: 0, registered: 0, verified: 0 })
        return
      }
      if (method === 'GET' && url.pathname === '/api/user/billing/account') {
        await json(route, {
          account: {
            ...e2eAccountBalance(user.id),
            email: user.email,
            name: user.name,
            status: 'active',
            plan: e2eFreePlan,
            effective_plan: e2eFreePlan,
            discount_percent: 0,
            grants: [],
            has_payment_method: false,
            storage_bytes: 0,
            realtime_hour_usd: 0.645,
            estimated_realtime_hours: 155,
          },
          payments_enabled: false,
        })
        return
      }
      if (method === 'GET' && url.pathname === '/api/user/billing/plans') {
        await json(route, {
          plans: [e2eFreePlan],
          topup_tiers: [],
          hourly: [{
            plan_code: 'free', plan_name: 'Free', discount_percent: 0,
            realtime_hour_usd: 0.645, realtime_upstream_usd: 0.43, realtime_gross_margin_percent: 33.3,
          }],
          payments_enabled: false,
        })
        return
      }
      if (method === 'GET' && (url.pathname === '/api/user/billing/usage' || url.pathname === '/api/user/billing/ledger')) {
        await json(route, { usage: [], ledger: [], payments: [] })
        return
      }
      if (method === 'GET' && url.pathname === '/api/user/billing/session-costs') {
        await json(route, { session_costs: this.sessionCosts })
        return
      }
      if (method === 'GET' && url.pathname === '/api/sessions') {
        await json(route, {
          page: 1,
          page_size: 60,
          sessions: this.sessions.map((session) => ({
            id: session.id,
            user_id: user.id,
            tenant_id: user.tenant_id,
            title: session.title,
            source_language: session.sourceLanguage,
            target_language: 'cmn',
            duration_seconds: 60,
            status: 'completed',
            ...(this.linkedSessionIds.has(session.id)
              ? { project_id: this.project?.id ?? 'project-1' }
              : {}),
            started_at: now,
            created_at: now,
            updated_at: now,
          })),
        })
        return
      }
      const sessionMatch = url.pathname.match(/^\/api\/sessions\/([^/]+)$/)
      if (sessionMatch && method === 'GET') {
        if (this.unavailableSessionIds.has(sessionMatch[1] ?? '')) {
          await json(route, { error: 'temporary session outage' }, 503)
          return
        }
        const session = this.sessions.find(({ id }) => id === sessionMatch[1])
        if (!session) {
          await json(route, { error: 'session not found' }, 404)
          return
        }
        await json(route, {
          id: session.id,
          user_id: user.id,
          tenant_id: user.tenant_id,
          title: session.title,
          source_language: session.sourceLanguage,
          target_language: 'cmn',
          duration_seconds: 60,
          status: 'completed',
          started_at: now,
          created_at: now,
          updated_at: now,
          transcripts: [],
        })
        return
      }
      if (sessionMatch && method === 'PUT') {
        const session = this.sessions.find(({ id }) => id === sessionMatch[1])
        await json(route, {
          ...body,
          id: session?.id,
          title: session?.title,
          source_language: session?.sourceLanguage,
          target_language: 'cmn',
          duration_seconds: 60,
          status: 'completed',
          started_at: now,
          created_at: now,
          updated_at: now,
        })
        return
      }
      const transcriptsMatch = url.pathname.match(
        /^\/api\/sessions\/([^/]+)\/transcripts$/,
      )
      if (transcriptsMatch && method === 'GET') {
        await json(route, {
          has_more: false,
          next_cursor: null,
          transcripts: [{
            id: `transcript-${transcriptsMatch[1]}`,
            session_id: transcriptsMatch[1],
            client_segment_id: `segment-${transcriptsMatch[1]}`,
            speaker: 'Speaker 1',
            text: 'The launch decision is Friday and Alice owns the release notes.',
            start_time: 0,
            end_time: 4,
            status: 'confirmed',
            is_partial: false,
            created_at: now,
            updated_at: now,
          }],
        })
        return
      }

      if (method === 'GET' && url.pathname === '/api/ai/projects') {
        const requestedSessionId = url.searchParams.get('session_id') ?? ''
        await this.delay(`projects:${requestedSessionId}`)
        const scoped = this.projectsBySession.get(requestedSessionId)
        await json(route, {
          linked_project_id: scoped
            ? scoped.linkedProjectId || null
            : this.linkedProjectId || null,
          projects: scoped?.projects ?? (this.project ? [this.project] : []),
        })
        return
      }
      if (method === 'POST' && url.pathname === '/api/ai/projects') {
        this.project = {
          context_mode: 'smart',
          description: '',
          id: 'project-1',
          max_context_tokens: 64_000,
          name: String(body?.name ?? 'E2E project'),
        }
        await json(route, { project: this.project }, 201)
        return
      }
      if (method === 'DELETE' && url.pathname === '/api/ai/projects/project-1') {
        this.project = undefined
        this.linkedProjectId = ''
        this.sources = []
        await route.fulfill({ status: 204 })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/study/state'
      ) {
        await json(route, {
          states: this.studyStates,
          continue: this.studyStates.length > 0
            ? { skill_label: '识别相关关系', level: 'supervised' }
            : { skill_label: '识别相关关系', level: 'learner' },
        })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/study/weeks'
      ) {
        await json(route, {
          week_start: '2026-03-02',
          week_start_inferred: true,
          current_week: 2,
          weeks: [
            { week: 1, label: '第 1 周', start: '2026-03-02', end: '2026-03-08', status: 'behind',
              sessions: [{ id: 'session-1', title: 'AI E2E session', started_at: '2026-03-03T10:00:00Z' }],
              sources: [], skills: [{ id: 's1', label: '识别相关关系', level: this.studyStates[0]?.level ?? 'unlit', xp_total: 0 }] },
            { week: 2, label: '第 2 周', start: '2026-03-09', end: '2026-03-15', status: 'current',
              sessions: [], sources: [{ id: 'source-1', name: 'launch-notes.txt', source_type: 'file' }], skills: [] },
          ],
          behind_weeks: this.studyStates.length > 0 ? [] : [1],
          focus: this.studyStates.length > 0
            ? { week: 2, reason: '这是本周的内容，跟上节奏就好。' }
            : { week: 1, skill_label: '识别相关关系', reason: '第 1 周的内容还没练熟，先从这里补，补上就能跟上本周。' },
          unassigned: { sessions: [], sources: [], skills: [] },
        })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/study/lesson'
      ) {
        await json(route, {
          lesson: {
            id: 'lesson-1', project_id: 'project-1',
            skill_key: '识别相关关系', skill_label: '识别相关关系',
            content: {
              rule: '看到 correlation，先想第三变量',
              concepts: [{ term: 'confounding variable', gloss: '同时影响两边的变量', quote: 'a third variable' }],
              misconceptions: [{ label: 'correlation_as_causation', how_to_tell: '一看到一起变就说因为' }],
              example: {
                situation: '喝咖啡多的学生 GPA 高',
                question: '能说咖啡提升成绩吗？',
                answer: 'No. The data are correlational.',
                walkthrough: '先判数据类型',
              },
            },
            created_at: now,
          },
          generated: true,
          cost_usd: 0.003,
        })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/study/reveal'
      ) {
        await json(route, {
          scenario_id: body?.scenario_id,
          reveal: {
            format: 'open',
            model_answer: 'No. The data are correlational.',
            explanation: '看到相关先找第三变量。',
            targets: ['correlation ≠ causation'],
          },
          cost_usd: 0,
        })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/study/next'
      ) {
        this.studyServes += 1
        if (this.studyServes === 1) {
          await json(route, {
            scenario_id: 'scenario-1',
            difficulty: 1,
            level: 'learner',
            generated: true,
            scaffold: {
              offer_zh: true, show_zh: false, offer_hint: true,
              offer_glossary: true, offer_starters: true,
            },
            cost_usd: 0.008,
            scenario: {
              situation: 'A study finds students who drink more coffee have higher GPAs.',
              question: 'Can the researchers conclude coffee improves grades?',
              question_zh: '研究者能得出咖啡提升成绩的结论吗？',
              hint: '想想有没有第三个变量',
              format: 'open',
              lang: '中文框架 · EN 术语',
              glossary: [{ term: 'confound', gloss: '混淆变量' }],
              starters: ['The study cannot establish causation because'],
            },
          })
          return
        }
        await json(route, {
          scenario_id: 'scenario-2',
          difficulty: 1,
          level: 'supervised',
          generated: false,
          scaffold: {
            offer_zh: true, show_zh: false, offer_hint: false,
            offer_glossary: true, offer_starters: false,
          },
          coach_line: '下一题把提示拿掉。',
          scenario: {
            situation: 'Ice cream sales and drowning deaths rise together every summer.',
            question: 'Which variable most plausibly explains the link?',
            format: 'single',
            options: ['Ice cream', 'Temperature', 'Swimming lessons'],
            glossary: [{ term: 'spurious', gloss: '伪相关的' }],
          },
        })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/study/attempts'
        && body?.scenario_id === 'scenario-2'
      ) {
        await json(route, {
          grade: 'C',
          feedback: '选对了，理由还可以更具体',
          next_step: '说清楚温度如何同时推动两者',
          bonuses: [],
          xp: 100,
          combo: 2,
          events: [],
          used_hint: false,
          used_zh: false,
          leveled_up: false,
          format: 'single',
          answer_correct: true,
          language_tip: '你写的 "make both go up" → 学术写法 "drives both"',
          state: this.studyStates[0],
          difficulty: 1,
          difficulty_multiplier: 1,
          retry_allowed: false,
          reveal: {
            format: 'single',
            answer_indexes: [1],
            option_notes: ['冰淇淋不会让人淹死', '天气热同时推高两者', '游泳课和冰淇淋无关'],
            model_answer: 'Temperature drives both ice cream sales and swimming.',
            explanation: '两个变量一起变，先找同时推动两者的第三个变量。',
            targets: ['confounding variable'],
          },
        })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/study/attempts'
      ) {
        const state = {
          skill_key: '识别相关关系',
          skill_label: '识别相关关系',
          level: 'supervised',
          xp_total: 180,
          attempts_count: 1,
          clean_streak: 0,
          last_grade: 'D',
        }
        this.studyStates = [state]
        await json(route, {
          grade: 'D',
          feedback: '抓到了混淆变量',
          next_step: '再主动改写结论就能到 HD',
          bonuses: [],
          xp: 180,
          combo: 1,
          events: [],
          used_hint: Boolean(body?.used_hint),
          used_zh: Boolean(body?.used_zh),
          leveled_up: true,
          cost_usd: 0.0021,
          state,
          difficulty: 1,
          difficulty_multiplier: 1,
          retry_allowed: false,
          reveal: {
            format: 'open',
            model_answer: 'No. The data are correlational, so a confound such as study habits could drive both.',
            explanation: '看到相关数据得出因果结论，先找第三变量。',
            targets: ['correlation ≠ causation', 'confounding variable'],
          },
        })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/study/costs'
      ) {
        await json(route, {
          billing_enabled: true,
          summary: {
            project_id: 'project-1',
            total_usd: 0.0342,
            by_feature: { skill_map: 0.02, study_bank: 0.01, study_grade: 0.0042 },
            operations: 3,
          },
          items: [{
            id: 'usage-1', session_id: null, action: 'summarize', model: 'gpt-test',
            quantity: 1, input_tokens: 1200, cached_input_tokens: 0, cache_write_tokens: 0,
            output_tokens: 300, cost_usd: 0.02, grant_usd: 0.02, wallet_usd: 0,
            attribution: 'platform', feature: 'skill_map', project_id: 'project-1',
            settled: true, refunded: false, created_at: now,
          }],
        })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/skill-map'
      ) {
        if (this.skillMapJob && this.skillMapJob.status !== 'ready') {
          this.skillMapPolls += 1
          if (this.skillMapPolls >= 1) {
            this.skillMapDoc = {
              version: 1,
              generated_at: now,
              session_count: 1,
              skills: [
                {
                  id: 's1',
                  label: '识别相关关系',
                  summary: '看懂两个变量一起变化意味着什么。',
                  outcome: '能识别研究里报告的相关关系',
                  evidence: [{
                    session_id: 'session-1',
                    session_title: 'AI E2E session',
                    quote: 'correlation does not imply causation',
                  }],
                },
                {
                  id: 's2',
                  label: '区分相关与因果',
                  outcome: '能判断因果结论是否越界',
                  prerequisites: ['s1'],
                  new: true,
                },
              ],
            }
            this.skillMapJob = {
              id: 'skill-map-job-1',
              project_id: 'project-1',
              status: 'ready',
              chunk_count: 1,
              processed_chunks: 1,
              cost_usd: 0.02,
            }
          }
        }
        await json(route, {
          artifact: this.skillMapDoc
            ? { id: 'skill-map-artifact', artifact_type: 'skill_map' }
            : null,
          map: this.skillMapDoc,
          job: this.skillMapJob,
        })
        return
      }
      if (
        method === 'DELETE'
        && url.pathname === '/api/ai/projects/project-1/skill-map'
      ) {
        if (this.skillMapJob && this.skillMapJob.status !== 'ready') {
          this.skillMapJob = { ...this.skillMapJob, status: 'cancelled' }
        }
        await json(route, {
          artifact: this.skillMapDoc
            ? { id: 'skill-map-artifact', artifact_type: 'skill_map' }
            : null,
          map: this.skillMapDoc,
          job: this.skillMapJob,
        })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/skill-map'
      ) {
        this.skillMapPolls = 0
        this.skillMapJob = {
          id: 'skill-map-job-1',
          project_id: 'project-1',
          status: 'processing',
          chunk_count: 1,
          processed_chunks: 0,
        }
        await json(route, {
          artifact: this.skillMapDoc
            ? { id: 'skill-map-artifact', artifact_type: 'skill_map' }
            : null,
          map: this.skillMapDoc,
          job: this.skillMapJob,
          replayed: false,
        })
        return
      }
      // 课表: the study page reads class times for the calendar and the
      // course; this workflow sets none.
      if (
        method === 'GET'
        && (url.pathname === '/api/ai/timetable'
          || url.pathname === '/api/ai/projects/project-1/timetable')
      ) {
        await json(route, { slots: [] })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/sessions'
      ) {
        await json(route, {
          sessions: this.sessions
            .filter(({ id }) => this.linkedSessionIds.has(id))
            .map((session) => ({
              id: session.id,
              title: session.title,
              source_language: session.sourceLanguage,
              target_language: 'cmn',
              duration_seconds: 60,
              status: 'completed',
              project_id: 'project-1',
              started_at: now,
              created_at: now,
              updated_at: now,
            })),
        })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/sessions'
      ) {
        this.linkedProjectId = 'project-1'
        if (typeof body?.session_id === 'string') {
          this.linkedSessionIds.add(body.session_id)
        }
        await route.fulfill({ status: 204 })
        return
      }
      if (
        method === 'DELETE'
        && /^\/api\/ai\/projects\/project-1\/sessions\//.test(url.pathname)
      ) {
        this.linkedProjectId = ''
        this.linkedSessionIds.delete(url.pathname.split('/').pop() ?? '')
        await route.fulfill({ status: 204 })
        return
      }
      if (
        method === 'GET'
        && url.pathname === '/api/ai/projects/project-1/sources'
      ) {
        await json(route, { sources: this.sources })
        return
      }
      if (
        method === 'POST'
        && url.pathname === '/api/ai/projects/project-1/sources'
      ) {
        const postData = request.postData() ?? ''
        const contentType = await request.headerValue('content-type')
        if (!contentType?.startsWith('multipart/form-data')) {
          await json(route, { error: 'expected multipart upload' }, 400)
          return
        }
        const source: MockSource = {
          chunk_count: 2,
          id: 'source-1',
          index_status: 'unindexed',
          name: postData.includes('launch-notes.txt')
            ? 'launch-notes.txt'
            : 'uploaded knowledge',
          ocr_languages: postData.includes('chi_sim') ? ['eng', 'chi_sim'] : ['eng'],
          source_type: 'file',
          status: 'ready',
        }
        this.sources = [source]
        await json(route, { source }, 201)
        return
      }
      if (
        method === 'DELETE'
        && url.pathname === '/api/ai/projects/project-1/sources/source-1'
      ) {
        this.sources = []
        await route.fulfill({ status: 204 })
        return
      }

      if (method === 'POST' && url.pathname === '/api/ai/context/preview') {
        const sessionId = String(body?.session_id ?? '')
        await this.delay(`context:${sessionId}`)
        const noRetrieval = this.noRetrievalSessions.has(sessionId)
        const emptyProject = this.emptyProjectFallsThroughToSession
          && this.sources.length === 0
        const indexTargets = noRetrieval
          ? []
          : this.project && !emptyProject
          ? [
              {
                target_type: 'project',
                target_id: this.project.id,
                index_status: this.projectIndexReady ? 'ready' : 'unindexed',
              },
              ...(this.emptyProjectFallsThroughToSession
                ? [{
                    target_type: 'session',
                    target_id: sessionId,
                    index_status: this.sessionIndexReady ? 'ready' : 'unindexed',
                  }]
                : []),
            ]
          : [{
              target_type: 'session',
              target_id: sessionId,
              index_status: this.sessionIndexReady ? 'ready' : 'unindexed',
            }]
        await json(route, {
          effective_mode: 'smart',
          estimated_tokens: 180,
          index_status: indexTargets.some(({ index_status }) => index_status !== 'ready')
            ? 'unindexed'
            : 'ready',
          index_targets: indexTargets,
          preview: `Context preview for ${sessionId}.`,
          rag_used: !noRetrieval,
          retrieval_mode: noRetrieval
            ? 'none'
            : this.projectIndexReady || this.sessionIndexReady
            ? 'hybrid'
            : 'lexical_fallback',
          segment_count: 1,
          sources: [{
            kind: 'transcript',
            id: sessionId,
            label: `Session transcript ${sessionId}`,
          }],
          truncated: false,
        })
        return
      }
      if (method === 'POST' && url.pathname === '/api/ai/index/preview') {
        const targetType = String(body?.target_type)
        const targetId = String(body?.target_id)
        const isProject = targetType === 'project'
        const ready = isProject ? this.projectIndexReady : this.sessionIndexReady
        const emptyProject = isProject
          && this.emptyProjectFallsThroughToSession
          && this.sources.length === 0
        await json(route, {
          target_type: targetType,
          target_id: targetId,
          model: 'text-embedding-3-small',
          dimensions: 1_536,
          source_count: emptyProject ? 0 : 1,
          chunk_count: emptyProject ? 0 : 2,
          indexed_chunks: ready ? 2 : 0,
          pending_chunks: ready || emptyProject ? 0 : 2,
          estimated_tokens: ready || emptyProject ? 0 : 20,
          estimated_dp: ready || emptyProject ? 0 : 0.01,
          confirmation_token: ready || emptyProject
            ? undefined
            : `signed-confirmation-${targetType}`,
          index_status: ready ? 'ready' : 'unindexed',
          requires_indexing: !ready && !emptyProject,
        })
        return
      }
      if (method === 'POST' && url.pathname === '/api/ai/index/jobs') {
        await json(route, {
          job: {
            id: `job-${String(body?.target_type)}`,
            target_type: body?.target_type,
            target_id: body?.target_id,
            model: 'text-embedding-3-small',
            dimensions: 1_536,
            status: 'processing',
            chunk_count: 2,
            processed_chunks: 1,
            estimated_tokens: 20,
            estimated_dp: 0.01,
            client_request_id: body?.client_request_id,
            attempt_count: 0,
            max_attempts: 3,
            created_at: now,
            updated_at: now,
          },
        }, 201)
        return
      }
      const jobMatch = url.pathname.match(/^\/api\/ai\/index\/jobs\/([^/]+)$/)
      if (method === 'GET' && jobMatch) {
        const targetType = jobMatch[1]?.replace('job-', '') === 'session'
          ? 'session'
          : 'project'
        if (this.indexJobCanFinish) {
          if (targetType === 'project') this.projectIndexReady = true
          else this.sessionIndexReady = true
        }
        await json(route, {
          job: {
            id: jobMatch[1],
            target_type: targetType,
            target_id: targetType === 'project'
              ? this.project?.id
              : this.sessions[0]?.id,
            model: 'text-embedding-3-small',
            dimensions: 1_536,
            status: this.indexJobReturnsError
              ? 'error'
              : this.indexJobCanFinish ? 'ready' : 'processing',
            chunk_count: 2,
            processed_chunks: this.indexJobCanFinish ? 2 : 1,
            estimated_tokens: 20,
            actual_tokens: this.indexJobCanFinish ? 18 : undefined,
            estimated_dp: 0.01,
            error_message: this.indexJobReturnsError
              ? 'mock indexing failure'
              : undefined,
            attempt_count: 1,
            max_attempts: 3,
            created_at: now,
            updated_at: now,
          },
        })
        return
      }
      const retryJobMatch = url.pathname.match(
        /^\/api\/ai\/index\/jobs\/([^/]+)\/retry$/,
      )
      if (method === 'POST' && retryJobMatch) {
        if (this.indexRetryReturnsConflict) {
          this.indexRetryReturnsConflict = false
          this.indexJobReturnsError = false
          this.indexJobCanFinish = true
          await json(route, {
            error: 'index target changed after confirmation; preview again',
          }, 409)
          return
        }
        await json(route, {
          job: {
            id: retryJobMatch[1],
            target_type: 'session',
            target_id: this.sessions[0]?.id,
            model: 'text-embedding-3-small',
            dimensions: 1_536,
            status: 'processing',
            chunk_count: 2,
            processed_chunks: 0,
            estimated_tokens: 20,
            estimated_dp: 0.01,
            attempt_count: 2,
            max_attempts: 3,
            created_at: now,
            updated_at: now,
          },
        }, 202)
        return
      }
      if (method === 'DELETE' && jobMatch) {
        await json(route, {
          job: {
            id: jobMatch[1],
            target_type: jobMatch[1] === 'job-session' ? 'session' : 'project',
            target_id: jobMatch[1] === 'job-session'
              ? this.sessions[0]?.id
              : this.project?.id,
            model: 'text-embedding-3-small',
            dimensions: 1_536,
            status: 'cancelled',
            chunk_count: 2,
            processed_chunks: 1,
            estimated_tokens: 20,
            estimated_dp: 0.01,
            attempt_count: 1,
            max_attempts: 3,
            created_at: now,
            updated_at: now,
          },
        })
        return
      }

      if (method === 'POST' && url.pathname === '/api/rag/ask') {
        const requestedSessionId = String(body?.session_id ?? '')
        await this.delay(`chat:${requestedSessionId}`)
        await json(route, {
          answer: this.noRetrievalSessions.has(requestedSessionId)
            ? `Chat answer for ${requestedSessionId}.`
            : 'Alice owns the release notes and the launch decision is Friday.',
          context: {
            effective_mode: 'smart',
            estimated_tokens: 180,
            index_status: this.projectIndexReady || this.sessionIndexReady
              ? 'ready'
              : 'unindexed',
            rag_used: true,
            retrieval_mode: body?.retrieval_preference === 'lexical_only'
              ? 'lexical_fallback'
              : 'hybrid',
            truncated: false,
          },
          latency_ms: 25,
          usage: {
            prompt_tokens: 180,
            completion_tokens: 20,
            total_tokens: 200,
            model: 'gpt-5-mini',
          },
        })
        return
      }
      if (method === 'GET' && url.pathname === '/api/ai/artifacts') {
        const requestedSessionId = url.searchParams.get('session_id') ?? ''
        await this.delay(`artifacts:${requestedSessionId}`)
        await json(route, {
          artifacts: this.artifactsBySession.get(requestedSessionId) ?? this.artifacts,
        })
        return
      }
      if (method === 'POST' && url.pathname === '/api/ai/artifacts') {
        const requestedSessionId = String(body?.session_id ?? '')
        await this.delay(`artifact-create:${requestedSessionId}`)
        const artifactType = body?.artifact_type as MockArtifact['artifact_type']
        const titles: Record<MockArtifact['artifact_type'], string> = {
          summary: 'Session summary',
          notes: 'Session notes',
          action_items: 'Session action items',
        }
        const artifact: MockArtifact = {
          artifact_type: artifactType,
          content: this.noRetrievalSessions.has(requestedSessionId)
            ? `Generated ${artifactType} for ${requestedSessionId}.`
            : `Generated ${artifactType} from the session and project knowledge.`,
          context_tokens: 180,
          created_at: now,
          id: `artifact-${artifactType}`,
          model: 'gpt-5-mini',
          title: this.noRetrievalSessions.has(requestedSessionId)
            ? `${titles[artifactType]} ${requestedSessionId}`
            : titles[artifactType],
        }
        const scopedArtifacts = this.artifactsBySession.get(requestedSessionId)
        if (scopedArtifacts) {
          this.artifactsBySession.set(requestedSessionId, [...scopedArtifacts, artifact])
        } else {
          this.artifacts = [...this.artifacts, artifact]
        }
        await json(route, {
          artifact,
          context: {
            effective_mode: 'smart',
            estimated_tokens: 180,
            index_status: 'ready',
            rag_used: true,
            retrieval_mode: 'hybrid',
            truncated: false,
          },
          latency_ms: 25,
        }, 201)
        return
      }
      const artifactMatch = url.pathname.match(/^\/api\/ai\/artifacts\/([^/]+)$/)
      if (method === 'DELETE' && artifactMatch) {
        this.artifacts = this.artifacts.filter(({ id }) => id !== artifactMatch[1])
        await route.fulfill({ status: 204 })
        return
      }

      this.unhandled.push(`${method} ${url.pathname}`)
      await json(route, { error: `Unhandled E2E API route: ${method} ${url.pathname}` }, 501)
    })
  }
}

async function login(page: Page): Promise<void> {
  await page.goto('/pro.html')
  await page.locator('.dt-auth__card input[type="email"]').fill(user.email)
  await page.locator('.dt-auth__card input[type="password"]').fill('password123')
  await page.locator('.dt-auth__card .dt-button--primary').click()
  await expect(page.locator('.dt-sidebar')).toBeVisible()
}

async function loadSession(page: Page, title: string): Promise<void> {
  const sessionButton = page.locator('.dt-history-item__main', { hasText: title })
  await expect(sessionButton).toBeVisible()
  await sessionButton.click()
  await expect(sessionButton.locator('..')).toHaveClass(/is-active/)
}

async function loadSessionBehindSheet(page: Page, title: string): Promise<void> {
  const sessionButton = page.locator('.dt-history-item__main', { hasText: title })
  await expect(sessionButton).toBeVisible()
  await sessionButton.evaluate((element) => (element as HTMLButtonElement).click())
  await expect(sessionButton.locator('..')).toHaveClass(/is-active/)
}

async function openAssistant(page: Page): Promise<void> {
  await page.locator('.dt-recorder button[aria-label="打开 AI 助手"]').click()
  await expect(page.locator('.dt-assistant')).toBeVisible()
}

async function selectAssistantTab(page: Page, index: 0 | 1 | 2): Promise<void> {
  await page.locator('.dt-assistant [role="tablist"] button').nth(index).click()
}

async function openAssistantSettings(page: Page): Promise<void> {
  const settings = page.locator('.dt-ai-settings')
  if (!await settings.evaluate((element) => (element as HTMLDetailsElement).open)) {
    await settings.locator(':scope > summary').click()
  }
  // Advanced options (context mode, preview, OCR) stay collapsed by default for UX.
  const advanced = settings.locator('.dt-ai-advanced')
  if (
    await advanced.count()
    && !await advanced.evaluate((element) => (element as HTMLDetailsElement).open)
  ) {
    await advanced.locator(':scope > summary').click()
  }
}

for (const model of ['gpt-5-mini', 'cerebras::qwen-3.8-27b']) {
  test(`chat without a transcript session works with ${model}`, async ({ page }) => {
    const backend = new MockAIBackend([])
    await backend.install(page)
    const requests: Array<Record<string, unknown>> = []
    await page.route('**/api/rag/ask', async route => {
      const body = jsonBody(route) ?? {}
      requests.push(body)
      // Match the server contract: an absent session is allowed, but a
      // placeholder such as "current_session" must not reach UUID validation.
      if (body.session_id !== '') {
        await route.fulfill({ status: 400, body: 'session_id must be a UUID' })
        return
      }
      await json(route, {
        answer: `Reply ${requests.length} from ${model}`,
        usage: { model, total_tokens: 20 },
        context: { effective_mode: 'smart', rag_used: false, retrieval_mode: 'none', index_status: 'unindexed', estimated_tokens: 0, truncated: false },
      })
    })

    await login(page)
    await page.getByRole('button', { name: '跳过引导', exact: true }).click()
    await openAssistant(page)
    await page.locator('.dt-chat__composer textarea').fill('Hello')
    await page.locator('.dt-chat__composer button[type="submit"]').click()
    await expect(page.locator('.dt-chat__message--assistant')).toContainText(`Reply 1 from ${model}`)
    await page.locator('.dt-chat__composer textarea').fill('Continue')
    await page.locator('.dt-chat__composer button[type="submit"]').click()
    await expect(page.locator('.dt-chat__message--assistant').last()).toContainText(`Reply 2 from ${model}`)
    expect(requests).toHaveLength(2)
    expect(requests[1]).toMatchObject({
      session_id: '',
      question: 'Continue',
      history: expect.arrayContaining([
        { role: 'user', content: 'Hello' },
        { role: 'assistant', content: `Reply 1 from ${model}` },
      ]),
    })
    expect(backend.records.some(({ method, path }) => method === 'POST' && path === '/api/sessions')).toBe(false)
    expect(backend.unhandled).toEqual([])
  })
}

test('AI project workflow survives index progress reload and keeps API contracts', async ({
  page,
}) => {
  const backend = new MockAIBackend([{
    id: 'session-1',
    sourceLanguage: 'en',
    title: 'AI E2E session',
  }])
  await backend.install(page)
  page.on('dialog', (dialog) => { void dialog.accept() })

  await login(page)
  await loadSession(page, 'AI E2E session')
  await openAssistant(page)
  await selectAssistantTab(page, 2)

  const newProjectRow = page.locator('.dt-ai-memory-row').filter({
    has: page.locator('input'),
  }).first()
  await newProjectRow.locator('input').fill('Launch knowledge')
  await newProjectRow.locator('button').click()
  await expect(page.locator('.dt-ai-project-editor')).toBeVisible()

  await page.locator('.dt-ai-file-button input[type="file"]').setInputFiles({
    buffer: Buffer.from('Launch is Friday. Alice owns the release notes.'),
    mimeType: 'text/plain',
    name: 'launch-notes.txt',
  })
  await expect(page.locator('.dt-ai-source-list article')).toContainText(
    'launch-notes.txt',
  )
  await selectAssistantTab(page, 0)
  await openAssistantSettings(page)
  await page.locator('.dt-ai-reasoning [role="radio"]').filter({
    hasText: '深入',
  }).click()
  await page.locator('.dt-chat__composer textarea').fill('Who owns the release notes?')
  await page.locator('.dt-chat__composer button[type="submit"]').click()
  await expect(page.locator('.dt-ai-index-gate')).toBeVisible()
  await page.locator('.dt-ai-index-gate .dt-button--primary').click()
  await expect(page.locator('.dt-ai-index-gate progress')).toHaveAttribute('value', '50')
  await expect.poll(() => page.evaluate(() => (
    Object.keys(sessionStorage).some((key) => key.startsWith('dreamtrans:ai-index:'))
  ))).toBe(true)

  await page.reload()
  await expect(page.locator('.dt-sidebar')).toBeVisible()
  await loadSession(page, 'AI E2E session')
  backend.indexJobCanFinish = true
  await openAssistant(page)
  await expect(page.locator('.dt-chat__message--assistant')).toContainText(
    'Alice owns the release notes',
    { timeout: 10_000 },
  )
  await expect.poll(() => page.evaluate(() => (
    Object.keys(sessionStorage).some((key) => key.startsWith('dreamtrans:ai-index:'))
  ))).toBe(false)

  await selectAssistantTab(page, 1)
  for (const [buttonIndex, title] of [
    [0, 'Session summary'],
    [1, 'Session notes'],
    [2, 'Session action items'],
  ] as const) {
    await page.locator('.dt-ai-artifact-actions button').nth(buttonIndex).click()
    await expect(page.locator('.dt-ai-artifact', { hasText: title })).toBeVisible()
  }
  await expect(page.locator('.dt-ai-artifact')).toHaveCount(3)

  await page.locator('.dt-ai-artifact').first().locator('button').nth(2).click()
  await expect(page.locator('.dt-ai-artifact')).toHaveCount(2)

  await selectAssistantTab(page, 2)
  await page.locator('.dt-ai-source-list article').first().locator('button').last().click()
  await expect(page.locator('.dt-ai-source-list article')).toHaveCount(0)
  await page.locator('.dt-ai-project-editor > summary').click()
  await page.locator('.dt-ai-project-editor .dt-ai-action-row button').nth(1).click()
  await expect(page.locator('.dt-ai-project-editor')).toHaveCount(0)

  const record = (method: string, path: string) => backend.records.find(
    (candidate) => candidate.method === method && candidate.path === path,
  )
  expect(record('POST', '/api/auth/login')?.body).toMatchObject({
    email: user.email,
  })
  expect(record('POST', '/api/ai/projects')?.body).toMatchObject({
    context_mode: 'smart',
    max_context_tokens: 64_000,
    name: 'Launch knowledge',
  })
  expect(record('POST', '/api/ai/projects/project-1/sessions')?.body).toEqual({
    session_id: 'session-1',
  })
  const upload = record('POST', '/api/ai/projects/project-1/sources')
  expect(upload?.postData).toContain('name="session_id"')
  expect(upload?.postData).toContain('session-1')
  expect(upload?.postData).toContain('name="ocr_language"')
  expect(upload?.postData).toContain('eng')
  expect(upload?.postData).toContain('launch-notes.txt')

  const indexCreate = record('POST', '/api/ai/index/jobs')
  expect(indexCreate?.body).toMatchObject({
    confirmation_token: 'signed-confirmation-project',
    confirmed: true,
    project_id: 'project-1',
    session_id: 'session-1',
    target_id: 'project-1',
    target_type: 'project',
  })
  expect(indexCreate?.body?.client_request_id).toEqual(expect.any(String))

  const ask = record('POST', '/api/rag/ask')
  expect(ask?.body).toMatchObject({
    project_id: 'project-1',
    question: 'Who owns the release notes?',
    reasoning_effort: 'high',
    retrieval_preference: 'auto',
    session_id: 'session-1',
  })
  expect(ask?.body?.client_request_id).toEqual(expect.any(String))

  const artifactCreates = backend.records.filter(
    ({ method, path }) => method === 'POST' && path === '/api/ai/artifacts',
  )
  expect(artifactCreates.map(({ body }) => body?.artifact_type)).toEqual([
    'summary',
    'notes',
    'action_items',
  ])
  expect(artifactCreates.map(({ body }) => body?.reasoning_effort)).toEqual([
    'high',
    'high',
    'high',
  ])
  expect(new Set(artifactCreates.map(({ body }) => body?.client_request_id)).size).toBe(3)
  expect(record('DELETE', '/api/ai/artifacts/artifact-summary')).toBeDefined()
  expect(record('DELETE', '/api/ai/projects/project-1/sources/source-1')).toBeDefined()
  expect(record('DELETE', '/api/ai/projects/project-1')).toBeDefined()
  expect(backend.unhandled).toEqual([])
})

test('empty project falls through to session indexing and OCR follows session language', async ({
  page,
}) => {
  const backend = new MockAIBackend([
    { id: 'session-en', sourceLanguage: 'en', title: 'English session' },
    { id: 'session-ja', sourceLanguage: 'ja', title: 'Japanese session' },
  ])
  backend.emptyProjectFallsThroughToSession = true
  await backend.install(page)

  await login(page)
  await loadSession(page, 'English session')
  await openAssistant(page)
  await selectAssistantTab(page, 2)
  const newProjectRow = page.locator('.dt-ai-memory-row').filter({
    has: page.locator('input'),
  }).first()
  await newProjectRow.locator('input').fill('Empty project')
  await newProjectRow.locator('button').click()
  await expect(page.locator('.dt-ai-project-editor')).toBeVisible()

  const ocr = page.locator('.dt-ai-language-picker input[type="checkbox"]')
  await expect(ocr.nth(0)).toBeChecked()
  await expect(ocr.nth(2)).not.toBeChecked()

  await page.locator('.dt-sheet__header .dt-icon-button').click()
  await loadSession(page, 'Japanese session')
  await openAssistant(page)
  await selectAssistantTab(page, 2)
  await expect(ocr.nth(0)).not.toBeChecked()
  await expect(ocr.nth(2)).toBeChecked()

  backend.unavailableSessionIds.add('session-ja')
  await page.reload()
  await expect(page.locator('.dt-sidebar')).toBeVisible()
  await loadSession(page, 'Japanese session')
  await openAssistant(page)
  await selectAssistantTab(page, 2)
  await expect(ocr.nth(0)).not.toBeChecked()
  await expect(ocr.nth(2)).toBeChecked()

  await selectAssistantTab(page, 0)
  await page.locator('.dt-chat__composer textarea').fill('What did we decide?')
  await page.locator('.dt-chat__composer button[type="submit"]').click()
  await expect(page.locator('.dt-ai-index-gate')).toBeVisible()

  await expect.poll(() => backend.records.filter(
    ({ method, path }) => method === 'POST' && path === '/api/ai/index/preview',
  ).length).toBe(1)
  const indexPreviews = backend.records.filter(
    ({ method, path }) => method === 'POST' && path === '/api/ai/index/preview',
  )
  expect(indexPreviews.map(({ body }) => [
    body?.target_type,
    body?.target_id,
  ])).toEqual([
    ['session', 'session-ja'],
  ])

  await page.locator('.dt-ai-index-gate .dt-button--secondary').click()
  await expect(page.locator('.dt-chat__message--assistant')).toContainText(
    'Alice owns the release notes',
  )
  const ask = backend.records.find(
    ({ method, path }) => method === 'POST' && path === '/api/rag/ask',
  )
  expect(ask?.body).toMatchObject({
    project_id: 'project-1',
    retrieval_preference: 'lexical_only',
    session_id: 'session-ja',
  })
  expect(backend.unhandled).toEqual([])
})

test('late AI responses never cross owner or session scope', async ({ page }) => {
  const backend = new MockAIBackend([
    { id: 'session-a', sourceLanguage: 'en', title: 'Race session A' },
    { id: 'session-b', sourceLanguage: 'ja', title: 'Race session B' },
  ])
  const projectA: MockProject = {
    context_mode: 'smart',
    description: 'Only session A may read this.',
    id: 'project-a',
    max_context_tokens: 64_000,
    name: 'Project only A',
  }
  const projectB: MockProject = {
    context_mode: 'smart',
    description: 'Only session B may read this.',
    id: 'project-b',
    max_context_tokens: 64_000,
    name: 'Project only B',
  }
  const artifact = (session: 'a' | 'b'): MockArtifact => ({
    artifact_type: 'summary',
    content: `Stored artifact for session ${session}.`,
    context_tokens: 20,
    created_at: now,
    id: `artifact-${session}`,
    model: 'gpt-5-mini',
    title: `Artifact only ${session.toUpperCase()}`,
  })
  backend.projectsBySession.set('session-a', {
    linkedProjectId: '',
    projects: [projectA],
  })
  backend.projectsBySession.set('session-b', {
    linkedProjectId: '',
    projects: [projectB],
  })
  backend.artifactsBySession.set('session-a', [artifact('a')])
  backend.artifactsBySession.set('session-b', [artifact('b')])
  backend.noRetrievalSessions.add('session-a')
  backend.noRetrievalSessions.add('session-b')
  backend.responseDelayMs.set('projects:session-a', 600)
  await backend.install(page)

  await login(page)
  await loadSession(page, 'Race session A')
  const delayedProjects = page.waitForRequest((request) => (
    request.method() === 'GET'
    && request.url().includes('/api/ai/projects?session_id=session-a')
  ))
  await openAssistant(page)
  await delayedProjects
  await loadSessionBehindSheet(page, 'Race session B')
  await selectAssistantTab(page, 2)
  const projectSelect = page.locator('.dt-ai-field select').first()
  await expect(projectSelect.locator('option')).toContainText([
    '不关联项目',
    'Project only B',
  ])
  await page.waitForTimeout(700)
  await expect(projectSelect).not.toContainText('Project only A')

  await loadSessionBehindSheet(page, 'Race session A')
  const delayedArtifacts = page.waitForRequest((request) => (
    request.method() === 'GET'
    && request.url().includes('/api/ai/artifacts?session_id=session-a')
  ))
  await selectAssistantTab(page, 1)
  await delayedArtifacts
  await loadSessionBehindSheet(page, 'Race session B')
  await selectAssistantTab(page, 1)
  await expect(page.locator('.dt-ai-artifact')).toContainText('Artifact only B')
  await page.waitForTimeout(700)
  await expect(page.locator('.dt-ai-artifact')).not.toContainText('Artifact only A')

  await loadSessionBehindSheet(page, 'Race session A')
  await openAssistantSettings(page)
  await expect(page.getByRole('button', { name: '预览实际读取内容' })).toBeEnabled()
  backend.responseDelayMs.set('chat:session-a', 600)
  const delayedChat = page.waitForRequest((request) => (
    request.method() === 'POST'
    && new URL(request.url()).pathname === '/api/rag/ask'
    && request.postDataJSON().session_id === 'session-a'
  ))
  await page.locator('.dt-chat__composer textarea').fill('Old session question')
  await page.locator('.dt-chat__composer button[type="submit"]').click()
  await delayedChat
  await loadSessionBehindSheet(page, 'Race session B')
  await page.waitForTimeout(700)
  await expect(page.locator('.dt-chat__messages')).not.toContainText(
    'Chat answer for session-a',
  )
  const sessionBHistory = await page.evaluate(() => (
    Object.entries(localStorage)
      .filter(([key]) => key.includes('session-b') && key.includes('ai-chat'))
      .map(([, value]) => value)
      .join('\n')
  ))
  expect(sessionBHistory).not.toContain('Old session question')

  await loadSessionBehindSheet(page, 'Race session A')
  await openAssistantSettings(page)
  await expect(page.getByRole('button', { name: '预览实际读取内容' })).toBeEnabled()
  backend.responseDelayMs.set('context:session-a', 600)
  const delayedPreview = page.waitForRequest((request) => (
    request.method() === 'POST'
    && new URL(request.url()).pathname === '/api/ai/context/preview'
    && request.postDataJSON().session_id === 'session-a'
  ))
  await page.getByRole('button', { name: '预览实际读取内容' }).click()
  await delayedPreview
  await loadSessionBehindSheet(page, 'Race session B')
  await page.waitForTimeout(700)
  await expect(page.locator('.dt-ai-context-preview')).toHaveCount(0)

  backend.responseDelayMs.delete('context:session-a')
  await loadSessionBehindSheet(page, 'Race session A')
  await openAssistantSettings(page)
  await expect(page.getByRole('button', { name: '预览实际读取内容' })).toBeEnabled()
  await selectAssistantTab(page, 1)
  backend.responseDelayMs.set('artifact-create:session-a', 600)
  const delayedArtifactCreate = page.waitForRequest((request) => (
    request.method() === 'POST'
    && new URL(request.url()).pathname === '/api/ai/artifacts'
    && request.postDataJSON().session_id === 'session-a'
  ))
  await page.locator('.dt-ai-artifact-actions button').first().click()
  await delayedArtifactCreate
  await loadSessionBehindSheet(page, 'Race session B')
  await selectAssistantTab(page, 1)
  await page.waitForTimeout(700)
  await expect(page.locator('.dt-ai-artifact')).toContainText('Artifact only B')
  await expect(page.locator('.dt-ai-artifact')).not.toContainText(
    'Session summary session-a',
  )
  expect(backend.unhandled).toEqual([])
})

test('index retry conflict requires a fresh preview and client request ID', async ({
  page,
}) => {
  const backend = new MockAIBackend([{
    id: 'session-retry',
    sourceLanguage: 'en',
    title: 'Index retry session',
  }])
  backend.indexJobReturnsError = true
  backend.indexRetryReturnsConflict = true
  await backend.install(page)

  await login(page)
  await loadSession(page, 'Index retry session')
  await openAssistant(page)
  await openAssistantSettings(page)
  await expect(page.getByRole('button', { name: '预览实际读取内容' })).toBeEnabled()
  await page.locator('.dt-chat__composer textarea').fill('Retry this index safely')
  await page.locator('.dt-chat__composer button[type="submit"]').click()
  await expect(page.locator('.dt-ai-index-gate')).toBeVisible()
  await page.locator('.dt-ai-index-gate .dt-button--primary').click()
  await expect(page.locator('.dt-ai-index-gate [role="alert"]')).toContainText(
    'mock indexing failure',
    { timeout: 10_000 },
  )

  await page.locator('.dt-ai-index-gate .dt-button--primary').click()
  await expect.poll(() => backend.records.filter(
    ({ method, path }) => (
      method === 'POST'
      && path === '/api/ai/index/preview'
    ),
  ).length).toBe(2)
  await expect(page.locator('.dt-ai-index-gate progress')).toHaveCount(0)

  await page.locator('.dt-ai-index-gate .dt-button--primary').click()
  await expect(page.locator('.dt-chat__message--assistant')).toContainText(
    'Alice owns the release notes',
    { timeout: 10_000 },
  )
  const indexCreates = backend.records.filter(
    ({ method, path }) => method === 'POST' && path === '/api/ai/index/jobs',
  )
  expect(indexCreates).toHaveLength(2)
  expect(indexCreates[0]?.body?.client_request_id).toEqual(expect.any(String))
  expect(indexCreates[1]?.body?.client_request_id).toEqual(expect.any(String))
  expect(indexCreates[1]?.body?.client_request_id).not.toBe(
    indexCreates[0]?.body?.client_request_id,
  )
  expect(backend.records.some(({ method, path }) => (
    method === 'POST'
    && path === '/api/ai/index/jobs/job-session/retry'
  ))).toBe(true)
  expect(backend.unhandled).toEqual([])
})

test('学习空间 opens a course, links a session, and deep-links into the workspace', async ({
  page,
}) => {
  const backend = new MockAIBackend([{
    id: 'session-1',
    sourceLanguage: 'en',
    title: 'AI E2E session',
  }])
  backend.project = {
    context_mode: 'smart',
    description: 'Research methods course.',
    id: 'project-1',
    max_context_tokens: 64_000,
    name: 'PSY2041',
  }
  await backend.install(page)

  await login(page)

  // 学习空间 is its own page (async study), separate from the live workspace.
  await page.locator('.dt-nav button', { hasText: '学习空间' }).click()
  await expect(page).toHaveURL(/\/pro\/study/)
  await expect(page.locator('.dt-study-topbar')).toContainText('学习空间')
  const study = page.locator('.dt-study')
  await expect(study).toBeVisible()

  const courseCard = study.locator('.dt-study__card', { hasText: 'PSY2041' })
  await expect(courseCard).toContainText('Research methods course.')
  await courseCard.click()
  await expect(study.locator('h2')).toContainText('PSY2041')

  // An empty course opens on setup guidance; management lives on its own tab.
  await expect(study).toContainText('这门课还是空的')
  await study.getByRole('button', { name: '去添加会话和资料' }).click()
  await expect(study).toContainText('这门课程还没有会话')

  // Link an existing cloud session from the picker.
  await study.getByRole('button', { name: '添加会话' }).click()
  await study.locator('.dt-study__picker .dt-study__row-main', {
    hasText: 'AI E2E session',
  }).click()
  const linkRecord = backend.records.find(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/sessions'
  ))
  expect(linkRecord?.body).toEqual({ session_id: 'session-1' })

  const sessionRow = study.locator('.dt-study__sessions .dt-study__row', {
    hasText: 'AI E2E session',
  })
  await expect(sessionRow).toBeVisible()

  // Upload a course material; it lists with its extraction status.
  await study.locator('input[aria-label="上传课程资料"]').setInputFiles({
    name: 'launch-notes.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('Correlation is not causation.'),
  })
  const materialRow = study.locator('.dt-study__source', { hasText: 'launch-notes.txt' })
  await expect(materialRow).toBeVisible()
  await expect(materialRow).toContainText('已就绪')
  expect(backend.records.some(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/sources'
  ))).toBe(true)

  // Generate the route, expand a skill, and check evidence + new badge.
  await expect(study).toContainText('还没有技能路线')
  page.once('dialog', async (dialog) => {
    expect(dialog.message()).toContain('扣除')
    await dialog.accept()
  })
  await study.locator('.dt-study__map').getByRole('button', { name: '生成技能路线' }).click()
  await expect.poll(() => backend.records.some(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/skill-map'
  ))).toBe(true)
  const generateRecord = backend.records.find(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/skill-map'
  ))
  expect(generateRecord?.body?.client_request_id).toEqual(expect.any(String))
  const skillRows = study.locator('.dt-study__skill')
  await expect(skillRows).toHaveCount(2)
  await expect(skillRows.nth(1)).toContainText('区分相关与因果')
  await expect(skillRows.nth(1).locator('.dt-study__skill-new')).toContainText('NEW')
  await skillRows.nth(0).locator('.dt-study__skill-head').click()
  await expect(skillRows.nth(0)).toContainText('correlation does not imply causation')
  await expect(study).toContainText('基于 1 场会话生成')

  // 今日行动: one mission card for the recommended skill and the route strip.
  await study.locator('.dt-study__tabs button', { hasText: '今日行动' }).click()
  const mission = study.locator('.dt-study__mission')
  await expect(mission).toContainText('OP-01')
  await expect(mission).toContainText('识别相关关系')
  await expect(study.locator('.dt-route__node')).toHaveCount(2)
  // 按周: the owed week is flagged and drives today's mission.
  const weeksPanel = study.locator('.dt-weeks')
  await expect(weeksPanel.locator('.dt-weeks__chip')).toHaveCount(2)
  await expect(weeksPanel).toContainText('第 1 周还没练熟')
  await expect(mission).toContainText('WEEK 01')
  await weeksPanel.locator('.dt-weeks__chip').nth(1).click()
  await expect(weeksPanel).toContainText('launch-notes.txt')
  await mission.getByRole('button', { name: '开始行动' }).click()

  // A never-practiced skill opens on its lesson card first.
  const practice = page.locator('.dt-practice')
  await expect(practice).toContainText('看到 correlation，先想第三变量')
  await expect(practice).toContainText('confounding variable')
  await expect(practice).toContainText('No. The data are correlational.')
  await practice.getByRole('button', { name: '看完了，做题' }).click()

  // Practice loop: situation → hint → answer → stamp with the reveal.
  await expect(practice).toContainText('Can the researchers conclude coffee improves grades?')
  await expect(practice).toContainText('中文框架 · EN 术语')
  const nextRecord = backend.records.find(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/study/next'
  ))
  expect(nextRecord?.body).toMatchObject({
    skill_label: '识别相关关系',
    practice_session_id: expect.any(String),
  })
  await practice.getByRole('button', { name: '看中文' }).click()
  await expect(practice).toContainText('研究者能得出咖啡提升成绩的结论吗？')
  await practice.getByRole('button', { name: '要提示' }).click()
  await expect(practice).toContainText('想想有没有第三个变量')
  // Language scaffolds: glossary shows.
  await expect(practice).toContainText('混淆变量')
  await practice.locator('textarea').fill(
    'No: correlation does not establish causation; a confound like study habits could drive both.',
  )
  await practice.getByRole('button', { name: '提交' }).click()
  await expect(practice.locator('.dt-practice__stamp')).toContainText('稳了')
  await expect(practice.locator('.dt-practice__grade--D')).toBeVisible()
  await expect(practice).toContainText('抓到了混淆变量')
  await expect(practice).toContainText('再主动改写结论就能到 HD')
  await expect(practice).toContainText('180')
  await expect(practice).toContainText('升到「辅助」')
  // The teaching layer is always revealed: model answer, explanation, targets.
  await expect(practice).toContainText('a confound such as study habits could drive both')
  await expect(practice).toContainText('先找第三变量')
  await expect(practice.locator('.dt-practice__target')).toHaveCount(2)
  const attemptRecord = backend.records.find(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/study/attempts'
  ))
  expect(attemptRecord?.body).toMatchObject({
    scenario_id: 'scenario-1',
    used_hint: true,
    used_zh: true,
    practice_session_id: expect.any(String),
  })
  // Every charged step accumulates into the visible session cost.
  await expect(practice.locator('.dt-practice__cost')).toBeVisible()

  // Next item is a single-choice recognition question: pick, justify, submit.
  await practice.getByRole('button', { name: '下一题' }).click()
  await expect(practice).toContainText('Which variable most plausibly explains the link?')
  await expect(practice).toContainText('单选')
  await expect(practice).toContainText('下一题把提示拿掉。')
  await practice.getByRole('button', { name: /Temperature/ }).click()
  await practice.locator('textarea').fill('Heat make both go up.')
  await practice.getByRole('button', { name: '提交' }).click()
  await expect(practice.locator('.dt-practice__stamp')).toContainText('过关')
  await expect(practice).toContainText('drives both')
  await expect(practice).toContainText('2 COMBO')
  // Option notes explain every choice once answered.
  await expect(practice.locator('.dt-practice__option.is-right')).toContainText('天气热同时推高两者')
  const choiceRecord = backend.records.filter(({ method, path }) => (
    method === 'POST' && path === '/api/ai/projects/project-1/study/attempts'
  )).at(-1)
  expect(choiceRecord?.body).toMatchObject({
    scenario_id: 'scenario-2',
    choices: [1],
    reason: 'Heat make both go up.',
    used_hint: false,
  })

  // 收工: the after-action card says what got clearer, then the route lights up.
  await practice.getByRole('button', { name: '收工' }).first().click()
  await expect(practice).toContainText('AFTER ACTION')
  await expect(practice).toContainText('2 题，2 题过关')
  await practice.getByRole('button', { name: '收工' }).click()
  await expect(practice).toHaveCount(0)
  await expect(study.locator('.dt-route__node.is-supervised')).toBeVisible()

  // Opening a session deep-links back into the transcription workspace.
  await study.locator('.dt-study__tabs button', { hasText: '课程管理' }).click()
  await sessionRow.locator('.dt-study__row-main').click()
  await expect(page).toHaveURL(/\/pro(?!\/study)/)
  await expect(
    page.locator('.dt-history-item', { hasText: 'AI E2E session' }),
  ).toHaveClass(/is-active/, { timeout: 10_000 })

  // The course keeps its sessions when reopened, and unlink empties it again.
  await page.locator('.dt-nav button', { hasText: '学习空间' }).click()
  await page.locator('.dt-study__card', { hasText: 'PSY2041' }).click()
  await page.locator('.dt-study__tabs button', { hasText: '课程管理' }).click()
  await expect(sessionRow).toBeVisible()
  await sessionRow.locator('button[aria-label="把 AI E2E session 移出课程"]').click()
  await expect(page.locator('.dt-study')).toContainText('这门课程还没有会话')
  expect(backend.records.some(({ method, path }) => (
    method === 'DELETE'
    && path === '/api/ai/projects/project-1/sessions/session-1'
  ))).toBe(true)
  expect(backend.unhandled).toEqual([])
})

test('history list shows each cloud session\'s realtime cost', async ({ page }) => {
  const backend = new MockAIBackend([{
    id: 'session-1',
    sourceLanguage: 'en',
    title: 'AI E2E session',
  }])
  // AI charges are deliberately excluded from the headline figure.
  backend.sessionCosts = [{
    session_id: 'session-1',
    transcription_usd: 0.108,
    transcription_seconds: 900,
    translation_usd: 0.012,
    ai_usd: 0.03,
    total_usd: 0.15,
  }]
  await backend.install(page)

  await login(page)

  const historyRow = page.locator('.dt-history-item', { hasText: 'AI E2E session' })
  await expect(historyRow).toBeVisible()
  await expect(historyRow).toContainText('$0.12')
  await expect(historyRow.getByText('$0.15')).toHaveCount(0)

  const costRequest = backend.records.find(({ method, path }) => (
    method === 'GET' && path === '/api/user/billing/session-costs'
  ))
  expect(costRequest).toBeTruthy()
  expect(backend.unhandled).toEqual([])
})
