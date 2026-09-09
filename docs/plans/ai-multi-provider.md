# 多家 OpenAI 兼容供应商（2026-09-09）

## 现状

所有 AI 请求（翻译、总结、聊天、嵌入、模型目录同步）只有一个端点：`OPENAI_API_BASE` +
`OPENAI_API_KEY`。目录里每个模型的 `provider` 都是常量 `openai-compatible`，模型策略、用户偏好、
成本目录、用量记录都只按模型 ID 定位；Responses/chat 和提示缓存按"基址是不是 api.openai.com"猜。
要同时用 Cerebras 之类的第二家，只能在前面架聚合层。

## 设计

**供应商注册表**（新包 `internal/aiproviders`）从环境变量装配，进程内只读：

```dotenv
# 现有变量原样保留，注册为名为 openai-compatible 的默认供应商
OPENAI_API_KEY=…
OPENAI_API_BASE=https://api.openai.com/v1
# 追加的供应商：名称=基址；名称只允许 [a-z0-9-]
AI_PROVIDERS=cerebras=https://api.cerebras.ai/v1;groq=https://api.groq.com/openai/v1
AI_PROVIDER_KEYS=cerebras=csk-…;groq=gsk-…
# 可选：每家的接口形态与提示缓存，默认 chat、不缓存
AI_PROVIDER_OPTIONS=cerebras=chat
# 嵌入固定走一家（维度 1536 不变）
AI_EMBEDDING_PROVIDER=openai-compatible
```

固定变量名是为了让安装器生成的 compose 能原样透传，不需要按供应商动态加行。

**模型标识**：非默认供应商的模型在系统内一律用 `供应商/模型ID`（如 `cerebras/llama-3.3-70b`），
默认供应商保持裸 ID。目录、策略、用户偏好、用量记录、账单都存这个字符串，因此不改
`model_policies` / `user_model_preferences` 的结构。解析时按第一个 `/` 拆出供应商；没有前缀即默认
供应商。`provider_models` 表本来就按 `(provider, model_id)` 键，`model_id` 存裸 ID。

**请求路由**：`openai_provider.NewConfigFromEnv()` 的调用点改为 `aiproviders.ConfigFor(modelID)`：
先拆供应商，再取该供应商的基址、密钥、接口形态和缓存开关，模型填裸 ID。回退模型
（`OPENAI_FALLBACK_MODELS`）只在同一供应商内生效。用户自带密钥的覆盖逻辑不变，且优先级最高。

**目录同步**：对每家供应商各拉一次 `/models`，各自记录同步状态和错误；后台目录按供应商分组显示，
模型 ID 冲突时靠前缀区分。成本目录本来就按 `(provider, sku)` 键，非默认供应商的模型需要管理员在
"模型与定价"里录入成本后才能被批准使用，规则与现在一致。

**嵌入**：只能配一家；换嵌入供应商需要它支持 1536 维（`text-embedding-3-small` 同规格），否则拒绝启动。

**RAG 开关**：注册表里至少有一家可用即开启，不再只看 `OPENAI_API_KEY`。

## 不做的事

- 不做后台录入密钥的界面；密钥仍来自部署环境（与现有做法一致，避免密钥入库）。
- 不做跨供应商自动回退；一个模型策略只指向一家。
- 不改嵌入维度。

## 对外可见的变化

- 设置面板和账单里非默认供应商的模型显示为 `供应商/模型`。
- 后台"模型与定价"多一列供应商和每家的同步状态。
- 安装器 `.env` 和 compose 多出 `AI_PROVIDERS`、`AI_PROVIDER_KEYS`、`AI_PROVIDER_OPTIONS`、
  `AI_EMBEDDING_PROVIDER` 四个变量，默认为空即单供应商，行为与现在完全一致。
