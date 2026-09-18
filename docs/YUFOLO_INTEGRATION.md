# Yufolo 联动合同 · 草案

目标：YuAction 创建活动后，主持人可以创建 / 关联 Yufolo 转录房间；主持端只建立一路音频采集与上游识别，所有参与者订阅共享内容。

**当前状态**：YuAction 的接收和广播接口已实现。以下创建 / 控制 / 绑定 Yufolo 会话的流程尚未实现，也没有修改 DreamTrans 仓库。不能将它当成已存在的 Yufolo API。

## 已实现：最终字幕接收

```http
POST /api/internal/rooms/{code}/segments
Authorization: Bearer <YUFOLO_INGEST_KEY>
Content-Type: application/json

{
  "id": "stable-provider-segment-id",
  "text": "我们从观察真实需求开始。",
  "translation": "We start by observing real needs."
}
```

- 凭证仅在服务端保存，不能交给观众浏览器。当前是一把部署级集成密钥，持有者能够写入全部活动；正式对接时应替换为绑定房间、绑定 Yufolo 会话的凭证。
- `id` 在同一 YuAction 房间内唯一，同 ID、相同原文和译文可安全重试；冲突内容返回 409。
- `text` 必填、1–2,000 字符；`translation` 可选、最多 2,000 字符；仅接受最终文本，不接受中间识别结果。
- 服务端附加接收时间和 `source=yufolo`。这只是来源标记，不代表浏览器与 Yufolo 已经建立连接。
- 成功响应 200 返回最新公开房间快照。成功后，同房间 SSE 客户端收到更新。
- 401 表示集成凭证无效，404 表示房间不存在，409 表示活动结束 / ID 冲突 / 达到容量上限。
- 不记录音频，不调用模型，不进行计费。

演示接口 `POST /api/rooms/{code}/demo-segments` 仅在 `YUACTION_DEMO=true` 时开放，要求主持人密钥，统一标记 `source=demo`。

## 待实现：房间生命周期

1. 主持人在 YuAction 中授权连接 Yufolo 账号，后端校验两侧身份与活动管理权限。
2. YuAction 以活动 ID 为幂等键请求创建共享转录会话；保留两侧 ID 映射，不假设两者 ID 相同。
3. 主持人明确开始采集，Yufolo 对该房间建立唯一活动转录任务。并发点击 / 重试不能创建重复上游会话。
4. Yufolo 将最终字幕与共享译文通过受限集成接口发送给 YuAction；听众只读取，不建立自己的识别连接。
5. 暂停、余额不足、断网、恢复、结束的状态需要同步到整个房间；不应将意外断线显示为已结束。
6. 结束活动后停止采集、结算房间使用量并确定允许参与者保存的公开内容范围。

## 待实现：顺序与恢复

正式事件需要 `eventId`、`activityId`、`transcriptSessionId`、`sequence`、`speaker`、`startMs`、`endMs`、`sourceLanguage`、`translations` 和 `final`。按房间保存连续序列，重连使用游标补齐；字幕修订使用版本号，不复用当前仅针对最终内容的写入合同。

Yufolo 与 YuAction 之间需要 durable outbox / acknowledgement，防止临时网络故障丢失已识别内容。中间识别文本可以短暂广播，但不能混入最终归档。

## 产品约束

- 学生不需要为接收同一份字幕启动额外识别任务。
- 相同目标语言共享翻译任务；个人 AI 学习操作独立授权与计费。
- 老师看到的是公开提问与主动反馈；学生的私人笔记、AI 对话不因加入房间而共享。
- 共享内容是否可回看、保存到个人 Yufolo 空间，应由活动权限控制。
- 当前代码不自动启动任何付费转录服务。
