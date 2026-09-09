# 环境变量配置

建议从 [`backend/.env.example`](../backend/.env.example) 复制配置。生产环境
的 `.env` 应设为 `0600`，不要提交到 Git。

## Docker Compose

根目录 Compose 会读取项目根目录的 `.env`：

```bash
cp backend/.env.example .env
chmod 600 .env
docker compose up -d
```

主要部署变量：

```dotenv
# 主机监听；默认仅本机可访问
BIND_ADDRESS=127.0.0.1
PORT=16002
IMAGE_TAG=latest

# 必填
SM_API_KEY=...
POSTGRES_DB=dreamtrans
POSTGRES_USER=dreamtrans
POSTGRES_PASSWORD=...
JWT_SECRET=...
JWT_REFRESH_SECRET=...

# 新数据库建议同时设置；已有安全管理员时可留空
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=...
```

`POSTGRES_PASSWORD`、`JWT_SECRET`、`JWT_REFRESH_SECRET` 必须是三个不同的
随机值。JWT 两把密钥至少 32 字符；管理员密码至少 16 字符。项目没有
默认数据库密码、JWT 密钥或管理员账户。

一键安装器会自动生成这些密钥，并把与所选应用镜像完全一致的迁移包
从镜像中提取出来。`--update --tag <tag>` 会持久化新镜像标签。

## 后端运行时

### Speechmatics

```dotenv
SM_API_KEY=...
SM_API_KEY_NO_TRAINING=
BATCH_BILLING_RESERVATION_MINUTES=10080
ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=false
CLASSIC_TOKEN_BILLING_MINUTES=10
```

实时和批量转录都需要 `SM_API_KEY`。密钥只保存在服务端。

`SM_API_KEY_NO_TRAINING` 可选，是另一个 Speechmatics 账户的密钥，该账户应
关闭 Model Training。设置后即开启「训练计划」：

- 只有在设置或新手引导里明确加入计划的用户，其实时与批量音频才经
  `SM_API_KEY`（训练开启）提交，并享受转录折扣；折扣比例读取
  `system_settings` 里的 `training_discount_percent`，默认 30。
- 未回答、拒绝加入以及匿名调用一律走 `SM_API_KEY_NO_TRAINING`，价格不变。
- 批量任务会记住提交时用的账户，后续查询状态、取转录和删除都用同一密钥。
- PCAS gRPC 服务端集成和 event-worker 没有可以加入计划的用户身份，配置了
  `SM_API_KEY_NO_TRAINING` 时一律使用它，否则退回 `SM_API_KEY`。
- 留空则不提供计划，所有流量照旧走 `SM_API_KEY`，用户已保存的选择保留但
  不生效。
- `/api/system/access` 返回 `training_program_available` 和
  `training_discount_percent`，前端据此决定是否展示该选项。

数据库计费模式下：

- 实时客户端必须使用可测量音频字节并增量结算的 `/ws/speechmatics`。
  `/api/token/rt` 默认拒绝发放后端无法继续测量或限制复用的临时直连
  token。只有明确设置
  `ALLOW_UNMETERED_CLASSIC_TOKEN_WITH_BILLING=true` 才会恢复旧行为；
  `CLASSIC_TOKEN_BILLING_MINUTES` 的固定扣费并不代表真实用量，运营方需
  自行承担余额和上游成本风险。
- Pro 工作区的「批量转录」支持多文件队列。浏览器将可解码音频转成
  16 kHz 单声道 PCM WAV；`audio_format=pcm16` 提交由服务端验证完整
  WAV 结构和字节长度，按向上取整到秒的时长预扣，完成后结算实际用量。
  `/api/transcribe/batch/quote` 提供当前会员价格的只读预估。每批最多
  10 个文件，总计 100 MiB；转换后也受 100 MiB 限制。任务标识按账户
  保存在浏览器，重新打开会继续查询并幂等保存云端历史；未上传文件需重选。
- 旧客户端直接提交的压缩批量音频在提交前无法可靠推导时长。默认
  `BATCH_BILLING_RESERVATION_MINUTES=10080`，即先预留接口允许的最坏
  7 天，再在完成时用同一笔 reservation 按 Speechmatics 返回的真实时长
  原子结算并退回差额。账户余额（赠送额度 + 钱包）若无法覆盖该最坏
  预留会收到 402；这是安全拒绝。显式调低该变量可改善可用性，但意味着
  恶意伪装长音频可能让上游成本超过预留。

