# YuAction

让每一次表达，都有回应。

YuAction 面向教师与演讲者，把活动、现场提问、大屏与共享字幕放进同一个房间。它与 Yufolo 的产品分工是：YuAction 组织现场互动，Yufolo 提供转录、翻译和个人学习能力。

**当前版本：开发预览。** 已实现多人互动、Yufolo 账号登录、主持端麦克风转录、参与者自选译文，以及主持人 AI／资料知识库。真实转录由主持人明确开启，使用其 Yufolo 余额；独立的演示字幕仍不调用付费服务。

界面预览：[登录首页](docs/previews/login.png) · [活动空间](docs/previews/workspace.png) · [主持人工作台](docs/previews/host.png) · [手机参与页](docs/previews/participant-mobile.png) · [现场大屏](docs/previews/display.png)

## 已实现

- 创建课堂 / 演讲，生成房间码、邀请链接和二维码。
- 主持人工作台、手机参与页、独立大屏，采用 React + TypeScript。
- 创建活动时引导登录并自动继续；已登录用户直接查看活动，显示进行中 / 已结束状态。
- 邀请弹窗集中展示二维码和链接；手机端提供字幕 / 提问快捷入口，跨设备录音状态明确提示。
- 匿名公开提问，可引用一段共享字幕。
- 主持人展示问题、标记已解答、结束和重新开启活动。
- 房间内 SSE 实时同步；重连获取最新快照，不会修改其他房间的上屏状态。
- PostgreSQL 持久化与乐观并发控制；本地也可以显式选择内存演示模式。
- 主持人密钥校验、活动创建密钥、服务端字幕接入凭证、请求大小与基本速率限制。
- 已确认字幕的幂等接收和一对多分发；同一字幕 ID 的重复提交不会产生第二条字幕。
- Yufolo 邮箱密码登录、账号活动列表、换浏览器恢复主持权限。
- 创建关联转录会话、单路麦克风采集、暂停／继续、参与者各自选择译文、结束活动时停止上游转录。
- 复用 Yufolo 的字幕合并规则：同一说话人的短碎片合并，稳定后翻译，同语言结果在房间内缓存。
- 最终字幕先持久化，再归档到 Yufolo；归档失败后停止录音，下次开始补存。
- 主持人资料上传、知识库检索、通用 AI 与资料回答、来源引用、自定义提示词、自动回答、问题删除。
- 连接 Yufolo 后复用其知识库、提取／索引、模型和计费；独立安装可配置兼容 OpenAI 的接口。详见 [AI 与知识库](docs/AI_KNOWLEDGE.md)。

## 合仓后的部署边界

YuAction 保留独立容器、域名和端口（默认 `11452`）。本次统一代码、CI 和发行版本；DreamTrans 的 `dreamtransctl upgrade` 尚不升级 YuAction，后者仍通过自己的安装器原地更新。已有安装首次迁移请使用下方新地址，保留原 `--dir` 或 `--dreamtrans-dir` 参数。原数据库、房间、密钥和端口均保留。详见 [合仓说明](../docs/deployment/yuaction-monorepo.md)。

## 一键安装 / 更新

在 Linux 服务器执行：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash
```

默认安装到 `~/yuaction`，访问 `http://127.0.0.1:11452`。脚本生成独立随机密钥、拉取已发布镜像并检查服务健康；Debian / Ubuntu 缺少 Docker 时，以 root 运行可自动安装。

更新时再次执行同一命令，或明确使用：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --update
```

更新保留配置和数据，先备份数据库，再替换服务。通过 `bash ~/yuaction/install.sh --show-key` 查看创建活动所需的密钥。直接通过服务器 IP 访问时，安装命令末尾改为 `bash -s -- --bind 0.0.0.0`；公网使用请配置 HTTPS。自定义目录、端口和备份恢复见 [安装说明](docs/INSTALL.md)。

如果 DreamTrans 已安装在 `/dreamtrans`，可直接安装到它的子目录，读取现有 `.env` 并复用同一个 PostgreSQL 容器和数据库：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --dreamtrans-dir /dreamtrans
```

YuAction 使用独立的 `yuaction` schema，自己的端口、密钥和镜像版本保存在 `/dreamtrans/yuaction/.env`。安装脚本会发现 DreamTrans 应用容器并配置 `YUFOLO_URL`；更新后用已有 Yufolo 邮箱密码登录，创建活动并点击「开始转录」。主持人页面需要 HTTPS 或 localhost 才能使用麦克风。详情见 [账号与转录联动](docs/YUFOLO_INTEGRATION.md)。

## 本地启动

从 DreamTrans 仓库根目录先执行 `cd yuaction`，再运行下列命令。

需要 Go 1.22+、Node.js 22.12+（推荐 Node 24）。前后端分别开一个终端。

```bash
cd backend
go mod download
YUACTION_DEMO=true go run ./cmd/server
```

```bash
cd frontend
npm ci
npm run dev
```

