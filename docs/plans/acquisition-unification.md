# 拉新来源统一（2026-09-09）

## 问题

9 月 6 日到 7 日分三次做了三套"带来一个用户"的机制：渠道推广链接（promotion_invites /
promotion_registrations）、用户推荐（users.referral_code / referrals）、代理与一次性兑换码
（redeem_batches / redeem_codes / agent_*）。三张归因表、两条发放赠送额度的路径、两份渠道标签，
概览漏斗要手工合并归因，代理没有链接只能靠用户注册后输码，活动赠送和兑换码赠送可以叠着领。

## 模型

一个**来源**（source）实体承载全部三种情况；`promotion_invites` 表升级为来源表，不再新建表。

| 字段 | 含义 |
|---|---|
| `kind` | `campaign` 渠道活动 / `referral` 用户推荐 / `agent` 代理 |
| `owner_user_id` | 推荐和代理的归属人；活动为空 |
| `claim_mode` | `link` 链接注册即归因 / `code` 只能凭一次性码归因（老"兑换码批次"） |
| 奖励字段 | 沿用：`grant_usd`、`grant_days`、`plan_code`、折扣、首充加赠、里程碑 |
| `channel`、`tags`、`expires_at`、`max_registrations`、`enabled`、文案 | 沿用 |

- **一次性码**：`redeem_codes.invite_id` 指向来源，任何来源都可以生成码；码只是领取方式，
  面值和有效期来自来源，不再有 `redeem_batches`。
- **归因**：只有 `promotion_registrations` 一张表，`user_id` 唯一，即一个账户只归因一次。
  凭码领取的记录带 `code_id`。已被归因的账户再兑码会被拒绝，赠送不再叠加。
- **奖励**：只走 `GrantPromotionRewards` 一条路径（含风控预留）。兑码 = 写归因 + 走同一发放。
- **推荐**：每个用户懒创建一个 `kind='referral'` 的来源，代码即原 `referral_code`；`referrals`
  表和 `users.referral_code` 列删除。
- **代理**：`agent_profiles` 保留分成比例、结算门槛、发码限额；保存代理资料时自动创建一个
  `kind='agent'` 的来源（面值 = `code_value_usd`），代理因此有落地页、二维码和 UTM 漏斗；
  代理发的码挂在这个来源下。分成、风控标记、留存都按归因表算。`agent_flags.code_id` 改为
  `registration_id`，链接和码带来的用户一视同仁。
- **访问**：`invite_visits.referrer_user_id` 并入 `invite_id`。

## 迁移 051

1. 来源表加 `kind`、`owner_user_id`、`claim_mode`；每个推荐人/持有 referral_code 的用户生成
   推荐来源；每个 `redeem_batches` 行生成一个 `claim_mode='code'` 的来源（有 agent 的为
   `agent` 类型，归属该代理）。
2. `redeem_codes` 加 `invite_id`，由批次映射填充；删 `batch_id`、`redeem_batches`。
3. 已兑换的码和 `referrals` 行写入 `promotion_registrations`（已有归因的账户不覆盖，先到先得）。
4. `agent_flags` 由码映射到归因记录；`invite_visits` 推荐访问映射到推荐来源；删旧列旧表。

## 行为变化（对外）

- 兑换码只能由**尚未归因**的已验证账户使用；文案改为"每个账户只能通过一种活动获得赠送"。
- 代理多了链接 `/invite?code=<代理来源码>`，通过链接注册的用户享受与码相同的赠送并归因给代理。
- 后台"兑换码"页保留（生成即创建一个凭码领取的来源），"推广邀请"页能看到每个来源的码。
- API 形状尽量不变：`/api/auth/invite` 对活动和代理返回 `kind: promotion`，推荐返回
  `kind: referral`；`/api/admin/redeem-codes`、`/api/user/redeem`、`/api/user/referral` 保留。