### OpenAI 兼容接口

```dotenv
OPENAI_API_KEY=
OPENAI_API_BASE=https://api.openai.com/v1
OPENAI_MODEL=gpt-5.6-sol
OPENAI_EMBEDDING_MODEL=text-embedding-3-small
OPENAI_FALLBACK_MODELS=gpt-5-mini,gpt-5-nano
OPENAI_DEBUG=0
OPENAI_USE_RESPONSES=true
OPENAI_PROMPT_CACHE=true
OPENAI_PROMPT_CACHE_TTL=1800
AI_MAX_CONTEXT_TOKENS=256000
AI_CONTEXT_OUTPUT_RESERVE_TOKENS=4096
AI_MODEL_CONTEXT_WINDOW_TOKENS=260096
```

未设置 `OPENAI_API_KEY` 且没有配置其他供应商时，AI/RAG 工作区会明确返回不可用，实时转录仍可独立
工作。自定义 API Base 只允许 HTTP(S)，服务端请求有超时、响应体上限和安全错误
处理。

### 多家 OpenAI 兼容供应商

```dotenv
AI_PROVIDERS=cerebras=https://api.cerebras.ai/v1;groq=https://api.groq.com/openai/v1
AI_PROVIDER_KEYS=cerebras=csk-…;groq=gsk-…
AI_PROVIDER_OPTIONS=cerebras=chat
AI_EMBEDDING_PROVIDER=openai-compatible
```

- `OPENAI_*` 注册的是名为 `openai-compatible` 的默认供应商，其模型保持原有的裸 ID；`AI_PROVIDERS`
  追加的供应商用 `名称=基址` 列出，名称只允许小写字母、数字和连字符，`openai-compatible` 与
  `speechmatics` 为保留名。每个追加的供应商必须在 `AI_PROVIDER_KEYS` 里有密钥，否则应用拒绝启动。
- 追加供应商的模型在目录、策略、用户偏好、用量和账单里一律写作 `名称::模型ID`（如
  `cerebras::llama-3.3-70b`），成本目录按供应商与模型分别录入，录入前不能审批。
- `AI_PROVIDER_OPTIONS` 每家可选 `chat`（默认）、`responses`、`cache`；追加供应商默认走 chat
  completions、不开提示缓存。回退模型只在同一供应商内生效。
- 嵌入只能走一家供应商（`AI_EMBEDDING_PROVIDER`，默认 `openai-compatible`），模型仍由
  `OPENAI_EMBEDDING_MODEL` 指定且必须是 1536 维。
- 设计说明见 `docs/plans/ai-multi-provider.md`。

### 计费与在线支付

```dotenv
STRIPE_SECRET_KEY=
STRIPE_WEBHOOK_SECRET=
APP_BASE_URL=https://dreamtrans.example.com
STRIPE_CURRENCY=usd
STRIPE_USD_EXCHANGE_RATE=
STRIPE_FX_MARKUP_PERCENT=0
STRIPE_FX_RATE_URL=
```

计费以美元记账：每个用户一个账户，账户里有会过期的**赠送额度**（注册
试用金、充值赠送、活动）和永不过期的**钱包余额**；用量按
`上游成本 × (1 + 加价) × (1 − 会员折扣)` 扣费，先扣赠送再扣钱包。会员
（`pro` 套餐）单独订阅，提供用量折扣、功能解锁和更高的存储/并发上限，
不包含小时数。成本表、加价、套餐、充值档位、试用金都在管理后台
（`/pro/admin`）配置。

- `STRIPE_SECRET_KEY` 未设置时，在线充值和会员开通接口返回 503，其余
  计费功能（余额、扣费、管理员手动赠送/调整）照常工作。
- `STRIPE_CURRENCY` 是 Stripe 收单币种（ISO 4217，默认 `usd`）。账本始终
  按美元记，`STRIPE_USD_EXCHANGE_RATE` 是 1 美元折合多少该币种（非 `usd`
  时必填），充值 $20 会按 `20 × 汇率` 收款，入账仍是 $20。微信支付、
  支付宝等本地支付方式要求用 Stripe 账户所在国家的币种收单（例如澳洲
  账户只能用 `aud`），这时需要设置这两项。付款时的汇率会记录在 Stripe
  metadata 中，之后调整汇率不影响旧订单的退款。不支持日元等无小数币种。
