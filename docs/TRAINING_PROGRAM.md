# 训练计划、赠送额度与账号路由

原则：**没掏钱的余额一律按标准价、走未开启训练的账号。**

## 资金类别

- `grants.funding`：`gift`（试用、活动、邀请、人工调整、结算返还继承来源）或 `paid`（充值赠送 `topup_bonus`）。
- 扣费顺序：赠送 → 充值赠送 → 钱包。每条 `usage_logs` 记录 `gift_usd`（赠送部分）、
  `funding_route`（`gift`/`paid`）和 `training_route`（`training`/`standard`）。
- 按折扣价计费的会话（付费路由）优先用付费余额（充值赠送、钱包）；付费余额不足时暂停；不能拿新发的赠送额度抵扣训练折扣价。管理员明确开启赠送折扣时除外。

## 路由决策（`billing.RouteForUser`）

会话建立（实时连接、批量上传、旧版 token）时决定一次，整场不切换，顺序：

1. 训练计划总开关（`system_settings.training_program_enabled`，默认开）关闭 → 不训练。
2. 租户 `kind = institution` → 不训练（任何设置盖不过）。
3. 租户 / 用户 `speechmatics_route = standard` → 不训练；`training` → 训练。
4. 账户还有赠送额度且 `gift_training_discount` 关闭 → **赠送路由**：不训练、标准价、活动折扣也不生效。
5. 否则按用户的训练开关。

`GET /api/user/billing/account` 的 `route` 字段给出决策与原因，账户页与设置页显示。训练选择只在注册/新手引导与设置中展示，不再向老用户登录主动弹窗。批量预扣与上游提交使用同一次路由决定。

## 赠送额度用完后的退差额

赠送路由的会话结束（代理断开 / 批量任务结算）时调用 `RefundRouteDiscount`：
付费部分 = Σ(charge − gift_usd)，按用户当时应享的折扣（训练折扣 × 活动折扣叠加）退回钱包，
账本一条 `route_discount` 退款，`route_discount_refunds` 保证每场只退一次。

## 统计快照

- `promotion_registrations.training_opt_in_at_claim`：领取活动权益时的开关。
- `payments.training_opt_in`：每笔充值时的开关。
- `training_opt_in_changes`：每次修改。
- `GET /api/admin/training-program`：加入人数、领码/首充时开着训练的人数、首充后改过的人数、
  两个账号的小时数、赠送路由小时数、退差额总额。管理后台「设置」页展示。

## 后台开关

- `training_program_enabled`：总开关。关闭后前端隐藏训练内容、全员不训练、折扣为 0。
  重开时清零所有人选择的逻辑尚未实现（预留）。
- `gift_training_discount`：允许赠送额度享受训练折扣（默认关）。
- `training_discount_percent`：默认 20。
- 组织页可设置租户类型与强制账号；用户页可为单个账户强制账号（仅超级管理员）。
  这些改动写入 `admin_audit_logs`。

迁移：`044_training_funding.sql`。
