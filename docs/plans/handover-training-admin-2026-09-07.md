# 交接文档：训练折扣/赠送额度 + 管理员后台（2026-09-07）

需求原文见对话里的两份 v1 文档（「训练折扣与赠送额度」「管理员后台」）。用户九条决定：
充值赠送算付费；扣费顺序 赠送→充值赠送→钱包；按余额来源路由；差额退回；
租户加类型字段（机构永远不训练），"多人会话"从需求删掉；折扣 20%（代码原 30% 是错的）；
领码/首充快照；数据库总开关先做、重开清零以后做；文案放注册页权益预览；
活动折扣只作用于付费部分。**原则：没掏钱的余额一律标准价。**
用户要求：不分期，全部做完。

## 当前状态

已推送到 main 的：`59c1277`（邀请链接营销：落地页 /invite、访问漏斗+UTM、阶段奖励、只归因的用户推荐）。

**工作树里有一大批未提交的改动（Phase A，训练折扣与赠送额度）**，`git status` 可见：

- `backend/migrations/044_training_funding.sql`（已在本机测试库 `dreamtrans-test-pg` 应用；`scripts/migrate.sh` 已改到 044）
- `backend/internal/billing/routing.go`（新）：`RouteDecision`、`RouteForUser`、`decideRoute`、
  `RefundRouteDiscount`、`TrainingProgramEnabled`、`TrainingProgramStatistics`、`RecordTrainingOptInChange`
- `backend/internal/billing/ledger.go`：`grants.funding`、`openGrantsTx` 赠送优先、`debitAccountTx` 增加 `preferPaid` 返回 `debitSplit{Grant,Gift,Wallet}`、按记录路由计价、`usage_logs.gift_usd/funding_route/training_route`、结算/退款带 gift 账
- `pricing.go`（`applyRoute`、`EstimateCharge` 按路由）、`accounts.go`（`GiftUSD`、`GrantItem.Funding`、`AccountSummary.Route`、`grantFunding`）、`promotion.go`（领码快照）、`payments.go`（`payments.training_opt_in`）、`billing.go`（默认折扣 20、`UsageRecord.Route`、Service 字段）
- `internal/risk/budget.go`（新 kind）、`internal/models`（Tenant.Kind/SpeechmaticsRoute、User.SpeechmaticsRoute）、`internal/store/postgres.go`（租户 kind/route 读写、`SetTenantRouting`、`SetUserSpeechmaticsRoute`、用户 select 多一列）
- handlers：`admin.go`（设置项 `training_program_enabled`、`gift_training_discount`；租户 kind/route；用户 route 强制；`auditTx`；`HandleTrainingProgramStats`）、`auth.go`（开关判断走 billing，记录改动）、`speechmatics_proxy.go`（连接时 `RouteForUser`，`audioMeter.route` 附到每条预扣，断开后 `RefundRouteDiscount`）、`batch_transcribe.go`（结算后退差额）、接口新增方法及测试桩、`cmd/web/main.go`（lookup 改为 `billingSvc.TrainingRouteForUser`，`/api/system/access` 用总开关，新路由 `/api/admin/training-program`）
- 测试：`backend/internal/billing/routing_integration_test.go`（五条验收用例，已通过）
- 前端：`src/api.ts`、`src/admin/api.ts`（注意此文件被 grep 识别为二进制，用 `grep -a`）、`src/admin/verification.ts`、i18n zh/en（训练文案改为需求原文、赠送提示、路由状态）、`legal/documents.ts`（条款新增段落 zh/en）、`AccountPanel`（赠送额度行、路由状态）、admin `SettingsPage`（两个开关 + 训练统计区）、`TenantsPage`（类型/账号）、`UsersPage`（强制账号）
- `docs/TRAINING_PROGRAM.md`（新）

### Phase A 还差什么才能提交

1. **一个失败的测试**：`internal/handlers/promotion_marketing_integration_test.go:151`
   （`TestPromotionLandingVisitsAndStagedRewards`，"discount not applied"）。原因：该用例的用户还有赠送余额，
   按新规则活动折扣只作用于付费部分，所以估价是标准价。修法：在断言折扣前把该用户的 gift grants
   `remaining_usd` 置 0（或给钱包充值并清空赠送），再比较 `EstimateCharge`。
2. **lint**（`~/go/bin/golangci-lint run --build-tags event_worker ./...`）：
   - misspell：把注释里所有 `programme` 改成 `program`（routing.go、admin.go、main.go、routing_integration_test.go）。
   - gosec G706：`batch_transcribe.go` 两处 `log.Printf` 带 `reservationKey`，改用 `strconv.Quote(reservationKey)` 或不打印 key。
   - unparam：`routing_integration_test.go` 的 `usageRow` 去掉未用的 `charge` 返回值。
3. 前端 `npm run verify:ci`（含 42 个 Playwright 用例）已在 Phase A 改动上跑过一次，全部通过；若再改前端需重跑：
   `cd frontend && unset DISPLAY && CI=true VITE_BACKEND_URL=/ VITE_BACKEND_WS_URL=/ npm run verify:ci`。