- `STRIPE_USD_EXCHANGE_RATE=auto` 时汇率自动获取：启动时和之后每小时从
  欧洲央行参考汇率（Frankfurter API，无需密钥）拉取 USD 兑该币种的牌价，
  再加上 `STRIPE_FX_MARKUP_PERCENT` 的安全余量（百分比，默认 0，建议 2
  到 3 覆盖当日波动）。参考汇率每个工作日更新一次。获取失败时沿用上一次
  的牌价，超过 7 天没有成功更新则暂停在线支付（结账接口返回 503，界面
  显示未开启在线支付），直到重新拉到牌价。`STRIPE_FX_RATE_URL` 可换成
  其他 Frankfurter 兼容的接口地址。
- `STRIPE_WEBHOOK_SECRET` 是 Stripe Dashboard 中为
  `POST /api/billing/stripe/webhook` 创建的 endpoint 签名密钥。需要订阅
  的事件：`checkout.session.completed`、`customer.subscription.created`、
  `customer.subscription.updated`、`customer.subscription.deleted`、
  `checkout.session.async_payment_succeeded`、`invoice.paid`、
  `invoice.payment_failed`、`charge.refunded`。每个事件
  只处理一次；处理失败时返回 5xx 让 Stripe 重试。
- `APP_BASE_URL` 是支付完成后浏览器返回的站点地址；未设置时按请求的
  `Host` 推导。
- 会员和充值都用 Checkout 的即席价格，不需要在 Stripe 后台预建
  Product/Price；若套餐配置了 `stripe_price_id_*` 则优先使用。

### 数据库和本地数据

```dotenv
# 仅在不使用 Compose、直接运行 Go 服务时设置
DATABASE_URL=postgres://user:password@127.0.0.1:5432/dreamtrans?sslmode=disable

RAG_DB_PATH=./data/rag.db
# SQLite 总磁盘预算（MiB，含主库和 sidecar）；仅在明确需要时用 -1 关闭
RAG_MAX_DB_MB=102400
DREAMTRANS_CONFIG_PATH=./data/dreamtrans.config.json
KNOWLEDGE_DATA_PATH=./data/knowledge
KNOWLEDGE_MAX_FILE_MB=50
KNOWLEDGE_MAX_EXTRACTED_MB=10
KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB=100
KNOWLEDGE_MAX_IMAGE_MEGAPIXELS=40
KNOWLEDGE_MAX_PDF_PAGES=100
KNOWLEDGE_EXTRACT_WORKERS=2
AI_INDEX_WORKERS=2
PORT=8080
```

`AI_MAX_CONTEXT_TOKENS` 是服务端输入预算上限。实际可读输入上限取
`AI_MAX_CONTEXT_TOKENS` 与
`AI_MODEL_CONTEXT_WINDOW_TOKENS - AI_CONTEXT_OUTPUT_RESERVE_TOKENS` 的较小值，
从而给模型输出保留空间。默认的 `260096 - 4096` 仍提供 `256000` 输入预算；若
自定义模型窗口更小，必须同步调低输入预算或输出预留，并确保输出预留小于模型窗口。
无效值会回退到默认值；如果输出预留已经占满模型窗口，实现会把可用输入降到
1 token，使正常请求明确失败，而不会冒险溢出模型窗口。

用户可在 16K、64K、128K 和 256K 之间选择；`full` 超出有效输入上限时返回 422，
不会静默改成 RAG。官方 OpenAI 地址默认使用 Responses API。显式提示缓存只缓存
稳定的系统提示和上下文前缀，聊天历史与当前问题仍保持动态；自定义兼容地址默认
继续使用 Chat Completions。Responses 请求固定发送 `store: false`，不会为了提示
缓存而启用 provider 端 response application-state 存储。

`OPENAI_PROMPT_CACHE_TTL` 使用秒数表达兼容策略：GPT-5.6 及后续模型的显式断点
固定使用当前唯一受支持的 `30m`；将该值设为大于 1800 只会为官方明确支持扩展
保留的旧模型发送 `prompt_cache_retention: 24h`。旧模型不会收到
`prompt_cache_options` 或显式断点，仍使用其自动提示缓存。自定义兼容地址默认不发送
这些 OpenAI 专用字段。

