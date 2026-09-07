# 全项目功能与描述一致性审计（2026-09-07）

审计基线：`e27a0e1`，及仅修正 E2E 标题定位的 `48f1c19`。本报告记录发现，不表示这些问题已经修复，也不表示线上已经部署这两个提交。

结论：发现 **15 项问题：6 项 P1、8 项 P2、1 项 P3**。主要风险集中在训练授权、计费完整性、异步任务恢复和账户删除。批量、兑换码、动态后台等有实际实现；不能把“已经有入口”“测试通过”理解为每种异常和宣传口径都已闭环。

- P1：优先修复，涉及未按声明处理音频、漏计成本、奖励条件或资金/数据恢复。
- P2：正常或边界操作会遇到错误口径、结果不一致或管理功能失效。
- P3：过时文档与维护问题。
- 证据等级：“运行复现”指在隔离测试数据库或本地桩中执行；“代码确认”指已核对完整相关调用链，没有声称真实供应商或生产环境已复现。

## 范围与方法

对照首页、邀请落地页、中英文界面、套餐配置、隐私政策/条款、用户指南和运维文档，检查实时/批量转录、余额暂停、充值退款、训练路由、云端历史、AI 助手/学习空间、Moodle 扩展，以及新后台的权限、渠道范围、审计、数据面板和代理结算。

执行了 8 个临时审计探针，覆盖 8 项问题；代码、命令和观察输出保存在 [复现附件](./product-consistency-2026-09-07-evidence.md)。探针以“确认现有错误行为”为断言，**PASS 代表问题复现成功，不能作为功能验收通过**。临时测试文件已移出生产测试目录。

本次未访问真实用户音频、未操作生产数据库、未进行真实扣款/银行付款，也没有登录 Speechmatics 控制台验证两把密钥的训练开关。接口桩 E2E 与代码审计不能证明供应商、邮件和生产配置已经可用。

## 问题清单

| ID | 级别 | 问题 | 证据 |
|---|---|---|---|
| A01 | P1 | 用户拒绝训练仍可被管理员设为训练路由 | 运行复现 |
| A02 | P1 | 训练计划暂停后重开自动恢复旧同意，与条款相反 | 运行复现 |
| A03 | P2 | 中途更改训练选择会影响本次退差额，文案彼此冲突 | 运行复现 |
| A04 | P2 | 自动充值阈值只是保存，没有触发作用 | 运行复现 |
| A05 | P1 | 首次转录奖励在预扣时发放，零实际用量退款后仍保留 | 运行复现 |
| A06 | P1 | 批量任务依赖浏览器查询完成退款/保存，无服务端恢复闭环 | 代码确认 |
| A07 | P1 | 删除普通账户会级联删除本地支付与账本记录 | 运行复现 |
| A08 | P2 | 创建代理资料后，后台删除该账户会被外键阻止 | 运行复现 |
| A09 | P2 | 中文“一键整理”暗示不扣额度，与英文及计费实现不一致 | 代码确认 |
| A10 | P2 | 套餐“保留天数”可编辑，但没有按期限清理的实现 | 代码确认 |
| A11 | P2 | 会话费用遗漏批量关联，且未扣会话结束退差额 | 代码确认 |
| A12 | P2 | Moodle 文档称不导入讨论区，代码会按名称导入部分论坛 | 代码确认 |
| A13 | P3 | 当前用户指南、状态文档与实现明显脱节 | 代码确认 |
| A14 | P2 | 单密钥部署也可能显示“所有录音走未开启训练账户” | 代码确认，真实密钥设置待核实 |
| A15 | P1 | Speechmatics 内置翻译没有进入实时计费及成本统计 | 本地桩复现及代码确认 |

### A01 · P1 · 管理员训练路由覆盖用户拒绝