4. 全部通过后：只 `git add` 自己的文件，提交并推送（不要 `git add -A`）。
5. 生产：应用迁移 044；后台把折扣确认为 20；没有新环境变量。

### Phase A 里明确没做 / 做得不完整的
- **重开清零**：`training_program_enabled` 从关到开时清空所有人的 `training_opt_in`（用户决定以后做，路已留：开关在 settings，改动历史在 `training_opt_in_changes`）。
- 需求第 7 条「引导里展示但不主动推」：现在除了引导第 3 步，老账户登录还会弹一次 `TrainingProgramDialog`（frontend/src/unified/components/TrainingProgramDialog.tsx，localStorage 记住已关闭）。要不要去掉这个弹窗，需要问用户。
- 设置页（SettingsPanel）的训练开关旁只改了文案，没有像账户面板那样显示"当前赠送路由/机构/强制/已暂停"的原因（i18n 里 `settings.training.routeGift/routeInstitution/routePinned/routeOff` 已备好，未接线）。
- 旧版 `/api/token/rt` 路径：路由已走 `RouteForUser`，但那条路径的一次性预扣没有附 `Route`（会在账本里按当前状态自动决定，一致），也没有会话结束退差额（该路径没有结算钩子）。
- 批量转写：预扣时没有显式附 `Route`（账本自动决定），提交时用 `RouteForUser` 选账号，两者用同一规则，极小概率不一致（预扣与提交之间赠送余额刚好归零）。
- `usage_logs` 历史行的 `gift_usd` 迁移里按 balance_transactions 回填，`funding_route/training_route` 历史为 NULL。
- 生产还没部署：迁移 043（邀请营销）和 044 都未在 prod 应用；prod `training_discount_percent` 需确认为 20。
- 推广邀请（59c1277）的创建/暂停/文案修改、站内公告没有写 `admin_audit_logs`（需求要求所有改动可审计）。
- 落地页公开显示剩余名额（用户已知，如某活动不想暴露把上限设大）。

### Phase A 设计要点（给接手者）

- 路由顺序在 `decideRoute`：总开关关 → 机构租户 → 租户/用户强制 standard → 赠送余额（且 `gift_training_discount` 关）→ 租户/用户强制 training → 用户开关。`GiftFunded` 与账号决策独立：只要有赠送余额就赠送先扣、标准价。
- 付费路由且打折的记录 `preferPaid=true`：先扣充值赠送、钱包，付费不够才动赠送（避免用赠送买折扣价）。
- 会话结束退差额：`RefundRouteDiscount(userID, "speechmatics:<connID>:")`，按 `usage_logs.idempotency_key LIKE prefix%`，退 (charge−gift_usd) × 叠加折扣，`route_discount_refunds` 保证一次。
- 前端 `account.route` 显示原因；`training_program_available` 现在等于总开关 && 有 no-training key。

## 未开始的部分（管理员后台需求）

### B. 后台参数补齐
- **一次性兑换码**（需求里"邀请码：批量生成、面值、有效期、来源渠道标签、作废"是一次性码，和现有多人共用的渠道码不同）：
  新表 `redeem_codes(id, code UNIQUE, batch_id, channel, tags JSONB, face_value_usd, grant_days, expires_at, agent_user_id NULL, redeemed_by UNIQUE NULL, redeemed_at, voided_at, created_by)` +
  `redeem_batches`。用户端 `POST /api/user/redeem {code}` → `AddGrant(kind promo, funding gift)` + 归因；
  管理端批量生成/列表/作废（`/api/admin/redeem-codes`）；账户面板加"兑换码"输入框（文案：赠送额度不适用训练计划折扣，按标准价计费）。
  代理的码就是 `agent_user_id` 非空的兑换码。
- **涉及钱的参数二次确认**：服务端对 plans / topup-tiers / markup / model-cost / cost-overrides / settings 里的金额键要求
  `X-Admin-Confirm: true`（否则 428 + `code: confirmation_required`），前端 `adminFetch` 捕获 428 弹确认框后重试。SettingsPage 已有前端确认弹窗可复用。
- **审计日志页**：`GET /api/admin/audit?page=&search=` 读 `admin_audit_logs`（已有表，`auditTx` 已写租户/用户路由改动；设置、目录、模型改动也写）。前端加「审计」标签。
- 一键停用账户：用户页已有。