提示缓存只能降低重复稳定前缀的费用和延迟，不会扩大模型上下文窗口，也不会让
超出 `AI_MAX_CONTEXT_TOKENS` 的全文变得可读。总预算会同时计算系统提示、历史、
当前问题、转录、项目知识和会话检索块。

`AI_INDEX_WORKERS` 和 `KNOWLEDGE_EXTRACT_WORKERS` 是 PostgreSQL 租约队列的固定
worker 数，默认均为 2，运行时接受 1–32。索引和文件任务可在进程重启后恢复；
不要按 HTTP 并发量把 worker 数无限放大。

`KNOWLEDGE_EXTRACT_WORKERS` 同时限制单个服务进程内文件提取和 Moodle OCR 的
外部命令总并发。Moodle 派生材料上传每用户最多 1 个、全局最多该数量的请求；
超额时在解析请求体之前返回 429 和 `Retry-After`。上传提取总时限为 2 分钟，
取消或超时不会保存不完整材料。

反向代理部署可设置 `TRUSTED_PROXY_CIDRS`（逗号分隔 CIDR，例如
`127.0.0.1/32,::1/128`）。默认不信任任何转发头；只在直连来源属于这些网段时，
限流从右向左解析 `X-Forwarded-For`，采用第一个非可信代理地址。代理必须正确
覆盖或追加客户端地址；不要把普通客户端所在网段设为可信代理。配置必须传入
应用进程或容器的环境，修改后重启生效；无效配置会禁用转发头信任并记录警告。

知识文件解析限制的单位如下：

- `KNOWLEDGE_MAX_FILE_MB`：单个原文件 MiB；
- `KNOWLEDGE_MAX_EXTRACTED_MB`：单个知识源提取文本 MiB，默认 10；
- `KNOWLEDGE_MAX_OFFICE_UNCOMPRESSED_MB`：DOCX/XLSX 包内总解压内容 MiB，
  默认 100；
- `KNOWLEDGE_MAX_IMAGE_MEGAPIXELS`：单张图片百万像素，默认 40；
- `KNOWLEDGE_MAX_PDF_PAGES`：扫描 PDF OCR 页数，默认 100。

这些值越界或格式非法时会回退到安全默认值。超过运行限制的文件会明确失败，不会
静默截断。容器只接受 `eng`、`chi_sim`、`jpn` 和 `kor` OCR 语言，且已经内置对应
Tesseract 数据。

项目知识库文件保存在 `KNOWLEDGE_DATA_PATH`，元数据与索引状态保存在
PostgreSQL。镜像内置 PDF 文本提取和图片 OCR。支持 PDF、DOCX、XLSX、
CSV/TSV、TXT/Markdown/JSON、PNG/JPEG/WebP；单文件默认上限为 50 MiB。

Compose 会根据 `POSTGRES_*` 自动构造容器内的 `DATABASE_URL`。不要把
数据库端口暴露到公网。转录存储按计费账户计量并受套餐 `storage_gb`
限制，在每次云端转录写入的数据库事务内强制执行，按说话人、原文和
译文的 UTF-8 字节数计量；租户的 `storage_quota_gb` 仍限制 AI 知识库
（文件、记忆、向量）的存储；
`RAG_MAX_DB_MB` 是共享 SQLite 向量库的近似硬总预算：有限预算会扣除
1 MiB SHM 余量及总量 1/16（最少 4、最多 64 MiB）的 WAL 份额，余额才
用于每个连接的 `max_page_count`。WAL 会自动 checkpoint，并在启动、关闭
或超预算时截断；无法清理时拒绝后续写入，单次序列化写入上限为 16 MiB。
SQLite 无法对进行中事务的 WAL 提供字节硬限制，因此瞬时最坏情况是预算再
加一个事务生成的 WAL；病态全库页改写或外部 SQLite 写入可接近约 2 倍配置
值（另加少量 frame 元数据）。部署时仍需保留应急磁盘余量。`-1` 关闭总量
限制，但不关闭 WAL 清理和单次写入保护。

### API 鉴权和注册