访问 http://127.0.0.1:5174 。后端只监听 `127.0.0.1:18083`，前端通过 Vite 的同源代理访问 API。

创建一个活动，在主持人页复制参与链接，用另外两个浏览器窗口加入，再打开大屏。提交问题、点击「展示问题」或「发送演示字幕」，其他窗口会同步更新。主持人密钥保存在当前标签页的 `sessionStorage`，请展开「主持人访问密钥」保存它，以便关闭标签页后恢复管理。邀请链接中不含主持人密钥。

未设置 `DATABASE_URL` 的演示实例重启会丢失数据。要保留数据，同时设置 PostgreSQL 连接串。手机扫码测试需让手机能够访问前端：在可信局域网运行 `npm run dev -- --host 0.0.0.0`，通过主机局域网 IP 打开页面后再分享。公网部署应使用 HTTPS。

## Docker Compose（本地构建）

```bash
cp .env.example .env
# 编辑 .env，为 POSTGRES_PASSWORD 与 YUACTION_CREATOR_KEY 设置独立随机值。
# 可用 openssl rand -hex 32 分别生成。
docker compose up -d --build
```

访问 http://127.0.0.1:11452 。Compose 默认关闭演示模式，数据库持久化到 `yuaction_postgres` 卷；只有前端 Nginx 暴露本机端口。创建活动需要输入 `.env` 中的活动创建密钥。主持人仍通过每个房间独立的密钥管理活动。

配置 `YUFOLO_URL` 后改用 Yufolo 账号创建和管理活动；未配置时保留创建密钥模式。成员角色、问题审核等能力仍待完善，见 [开发路线](docs/ROADMAP.md)。

## 自动 Docker 发布（GHCR）

由仓库根目录 `.github/workflows/ci.yml` 统一验证两个产品。全部检查通过后，`docker-build.yml` 发布同一提交的 DreamTrans、Edge 与 YuAction 前后端 AMD64 / ARM64 镜像，全部构建成功后才提升主站和 YuAction 的 `latest`。PR 只验证，不发布；版本标签沿用 DreamTrans 的统一发行版本。

```text
ghcr.io/coyumelabs/dreamtrans-yuaction-backend:latest
ghcr.io/coyumelabs/dreamtrans-yuaction-frontend:latest
```

准备 `.env` 后，可以直接拉取镜像部署：

```bash
docker compose -f compose.ghcr.yml pull
docker compose -f compose.ghcr.yml up -d --no-build
```

将 `.env` 的 `IMAGE_TAG` 设为同一个 `sha-<完整commit SHA>` 可固定前后端版本；一键安装脚本会自动固定匹配版本。详见 [发布、更新和回退说明](docs/DOCKER_RELEASE.md)。自动化范围为镜像发布，服务器更新使用一键更新命令。

## 配置

| 环境变量 | 用途 |
|---|---|
| `LISTEN_ADDR` | Go 监听地址，默认 `127.0.0.1:18083` |
| `DATABASE_URL` | PostgreSQL 连接串；非演示模式必填 |
| `YUACTION_DEMO` | 必须显式设为 `true` 才开启匿名创建 / 演示字幕及内存存储回退 |
| `YUACTION_CREATOR_KEY` | 活动创建凭证；非演示模式至少 32 字符 |
| `YUFOLO_INGEST_KEY` | 服务端字幕接入凭证，至少 32 字符；不设置时关闭接入 |
| `YUFOLO_URL` | DreamTrans 后端地址；开启账号登录与实时转录，关联安装时自动发现 |
| `TRUST_PROXY` | 仅在受控代理之后设为 `true`，使用代理覆盖写入的 `X-Real-IP` 限流 |
| `API_TARGET` | Vite 开发代理的后端地址 |

## 验证

```bash
cd backend
go test -race ./...
go vet ./...
# 专用测试数据库，可选：
TEST_DATABASE_URL='postgres://user:password@localhost:5432/yuaction_test?sslmode=disable' go test ./internal/storage -count=1
```

```bash
cd frontend
npm run build
npx playwright install chromium
npm run test:e2e
```

浏览器测试自动启动隔离的内存后端（18084）和前端（5175），覆盖主持人、两个参与者与大屏之间的同步、字幕引用、重新进入后的补齐、结束活动及手机布局。

## 项目结构

```text
backend/
  cmd/server/            启动、配置、关闭
  internal/app/          房间模型、REST API、SSE 广播、权限与行为测试
  internal/storage/      内存 / PostgreSQL 存储、初始 schema、并发写入校验
frontend/
  src/                   工作空间、主持人、参与者、大屏
  e2e/                   多浏览器功能测试
docs/
  ARCHITECTURE.md         当前设计与边界
  YUFOLO_INTEGRATION.md   账号、会话与实时转录联动
  ROADMAP.md              后续开发顺序
```

当前代码在 DreamTrans 主仓库的 `yuaction/` 目录维护，原 YuAction 历史通过保留父提交的合并导入。AnyQA 原项目作为业务参考保留。
