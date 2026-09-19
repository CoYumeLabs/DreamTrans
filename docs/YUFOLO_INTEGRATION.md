# Yufolo 账号与实时转录

YuAction 直接复用 DreamTrans 已有的登录、会话、转录代理与归档接口，不读取或复制其 JWT 签名密钥。本版 AI 集成需要同时更新 Yufolo，以提供独立问答能力标记；旧 Yufolo 会显示更新提示。共享部署仍然使用同一 PostgreSQL 实例中的 `yuaction` schema，账号、余额和转录档案由 Yufolo 管理。

## 使用流程

1. 安装到 DreamTrans 目录下时，安装器发现运行中的 `app` / `dreamtrans` 容器后自动设置 `YUFOLO_URL`；其他部署在 YuAction `.env` 中手动填写后端可访问的 Yufolo 地址。
2. 使用 HTTPS（本机测试可用 localhost）打开 YuAction，以已有 Yufolo 邮箱和密码登录。
3. 创建活动，只选择原文语言（可选自动中英混合），点击「开始转录」并授权麦克风。首次启动自动创建关联的 Yufolo 会话。
4. 听众扫码加入，无须登录或开启麦克风，即可同步收到原文，并在「我的译文语言」选择自己的目标语言。
5. 「暂停转录」发送结束音频信号并等待最后的识别结果；再次开始沿用同一会话。「结束活动」同时结束 Yufolo 会话。

转录沿用 Yufolo 识别代理；参与者译文调用 Yufolo AI 翻译，使用主持人的余额与服务端模型。翻译默认关闭，同一字幕和目标语言只生成一次，结果持久化后共享。每个活动同一时间只允许一个主持端采集音频；听众不会建立额外识别连接。

中文普通话使用与 DreamTrans 一致的 `cmn` 语言代码（见 [Speechmatics 支持语言](https://docs.speechmatics.com/speech-to-text/languages)）。旧版 YuAction 保存或提交的 `zh` 会自动兼容为 `cmn`，原活动和关联会话继续使用，无须删除重建。这项代码兼容本身不需要更新 Yufolo，但新版 AI 集成需要。

「自动（中英混合）」使用 `cmn_en` 双语识别包，不是任意语种检测。其他语种请手动选择；参见 [Speechmatics 模型说明](https://docs.speechmatics.com/speech-to-text/models)。

字幕参考 Yufolo 的句子合并策略：按说话人、停顿、标点、时长和阅读长度合并短片段；识别流结束或字幕稳定约四秒后才提交译文，避免逐词翻译和重复显示。此规则适用于新录音，旧版本已保存的碎片不自动重写。

## 登录与权限

- 浏览器只持有随机的 HttpOnly、SameSite=Lax 会话 Cookie；通过受信任 HTTPS 代理访问时带 Secure。Yufolo access/refresh token 保存在 YuAction 后端内存中，按需刷新，不返回浏览器，不写入活动公开数据。
- 本地登录最长七天。后端重启后需要重新登录；活动归属和关联会话持久保存在数据库，登录同一账号即可恢复活动列表与主持权限。
- 主持操作验证 Yufolo 用户状态，录音期间每 20 秒复查。其他账号不能使用主持密钥绕过已绑定活动的所有权检查。
- 退出登录关闭该登录会话的录音连接，并撤销本地登录与上游刷新凭证。变更请求和音频 WebSocket 校验同源。
- 当前支持邮箱密码登录，不包含 OAuth 或跨域免登录。旧活动仍保留主持密钥校验；登录并关联转录后归属当前账号。

## 复用的 DreamTrans 接口

| 用途 | 已有接口 |
| --- | --- |
| 登录、刷新、退出 | `/api/auth/login`、`/api/auth/refresh`、`/api/auth/logout` |
| 验证账号 | `GET /api/user/profile` |
| 幂等创建会话 | `POST /api/sessions`，使用预先持久化的 UUID `client_session_id` |
| 音频代理 | `/ws/speechmatics?session_id=…`，后端携带用户 Bearer token |
| 幂等归档与译文更新 | `POST /api/sessions/{id}/transcripts`，使用稳定的 `client_segment_id` |
| 生命周期同步 | `PATCH /api/sessions/{id}` |
| 参与者译文 | `/ws/translate`，原子请求 ID；按字幕与语言缓存 |
| 资料与知识库 | `/api/ai/projects`、项目 `/sources`、`/api/ai/index/preview`、`/api/ai/index/jobs` |
| 通用与资料回答 | `/api/rag/ask`，`stateless: true` |

音频由 AudioWorklet 转为单声道 PCM16，以浏览器实际采样率发送。服务端生成 `StartRecognition`，可选 `translation_config`；暂停发送包含实际音频帧数的 `EndOfStream`，等待 `EndOfTranscript`。协议参考 [Speechmatics 实时转录文档](https://docs.speechmatics.com/api-ref/realtime-transcription-websocket)。

## 保存与恢复

- 收到最终原文后先保存到 YuAction 并广播，再归档到 Yufolo。后续微片段更新同一个字幕 ID；个人语言译文保存到 YuAction 的语言映射，由 Yufolo 处理翻译与计费。旧 API 原生译文仍按时间范围匹配。
- 归档失败会停止录音并提示。已持久保存的最终字幕保留在活动中；下次开始先补存尚未归档的字幕，成功后才建立新的付费识别连接。结束活动时也会补存，失败则保留活动并提示重试。
- 意外断线显示为中断，由主持人手动恢复，不自动反复开启付费连接。浏览器关闭页面会释放麦克风；服务重启后不会把失效连接显示为正在录音。
- 这里只保证已经持久保存的最终字幕可补存。断线时尚未收到的识别结果、尚未匹配原文的待处理译文不能保证恢复；不保存音频。旧 API 的原生跨段译文仍归到重叠最多的一段；新的参与者译文按完整字幕 ID 对齐。
- 新增版本化迁移为 `rooms` 增加私有 `integration` 列，并记录迁移版本。旧活动保留；共享安装不修改 DreamTrans 的业务表。

## 部署范围与验证

当前使用单个 YuAction 后端实例。会话缓存、录音互斥与 SSE 广播在进程内，不能直接增加后端副本。每活动上限为 5,000 段字幕、200 个实时订阅连接。

测试包含 Go race、PostgreSQL 持久化与权限隔离、原生协议模拟、浏览器麦克风采集与观众同步、归档失败后的补存，以及隔离运行的真实 DreamTrans 登录、创建会话、字幕更新和结束会话接口。自动化测试不调用真实付费识别服务，不代表已经验证真实语音的识别质量。

## 可选外部字幕接入

原有 `POST /api/internal/rooms/{code}/segments` 接口继续保留，要求部署级 `YUFOLO_INGEST_KEY`，接受稳定 `id`、最终 `text` 和可选 `translation`。同 ID 同内容可重试，冲突内容返回 409；密钥不能放到浏览器。内置麦克风转录不依赖此接口或密钥。