```dotenv
DREAMTRANS_API_KEY=
DREAMTRANS_ADMIN_API_KEY=
ALLOW_ANONYMOUS_API=false
ALLOW_WEBSOCKET_QUERY_TOKEN=false
API_RATE_LIMIT_PER_MINUTE=120
WEBSOCKET_MAX_CONNECTIONS=256
WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL=4

REGISTRATION_ENABLED=false
REGISTRATION_INVITE_CODE=
REGISTRATION_RATE_LIMIT_PER_HOUR=5
REGISTRATION_ALLOWED_EMAIL_DOMAINS=
REGISTRATION_BLOCKED_EMAIL_DOMAINS=
CORS_ALLOWED_ORIGINS=
ALLOW_USER_API_KEY=false
```

### 邮箱验证与防刷注册

```bash
RESEND_API_KEY=
MAIL_FROM=
SMTP_HOST=
SMTP_PORT=
SMTP_USERNAME=
SMTP_PASSWORD=
SMTP_TLS=
EMAIL_VERIFICATION_REQUIRED=
```

- 发信方式二选一：设置 `RESEND_API_KEY` 走 Resend 的 HTTP API（推荐，
  不需要 SMTP 端口）；或设置 `SMTP_HOST` 走 SMTP 中继。两者都配时优先
  Resend。`MAIL_FROM` 是发件人地址，形如 `DreamTrans <no-reply@yourdomain.com>`，
  Resend 要求该域名已在其后台完成验证（旧变量名 `SMTP_FROM` 仍可用）。
- 自主注册默认必须验证邮箱，不再直接登录：服务端发送一封带链接的
  验证邮件（24 小时有效），用户点击后账户才激活、才发放试用额度。未验证
  的账户登录会收到 `email_not_verified`，前端提供「重新发送」。链接指向
  `APP_BASE_URL` 下的 `/pro?verify=<token>`。发验证邮件时必须显式配置该变量为
  HTTP(S) 站点 origin（如 `https://dreamtrans.example.com`，不能带路径、用户信息、
  查询串或片段）；缺失或无效时注册和重发返回 503，不创建新账户或验证令牌。
  验证链接不会采用请求的 Host 或转发头。本地开发可使用 `http://localhost:8080`。
- `SMTP_TLS`：`starttls`（默认，端口 587）、`tls`（隐式 TLS，端口 465）或
  `none`。
- `SMTP_HOST=log` 不真正发信，只把邮件正文（含链接）打到容器日志，用于本地
  调试。
- `EMAIL_VERIFICATION_REQUIRED` 默认为 `true`：没有配置任何发信方式时，
  注册接口会返回 503（`email_delivery_unavailable`）并拒绝创建账户，启动日志
  会给出警告。只有显式设为 `false` 才会回到「注册即登录」的旧行为，仅建议
  内网或测试环境使用。
- 已有账户在升级时一律视为已验证，不会被锁在门外；管理员创建的账户也
  默认已验证。管理后台可手动把收不到邮件的用户标记为已验证。
- `REGISTRATION_RATE_LIMIT_PER_HOUR`：每个来源地址每小时允许的注册和重发
  验证邮件次数（默认 5），叠加在原有每分钟 20 次的认证接口限流之上。
- `REGISTRATION_ALLOWED_EMAIL_DOMAINS`：逗号分隔。设置后只有这些域名及其
  子域名可以注册（例如 `monash.edu` 同时放行 `student.monash.edu`）。
- `REGISTRATION_BLOCKED_EMAIL_DOMAINS`：逗号分隔，追加到内置的一次性邮箱
  黑名单（mailinator、guerrillamail、yopmail 等）之后。
- 同一个邮箱的别名只能注册一次：忽略 `+tag`，Gmail 还会忽略本地部分的点
  和 `googlemail.com` 别名。

- 浏览器统一工作台的 `/pro` 登录入口使用 JWT。
- WebSocket JWT 默认通过 `Sec-WebSocket-Protocol: dreamtrans.jwt.<JWT>` 传输。
  URL 中的 `?token=` 会进入代理日志，因此默认拒绝；仅迁移旧客户端时可临时
  设置 `ALLOW_WEBSOCKET_QUERY_TOKEN=true`。