**声明：** 隐私政策明确表示只有用户主动加入才会送往开启训练的账户，拒绝或未选择一律走非训练账户。证据：[隐私政策](../../frontend/src/legal/documents.ts#L153)、[条款](../../frontend/src/legal/documents.ts#L429)。

**实现：** `decideRoute` 在读取用户 opt-in 前先处理租户/用户的 `training` 固定路由。在计划开启、非机构、没有受保护赠送余额时，`training_opt_in=false` 仍返回 `Training=true`。管理员分流 API 可以写入这个值。证据：[routing.go](../../backend/internal/billing/routing.go#L95)、[console_routing.go](../../backend/internal/handlers/console_routing.go)。

**复现：** 用户明确拒绝、管理员固定训练路由 → `training=true, opt_in=false`。这不是“只少给优惠”，而是改变了声明中的音频提交账户。

**建议：** 将有效用户授权设为训练路由的必要条件；管理员可以限制为标准路由，不能代替用户同意。对已有被固定为训练的拒绝用户进行排查。这里判断的是产品承诺与代码不一致，不替代法律意见。

### A02 · P1 · 暂停/重开未重新征求选择

**声明：** 条款写着“暂停后所有已加入用户自动退出，重新开放时需重新选择加入”。证据：[条款第 7 节的计费说明](../../frontend/src/legal/documents.ts#L459)。

**实现及复现：** 设置只更新 `system_settings`，没有清空 `users.training_opt_in`。按 true → 暂停 → 重开操作，不执行任何新的用户确认，路由就重新变成训练。证据：[SetSystemSettings](../../backend/internal/billing/billing.go#L292)、[训练开关](../../backend/internal/billing/routing.go#L64)。

**建议：** 用计划版本与同意版本记录新一轮授权，或在暂停时一致地清除同意并保存审计。不能仅隐藏弹窗。交接文档曾列为未做，但当前对外条款已经作了承诺，因此仍是有效问题。

### A03 · P2 · 本次退差额受到中途设置变更影响

**声明：** 设置与隐私政策说修改“只影响之后开始的录音”；计费条款又写按结束时“当时的选择”退款。这两处本身就在冲突。证据：[设置中文](../../frontend/src/i18n/zh-CN.ts#L546)、[隐私政策](../../frontend/src/legal/documents.ts#L157)、[计费条款](../../frontend/src/legal/documents.ts#L459)。

**实现及复现：** `RefundRouteDiscount` 重新读取当前 opt-in、管理员分流、活动优惠和折扣比例，没有使用开始时的完整优惠快照。已加入用户以 $0.10 赠送余额开始，标准价扣 $0.645，结束前退出，付费部分 $0.545 的退款从原选择对应的 $0.109 变为 0。证据：[routing.go](../../backend/internal/billing/routing.go#L229)。

**建议：** 明确“路由”和“价格”各自的生效时点，并统一全部文案；建议将开始时应得优惠快照随会话保存，结算不再受中途修改影响。若坚持按结束时选择结算，必须明确告知这一例外。

### A04 · P2 · 自动充值阈值无效，“录音不中断”过度承诺

**声明：** 账户面板提供“余额低于”金额，说明会低于阈值自动充值；首页宣称“录音不中断”。证据：[中文文案](../../frontend/src/i18n/zh-CN.ts#L653)、[首页权益](../../frontend/src/i18n/zh-CN.ts#L880)。

**实现及复现：** 阈值只存入数据库；实际仅在一次预扣不足时调用充值。钱包 $9、阈值 $10、Pro 自动充值启用，一次成功扣费后回调次数仍为 0。证据：[SetAutoTopup](../../backend/internal/billing/accounts.go#L527)、[autoTopupRequest](../../backend/internal/billing/ledger.go#L106)、[RecordUsageBatch](../../backend/internal/billing/ledger.go#L591)。

**影响：** 用户设置的预留余额不起作用。支付失败或需要额外验证仍会导致暂停，“不中断”也不能保证。配置只验证 Stripe customer ID 存在，没有在此确认仍有可用的保存卡。

**建议：** 实现过阈值触发、并发去重和失败状态；文案改为说明尝试自动补款，失败时暂停并提示。

### A05 · P1 · 首次转录奖励可在零实际用量下获得

**声明：** 邀请文案将奖励条件表达为完成首次转录。证据：[邀请奖励文案](../../frontend/src/i18n/zh-CN.ts#L115)。

**实现：** `RecordUsageBatch` 成功预扣后立即调用 `awardSessionMilestoneAfterUsage`。实时代理在转发 `StartRecognition` 前就预扣第一个 **5 秒**窗口，此时可能还没发送任何音频。退款路径不会回收已发的首次转录奖励。证据：[奖励钩子](../../backend/internal/billing/ledger.go#L602)、[奖励函数](../../backend/internal/billing/promotion.go#L184)、[初次预扣](../../backend/internal/handlers/speechmatics_proxy.go#L832)。

**复现：** 已验证的活动注册用户，预扣 5 秒后全部退款，实际剩余用量为 0，但 `session_rewarded_at` 已填写，奖励仍保留。失败的批量提交也经过相同预扣钩子。

**建议：** 将奖励建立在成功结算且实际有效时长达到明确门槛之后；处理失败/全退款情况，并使用数据库幂等约束。已有注册风控不能弥补里程碑条件本身过早。

### A06 · P1 · 批量任务缺少服务端补偿和找回入口

**实现：** 浏览器 localStorage 保存 job ID；前端每 3 秒调用状态接口，再保存文字。失败退款与成功结算也在状态请求中完成。服务端虽然持久保存 job 所有权，但没有用户任务枚举 API、补偿轮询或后台结果入库任务。证据：[BatchTranscribePanel](../../frontend/src/unified/components/BatchTranscribePanel.tsx#L14)、[状态与退款](../../backend/internal/handlers/batch_transcribe.go#L316)、[作业持久化](../../backend/internal/store/postgres.go#L815)。

**触发：** 提交成功后关闭浏览器，等待上游失败，再清除站点数据或换设备。原浏览器不再查询，预扣不能自动退回，用户也没有任务列表可以找回。响应丢失形成的 `uncertain` 状态没有可自行完成的处理入口。正常关闭后回到同一浏览器且本地记录完好，已有恢复功能是有效的。

**建议：** 建立服务端状态机、租约轮询/回调、可重试结算和结果保存；提供按账户列出任务与重试 API；用客户端请求 ID 找回提交结果不确定的作业。不得对不确定提交直接再次付费重传。

### A07 · P1 · 删除账户会抹掉本地财务历史

**声明：** 隐私政策说计费账本与发票相关记录按财务、税务、争议处理所需期限保留。证据：[保留说明](../../frontend/src/legal/documents.ts#L209)。

**实现及复现：** 用户删除触发删除个人 `billing_accounts`，而 `payments`、`balance_transactions` 通过 CASCADE 删除。创建一笔 $10 的测试 Stripe 充值后删除用户，该 `stripe_object_id` 对应本地支付记录数量变为 0。证据：[删除触发器](../../backend/migrations/023_wallet_membership.sql#L103)、[payments 外键](../../backend/migrations/023_wallet_membership.sql#L160)、[后台删除调用](../../backend/internal/store/postgres.go#L1651)。

**影响：** 本地收入报表和追溯历史改变，也影响之后到达的退款事件匹配。这不表示 Stripe 自身的交易记录被删除；问题在应用本地账本。

**建议：** 拆分可删除身份资料与需要保留的最小财务记录；用匿名化所有者/墓碑记录维持引用，避免删除账户造成收入消失。具体保留时限另按实际运营要求制定。

### A08 · P2 · 代理账户删除入口无法完成

**实现及复现：** 只需创建 `agent_profiles`，即使还没发码、没结算，删除用户也会报 PostgreSQL `23503` 外键冲突。代理关联付款也可能阻止其客户的删除。后台删除路径没有相应归档流程或业务错误说明。证据：[048 迁移](../../backend/migrations/048_agent_settlements.sql)、[删除用户](../../backend/internal/store/postgres.go#L1554)。

**建议：** 保留结算历史是合理需求；应提供停用/归档与身份去标识化流程，明确告知不能硬删除的原因。不要以级联删除佣金记录来“修复”这个问题。

### A09 · P2 · 中文“一键整理”存在收费误导

**声明：** 中文为“点一下后台生成，可继续对话；不会自动扣额度”，英文则为“Nothing runs automatically”，表达的是不会自行运行，两者含义不同。证据：[中文](../../frontend/src/i18n/zh-CN.ts#L709)、[英文](../../frontend/src/i18n/en.ts#L709)。

**实现：** 点击生成通过 `requestAction` 执行模型请求；使用平台模型时经过 RAG 计费器预扣和结算，并非免费。已有索引、无需索引确认时可以直接生成。证据：[生成按钮](../../frontend/src/unified/components/AssistantPanel.tsx#L1186)、[计费上下文](../../backend/internal/handlers/rag.go#L1638)、[RecordUsage](../../backend/internal/handlers/rag.go#L3224)。

**建议：** 改成“仅在你点击后生成，按模型用量计费；可在后台完成”，并统一中英文。不要把“不会自动运行”翻成“不会扣额度”。

### A10 · P2 · 套餐保留期限是无效配置

**实现：** 后台显示“保留 N 天”，允许保存 `retention_days`；全仓库该字段只用于配置、校验和返回，没有按它清理会话的任务。隐私政策反而正确说明目前不按时间自动清理。证据：[套餐界面](../../frontend/src/pro/admin/PlansPage.tsx#L284)、[编辑项](../../frontend/src/pro/admin/PlansPage.tsx#L371)、[Plan](../../backend/internal/billing/plans.go#L28)、[隐私政策](../../frontend/src/legal/documents.ts#L210)。

**建议：** 在实现到期提醒、删除策略、导出与保留例外前，禁用此输入并标明未生效。公开套餐 API 也不应把它作为已兑现权益传播。目前首页没有实际展示这一权益，不能说用户购买页已普遍宣传它。

### A11 · P2 · 历史会话费用与实际账本不一致

**批量关联缺失：** 批量预扣/结算记录没有 `SessionID`，前端后来创建云端会话时也没有把 job 与 usage 绑定。因此历史的按 session ID 汇总无法读到批量费用，显示为没有费用标记，而非证明该转录免费。证据：[批量结算](../../backend/internal/handlers/batch_transcribe.go#L885)、[结果保存](../../frontend/src/unified/workspace/batchTranscription.ts#L81)、[历史展示](../../frontend/src/unified/components/HistoryPanel.tsx#L155)。

**退差额遗漏：** 会话汇总只 `SUM(charge_usd)`，没有减 `route_discount_refunds`；新的管理数据面板已经减了，两处统计口径不同。证据：[GetSessionCostSummaries](../../backend/internal/billing/analytics.go#L300)、[管理汇总](../../backend/internal/handlers/console_dashboard.go#L27)。

**建议：** 服务端保存批量任务与会话的稳定关联；抽取共享净费用计算，区分预扣、实际消耗、赠送支付和退款。用“跨赠送/钱包并退差额”和“批量完成”两类数据验收。

### A12 · P2 · Moodle “讨论区不同步”的声明不准确

**声明：** 扩展 README 写讨论区、提交物、成绩不同步。

**实现：** 名称匹配 `announce|news|公告|notice` 的 forum 会进入抓取流程，将 `.discussion`、discussion-list 行或 `.forumpost` 抽成文本上传。该判断依赖名称，不是验证 Moodle forum 的真实类型。证据：[README](../../extension/README.md#L9)、[分类规则](../../extension/src/content/discovery.ts#L36)、[forum 抓取](../../extension/src/content/fetcher.ts#L130)。

**触发与影响：** 一个名称带“公告”的论坛会被同步；普通论坛也可能因命名匹配被当作公告。是否包含发帖人信息取决于页面结构，本次未访问学校真实站点。

**建议：** 显式区分公告与讨论区，使用可靠类型信息和预览选择；文档说明到底导入哪些信息，避免用标题关键字作隐私边界。

### A13 · P3 · 用户指南与“当前状态”文档过时

- [USER_GUIDE](../USER_GUIDE.md#L123) 仍写 AI 助手只有“对话/会话摘要”两个标签、只读旧 RAG 摘要；当前是对话、一键整理、资料库，并有主动生成流程。
- [PROJECT_STATUS](../PROJECT_STATUS.md#L12) 仍把直连 token 当主架构、把 `/ws/translate` 称为未来占位接口；实际默认走可计费代理，翻译已实现，旧 token 路径默认禁用。
- [README](../../README.md#L148) 的 AI 预估仍写 DP，当前为 USD。
- [旧架构文档](../PROJECT_ARCHITECTURE_AND_STATUS.md#L46) 给出 provider 默认 50051；[实际 provider](../../backend/cmd/pcas-provider/main.go#L36) 为 50052，`GRPC_PORT` 仍有效，绑定地址另由 `GRPC_BIND_ADDR` 控制。不要混淆 provider 端口与 PCAS 上游默认 50051。

**建议：** 给历史方案加显眼归档标记和替代链接，更新唯一当前用户指南；维护“界面文案 → 配置 → 实现 → 验证”功能清单，减少多个状态文档分别声称当前有效。

### A14 · P2 · 单密钥部署的非训练提示没有依据

**实现：** 没有 `SM_API_KEY_NO_TRAINING` 时，训练计划不可用，账务路由原因为 `program_off`；设置页仍根据该原因显示“所有录音走未开启训练的账户”。实际代理只会使用唯一 `SM_API_KEY`，其训练设置不能由代码判断。证据：[训练计划可用性](../../backend/internal/billing/routing.go#L64)、[密钥选择](../../backend/internal/handlers/speechmatics_routing.go#L56)、[设置显示](../../frontend/src/unified/components/SettingsPanel.tsx#L398)、[中文状态](../../frontend/src/i18n/zh-CN.ts#L550)。

**范围：** 隐私政策已经写了单账户部署例外，因此这里是界面给出过强保证，不是断言所有单账户都训练。真实部署究竟训练与否需要核实供应商账户配置。

**建议：** 区分“运营方暂停双账户计划”和“未配置双账户”；后一种明确说明由部署配置决定，不能显示已验证为非训练。上线配置检查应核验真实供应商账户。

### A15 · P1 · Speechmatics 内置翻译漏计费、漏成本

**实现与复现：** 设置允许选择 Speechmatics 翻译，前端在 `StartRecognition` 携带 `translation_config`。代理仅解析音频格式，预扣/结算始终只写 `action=transcription, model=speechmatics-realtime-enhanced`；搜索得到的 `speechmatics-translation` 只有成本目录和定价映射，没有实时使用记录调用。本地桩验证带翻译配置能通过，初次预扣只产生一笔转录记录。证据：[客户端配置](../../frontend/src/unified/hooks/useUnifiedWorkspace.ts#L1844)、[代理解析](../../backend/internal/handlers/speechmatics_proxy.go#L86)、[唯一计费记录](../../backend/internal/handlers/speechmatics_proxy.go#L988)、[附加项成本](../../backend/internal/billing/catalog.go#L142)。

项目目录将该项单列为 $0.65/小时；审计日核对的 [Speechmatics 官方价格页](https://www.speechmatics.com/pricing) 也将 Translation 列为额外收费项。真实账户合同价可能不同，本次没有查看真实发票。

**影响：** 运营方承担未入账的上游附加成本；用户账单、后台毛利与供应商额度预测都可能失真。客户端还能提交已被代理接受、却未纳入报价的翻译配置；问题不只涉及默认 AI 翻译路径。

**建议：** 在发送上游前解析允许的附加配置，将转录与翻译费用原子预扣、按实际转发量结算，计入对应成本与额度；未实现收费的附加能力应明确禁用或明确为运营补贴且仍记录成本。

**相关边界：** 中途切到“学习”只改变界面和 AI 入队逻辑，没有关闭已经开启的 Speechmatics 内置翻译配置（[切换](../../frontend/src/unified/WorkspaceShell.tsx#L262)、[动态处理](../../frontend/src/unified/hooks/useUnifiedWorkspace.ts#L3270)）。上游仍可能处理译文，不能把界面变成学习模式当作附加成本已停止。此项未以真实供应商连接复现。

## 不应误报为“完全没实现”的部分

- Pro 批量入口、浏览器转 WAV、服务端测时预扣、job 所有权校验、同浏览器恢复及幂等云端文字保存已经存在。A06/A11 是生命周期和关联缺口。
- 余额不足暂停转录/AI 翻译，补款后明确恢复同一会话有完整 E2E，不能再描述为余额不足仍无限录制。
- 兑换码、后台涉钱确认、角色/渠道权限、审计列表、图表/CSV、代理结算都有实际接口和页面。检查了普通用户动态角色、撤权和渠道 SQL 限制；本轮没有确认可任意越权读取其他渠道的漏洞，这不等于覆盖了所有权限组合。
- “高级模型权限”“自定义提示词权限”“账单导出权限”“API access”仍有目录开关尚未接入，但后台已有“未实现”标注，首页/购买权益已过滤。不能把公共自定义提示词或账单导出误称为 Pro 专属。
- 代理现金结算是人工转账后登记凭据，文档已明确应用不发起银行付款。代理门户/后台历史窗口、未知 Stripe 手续费用 NULL、Speechmatics 额度为估算而非实时供应商余额，也已有说明。
- 学习路线、练习记录、课表归类、文件抽取/索引已有实现与测试；“上传材料”与“付费生成路线/题目”在现有文案中已经区分。
- Moodle 扩展的 Echo360 深度同步、slides 对齐、通知、VLM 描述在扩展 README 明确列为未做。这里不把设计稿中的计划自动视为已上线宣传。

## 部署与进一步验证

1. 生产是否部署迁移 043–048、实际运行镜像 SHA、默认训练折扣是否确为 20%，本次未核验；本地迁移成功和推送成功不能代替部署成功。
2. 核实两把 Speechmatics 密钥属于预期账户、Model Training 开关与机构路由；代码中的变量名称不是供应商配置证明。
3. 核实 Stripe `charge.updated` 事件订阅、保存卡的可用性、手续费补全及真实退款；实际转账需要对外付款凭证。
4. Speechmatics ASR 延迟是服务器观测到的样本，不含浏览器全链路延迟；聚合的“会话分位数的分位数”不能当成全部句子的总体 P90。当前数据面板命名/说明已做区分，但运营解读仍需保持此口径。
5. 审计记录先持久写入意图，完成状态更新是请求上下文中的 best effort；异常断连时可能留在 started。需要根据审计要求决定是否增加可靠结果回填与前后值差异，不能把 started 当成操作失败。
6. 大文件、低内存手机、真实浏览器回收存储、长时 WebSocket、真实 Moodle 站点和邮件投递，需要独立环境验收。本次没有把这些列为已通过。

## 建议修复顺序与验收

1. **训练承诺：** A01/A02/A14，随后统一 A03。拒绝、未回答、机构、暂停、重开、单账户部署全部纳入矩阵；同时验证上游密钥和账务选择。
2. **资金链路：** A15/A05/A04。内置翻译必须有可追溯的成本记录；零有效用量不能领“完成首次转录”奖励；阈值、并发充值、失败暂停分别验收。
3. **生命周期与账本：** A06/A07/A08/A11。关闭浏览器后仍能结算，换设备可找回任务；删除身份不会抹掉应保留的财务记录；所有会话费用与账本净额可核对。
4. **描述与入口：** A09/A10/A12/A13。统一中英文收费说明，禁用未生效配置，澄清扩展导入范围，归档旧状态文档。

本次审计不批量改动上述业务行为；已立即修复的是前一提交远端 CI 中代理页面测试的重复标题定位问题。业务修复应按上述组别分别提交并执行受影响部分的完整 CI。

## 提交与验证记录

- 后台功能提交 `e27a0e1` 已推送。该提交本地前端完整验证曾通过，但远端因代理页面 E2E 同时匹配两个标题而失败；不能记为远端通过。
- 测试定位修复 `48f1c19` 已推送：Node 24.18.0，完整 `verify:ci` 47 个 E2E 通过；[对应远端 CI](https://github.com/CoYumeLabs/DreamTrans/actions/runs/34131902893) 全部成功，包含后端、完整前端、镜像/运行时和安全检查。
- 前一功能提交本地后端完成 gofmt、依赖、event_worker 标签 lint、隔离数据库迁移、完整 race、event worker race 与安装/备份生命周期检查；本次审计未修改后端业务代码。
- 本报告与复现附件为文档提交，检查本地链接目标、问题级别计数和 diff 空白；没有把文档复核描述为重新执行所有业务测试。