### C. 数据埋点（面板前置）
- 已有：`usage_logs.training_route/funding_route/gift_usd`；`payments.training_opt_in`；`training_opt_in_changes`。
- **转录延迟**：`speechmatics_proxy.go` 收到 Speechmatics `AddTranscript` 时，用消息 `metadata.end_time` 与首个音频字节转发时刻算 `now − (t0 + end_time)`，会话内收集，断开时写 `session_metrics(conn_id, session_id, user_id, latency_p50_ms, latency_p90_ms, samples, route, created_at)`。
- **修改次数**：`upsertTranscriptsWithStorageQuota`（store/postgres.go ~708）在 `ON CONFLICT (session_id, client_segment_id)` 时若旧行非 partial 且 text 变化，`edit_count = edit_count + 1`（加列）。
- **Stripe 手续费**：`payments.fee_usd` 列；webhook（handlers/billing_user.go ~509）RecordTopup 后用 stripe client 取 PaymentIntent.latest_charge.balance_transaction.fee（best effort）。
- **Speechmatics 额度**：system_settings `speechmatics_credit_usd`（$2,000 起点）+ `speechmatics_credit_started_at` + 记在哪个账号；消耗 = 该账号 `usage_logs.upstream_cost_usd` 自起点累计；"按当前速度还能用 X 天" = 剩余 / 近 7 天日均。
- 翻译 token 成本按语言：`usage_logs` 没有目标语言，需从 sessions.target_language join（session_id 存在时）。

### D. 数据面板与图表
- 后端 `GET /api/admin/dashboard?granularity=day|week|month&from=&to=&channel=` 一次返回：漏斗（注册→领码→首场会话→用满1小时→首充→二充，可按渠道拆）、留存（领码后第1/2/4周，按批次）、每人每周小时分布（直方图桶）、分流（训练/不训练、赠送/付费、个人/机构）、成本毛利（按天堆叠、四条路径 Free/Pro × 训练/不训练 的每小时毛利）、额度剩余、收入（按周、各档）、模型（延迟分布、修改率、翻译目标语言）。
- 前端无图表库；建议手写小型 SVG 组件（折线、柱/堆叠柱、面积、直方图、漏斗、进度条），每张图带标题+一句说明。首页概览：活跃人数、本周小时、本周收入、额度剩余天数、四条路径毛利，五个数五张小图。
- CSV 导出：前端由同一份数据生成 `<a download>`（真实应用可下载）。
- ProAdmin 现有 tab：概览/用户/注册风控/推广邀请/站内公告/会员与充值/模型与定价/组织/系统设置（`frontend/src/pro/ProAdmin.tsx`）。

### E. 角色权限与代理
- 表 `admin_roles(id, key, name, permissions JSONB, channels JSONB, builtin)`，`users.admin_role_id`；权限字符串如
  `dashboard.read / finance.read / promotions.write / pricing.write / routing.write / users.write / roles.write / export / agent.self`。
  内置角色：超级管理员（全部）、运营/营销（面板读 + 邀请码/渠道/营销参数写，不能改价格与路由）、财务（成本/收入/毛利/负债/分成 只读+导出）、
  技术（模型/分流/延迟/错误 读 + 路由与 Speechmatics 账号设置写，不看单个用户账单）、代理（只看自己的码）。
- 中间件 `RequirePermission(perm)` 替换现在几乎全是 `superAdminRequired` 的挂法（`cmd/web/main.go` ~585-700）；`super_admin` 保留为全权。
- 渠道限定：角色 `channels` 非空时，推广邀请/兑换码/漏斗按 channel 过滤（营销人员只看小红书）。
- 代理：`agent_profiles(user_id, commission_percent, settle_threshold_usd, status)`；`agent_settlements(id, agent_user_id, period, amount_usd, status requested/approved/paid, requested_by/reviewed_by/paid_by, 时间)`；
  刷号规则 `agent_fraud_rules`（自推自：注册邮箱/设备哈希与代理相同；最低使用时长）命中写 `agent_flags(code_id, user_id, reason)`，代理页可见原因。
  代理门户：登录后台只看自己一块（码、注册/首充/12 个月累计充值、分成本期/已结/待结、汇总留存与用量），能生成自己的码（数量面值由管理员设）、申请结算。硬规则：代理带来的用户价格/赠送/条款与正价完全一致。
  结算流程：代理申请 → 财务审核 → 超级管理员付款；低于门槛用额度结，高于用现金，门槛在后台可调。
- 每个账户一个人、所有改动写 `admin_audit_logs`。

## 本机环境提示（见 memory）
- 测试库：`docker start dreamtrans-test-pg`；应用迁移：`docker exec dreamtrans-test-pg rm -rf /tmp/migrations && docker cp backend/migrations dreamtrans-test-pg:/tmp/migrations && docker cp scripts/migrate.sh dreamtrans-test-pg:/tmp/migrate.sh && docker exec -e PGHOST=localhost -e PGUSER=test -e PGPASSWORD=test -e PGDATABASE=dreamtrans_test -e MIGRATIONS_DIR=/tmp/migrations dreamtrans-test-pg sh /tmp/migrate.sh`
- Go 测试：`DREAMTRANS_TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55432/dreamtrans_test?sslmode=disable' go test -race ./... -count=1`
- 新迁移要同步 `scripts/migrate.sh` 的 `expected_latest_prefix`，并跑 `scripts/tests/migration_bundle_test.sh`。
- Playwright 前 `unset DISPLAY`，不要打开报告/浏览器；`gh` 未登录，CI 结果要用户自己看。
- 只暂存自己的文件，提交后立即推送。
