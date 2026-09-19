# 区域 Edge 与蓝绿发布验证记录

状态：开发验证中，尚未满足生产发布验收；不能将本记录视为上线批准。现有生产目录、卷、Tunnel 和 DNS 未操作。

## 2026-09-19 本地验证

- Node 24.18.0，已执行 `npm ci` 和 Playwright Chromium 安装。
- 后端 Go 1.26.5：`go mod tidy -diff`、依赖下载及 `go mod verify` 通过。
- golangci-lint 2.12.2，`--build-tags=event_worker`：0 issues。
- 迁移包连续到 054；全量迁移已应用到独立测试容器 `dreamtrans-edge-ci-pg` 的 `dreamtrans_edge_test` 数据库。
- 安装器 fresh lifecycle、administrator update 和 mocked backup 检查通过。
- 新发布控制器 7 项恢复/资源检查测试通过：有会话时超时保留、未确认的停止队列拒绝复用、先记录排空再停止、首次转换复用准确实例、拒绝错误数据目录、非 TTY 进度输出、主站与小规格 Edge 独立内存门槛。
- 完整后端 `go test -v -race -coverprofile=... ./...` 通过；独立 `event_worker` race 测试通过。
- 首次完整前端 `verify:ci` 命令成功结束：57 项 E2E 首轮通过、1 项 onboarding 重试通过（58 项总计）。失败 trace 显示模块加载 `ERR_NETWORK_CHANGED`；将等 Docker 构建/生命周期结束后完整重跑，不把这次结果描述为无不稳定测试。
- 在所有 Docker 构建与生命周期检查结束后，重新执行完整 `verify:ci`：59 项 E2E 全部首轮通过（含新增节点管理测试），无失败和重试。lint、type-check、build、core、long-session、AI、admin 验证全部通过。
- 主站与 Edge 镜像构建通过；Edge 镜像非 root、无数据库工具/主站 rag.db、拒绝 DATABASE_URL 凭证检查通过。
- 主站新镜像应用卷权限迁移、独立前端镜像构建与三入口运行时、生产镜像内文件提取 fixtures 检查通过；迁移后及标准部署升级/故障恢复集成测试通过，实际卷标识与新增数据保留。
- 原有 provider model、model catalog constraints、acquisition 迁移回放测试通过（隔离测试库）。

## 仍须完成的验收

1. 镜像运行时与迁移部署升级测试；新蓝绿真实代理切换、中断、回切和重启集成测试。
2. Edge 安装器首次注册与不同中断阶段恢复；独立 Tunnel 凭证配置；注册/轮换/吊销及故障场景的完整管理员操作 E2E。
3. 明确音频接收确认、结果保存确认和浏览器释放重放缓冲之间的关系；模拟供应商收到音频但尚未返回最终结果时的宕机。
4. 原节点故障跨节点接管、供应商会话重建、字幕拼接，以及重放音频计费边界的集成证据。
5. 指标完整性、受限凭证轮换与回传冲突的人工对账流程。
6. 应用文件与 PostgreSQL 联合备份的隔离恢复测试；YuAction 稳定内部入口交接验证。
7. Ubuntu 26.04 EC2 主站与 Ubuntu 24.04/26.04 Lightsail Edge 安装、重复安装、重启、卸载；1 GB Edge 双版本资源余量与真实多地区 P50/P95 延迟比较。

上述第 7 项不能用本地 Docker 或模拟供应商结果代替。现有 `--update` 仍是原地升级；蓝绿是新增的显式转换与发布命令。首次转换涉及停止旧 SQLite 写入和 16002 入口交接，存在维护中断。不能回切到继续写 SQLite 的旧镜像，也不能通过恢复旧数据库快照进行版本回切。

## 同步主线与 Lightsail 调整

已合入主线 `6cf14fa` 的 YuAction 无状态 RAG 与知识状态轮询修复，保留新的 `rag_stateless_supported` 声明。合入后后端完整 race、带 event_worker 的 lint、依赖、原迁移部署升级/恢复检查再次通过。

Edge 支持 Ubuntu 24.04/26.04 引导安装，并检测缺少 Compose 的情况；3 项引导测试通过。Edge 发布清单独立于主站，候选要求 384 MiB 可用内存，应用容器每个颜色限 256 MiB。在 1 vCPU/256 MiB（禁用 swap）的 Edge 生产镜像中，回传队列崩溃恢复/独占锁与音频转发/网络分区/排空运行时测试通过（模拟主站与供应商也在测试进程内）。1 GB Lightsail 整机蓝绿与真实供应商测试仍待机器就绪后执行。