- 服务调用在 `X-DreamTrans-API-Key` 中发送 `DREAMTRANS_API_KEY`。
- 管理服务密钥必须与普通服务密钥不同。
- 匿名 provider API 和自助注册默认关闭。
- 对公网启用注册时建议同时配置邀请码。
- `CORS_ALLOWED_ORIGINS` 是逗号分隔的完整 Origin；同源访问不需要 CORS。
- `ALLOW_USER_API_KEY=false` 时，普通用户不能绕过服务端托管的 API 密钥。
- `WEBSOCKET_MAX_CONNECTIONS` 是翻译和 Speechmatics 两类长连接共享的
  进程总上限；`WEBSOCKET_MAX_CONNECTIONS_PER_PRINCIPAL` 限制单个用户或
  服务调用方。达到上限时在升级前返回 `429`，防止长期连接耗尽文件描述符、
  SQLite 句柄或上游连接。
- 没有月度请求次数或分钟数配额：账户余额本身就是用量上限，套餐只限制
  并发转录流数（`max_concurrent_sessions`，作用于活跃的转录 WebSocket
  连接，而不是会话记录的状态）、存储和保留期。

### 健康检查与关闭

- `GET`/`HEAD /healthz`：进程存活，不访问外部 API。
- `GET`/`HEAD /readyz`：数据库模式下执行有 2 秒上限的 DB ping。
- Compose 等待 `/readyz` 健康后才报告服务可用。
- 服务最多用 20 秒排空请求和 WebSocket，Compose 提供 30 秒停止窗口。

## 备份

数据库每日加密备份到 Cloudflare R2 由 `scripts/backup.sh` 完成，所需的
`R2_*`、`BACKUP_PASSPHRASE`、`BACKUP_RETENTION_DAYS`、`BACKUP_HEALTHCHECK_URL`
写在同一个 `.env` 里，应用容器不读取它们。步骤与恢复方法见 `docs/BACKUP.md`。

## 前端构建变量

Vite 变量必须以 `VITE_` 开头，并在构建时注入：

```dotenv
VITE_BACKEND_URL=/
VITE_BACKEND_WS_URL=/
VITE_SPEECHMATICS_OPERATING_POINT=enhanced
```

生产镜像默认使用 `/`，即 API、WebSocket 和页面同源。分离域名构建：

```bash
docker build \
  --build-arg VITE_BACKEND_URL=https://api.example.com \
  --build-arg VITE_BACKEND_WS_URL=wss://api.example.com \
  -t dreamtrans .
```

本地开发可在 `frontend/.env` 中使用
`http://127.0.0.1:8080`/`ws://127.0.0.1:8080`。

## PCAS Event Worker

```dotenv
PCAS_ADDR=127.0.0.1:50051
PCAS_INSECURE=false
PCAS_CA_CERT=
PCAS_TLS_SERVER_NAME=
PCAS_API_KEY=
AUDIO_URL_ALLOW_HTTP=false
```

非回环 PCAS 地址默认使用 TLS。Worker 不会通过远程明文连接发送
`PCAS_API_KEY`。远程 `audio_url` 默认只允许 HTTPS，并始终拒绝私网、
回环、link-local、云元数据和其他特殊 IP。

## PCAS Streaming Provider

```dotenv
GRPC_BIND_ADDR=127.0.0.1
GRPC_PORT=50052
PCAS_API_KEY=
PCAS_TLS_CERT=
PCAS_TLS_KEY=
PCAS_ALLOW_INSECURE_REMOTE=false
PCAS_MAX_CONCURRENT_STREAMS=32
PCAS_ENABLE_REFLECTION=false
```

非回环监听必须使用至少 16 字符的独立服务密钥，并默认要求证书/私钥
成对配置。并发范围为 1–1024；reflection 默认关闭。

## 直接运行源码

后端与当前统一前端应分别启动；浏览器访问 Vite 地址（默认
`http://localhost:5173`），不要把 `backend/public` 中的历史发布快照当成
当前前端源码：

终端 1：

```bash
cd backend
go run ./cmd/web
```

终端 2：

```bash
cd frontend
npm ci
npm run dev
```

只在本机匿名兼容开发中使用以下后端启动方式：

```bash
SM_API_KEY=... ALLOW_ANONYMOUS_API=true go run ./cmd/web
```

对网络开放时应使用 PostgreSQL/JWT 或独立服务密钥，不要启用匿名模式。
