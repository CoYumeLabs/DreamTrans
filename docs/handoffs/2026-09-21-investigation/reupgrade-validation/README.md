# 2026-09-21 重新升级前的本地验证

> 以下为 2026-09-21 的实验记录；提及的 `.log` 文件未随仓库归档，结构化 JSON 结果与实验脚本予以保留。生产状态请参见当时的 [后续调查记录](../../2026-09-21-investigation.md)，不应据此执行今天的生产发布。

用户提供的生产只读 status：active green 为 e163d7b 固定旧版，blue 为 faf654e 固定新版且已停止；phase ready；采样时旧版 HTTP、任务、WebSocket、pending 均为 0。本实验未连接或修改生产。

复用已验证的恢复运行时测试初始化逻辑，用精确发布镜像、原镜像 CLI、固定 Nginx 与独立 PostgreSQL/应用卷构造相同颜色和版本状态。执行：旧 green → deploy 新版 --pause → abort → 再次候选 → resume → 验证普通 rollback 拒绝 → 专用恢复脚本回旧 green。

结果见 results.json 和 run.log：1,139 次持续 settings 请求全部 200，最大 21.174 ms。候选阶段代理仍指向 green；green 容器 ID、启动时间、CLI 哈希、应用环境、57 条迁移账本不变。abort 后 blue 停止，green 保持原实例，候选期间写入的 marker 数据保留。切流后普通 rollback 因 provider_credentials 能力降级而拒绝且未改变状态；专用恢复脚本成功恢复旧 green，保留数据库/应用卷、迁移和新写入。

限制：没有长连接或后台任务、没有活动 Edge、只有合成测试数据，不能据此确认原故障根因，也不能保证生产切流后随时恢复。专用恢复脚本要求 ready 或其自身恢复候选状态，且核验精确 CLI、镜像、迁移、卷、可用内存和 Edge 状态；生产恢复脚本绝对路径尚未确定。旧实例有连接导致 draining 时不满足恢复前置。

下一步仅候选阶段：固定原故障镜像 deploy --pause；入口继续旧 green，观察第二实例共存的资源影响。候选发布仍会运行迁移器和应用启动数据库操作，不是只读操作；abort 不撤销数据库已提交写入。待命候选不会承接正常业务，不能据此验收新版业务性能。切流前还需落实生产回退通路与同窗口观测。

run.py 是本地一次性实验驱动，依赖开发机恢复脚本绝对路径和 DREAMTRANS_CTL 环境变量；不是生产脚本或新增正式 CI。临时 Docker 容器、卷和网络已由 finally 清理。没有产品代码修改、提交或推送。

## 用户回传与条件切流命令

用户随后回传生产候选已启动。待命期间 5 轮 settings 均 200，本机 TTFB 1.028–1.624 ms，公网 37.262–55.456 ms；入口仍由旧 green 服务。用户确认生产 CLI SHA-256 为 `876f0ba2dd670c47287a81086bbcdd967b5162990b8c0f83c5d2d3982a1316ed`，恢复脚本 `/root/dreamtrans-transition/recover-e163.sh` SHA-256 为 `2402eaa44bd231387a632eac50f688c5aa5d789c314781b4b83395c7d5537c80`，均与本地验证对象一致。结构化记录见 production-candidate-observation.json。数据不能证明新版实际接流性能正常。

新增 switch-checked.sh 只用于该精确候选状态：检查颜色/版本/无未完成工作、全局及候选 Edge 路由关闭、固定入口、可用内存和只读 Edge 阻断计数，全部通过才执行一次 `resume --observe 0 --drain-timeout 0`。0 秒自动观察是因为普通自动回滚不能完成此能力降级，切流后必须进行外部性能观察；此脚本没有自动恢复功能，也不保证随后一定 ready。实验期间不应并行发布、修改配置或注册 Edge。

最终脚本通过 Bash 语法及精确发布镜像/隔离 PG 的再次运行时验证：活动 Edge、候选独立环境开启 Edge、路由请求返回 green 文本但退出码失败，三种情形均被拒绝且发布状态不变。正常路径切流至 blue/ready，随后使用专用脚本恢复旧 green。共 1,101 个 settings 请求全部 200，最大 21.766 ms；见此前的 checked-switch-final.log。本环境未安装 shellcheck，未执行该检查。这里只修改临时实验材料，没有修改产品或生命周期实现，因此未重跑产品全量 CI。

恢复进一步边界：脚本还要求新版 active 容器仍在运行，并始终拉取旧镜像，因此不能承诺容器退出、切流中断或镜像仓库不可达时立即恢复。当前 agent 尚未执行生产切流；下一步命令交由用户在其生产终端运行并回传结果。

## 粘贴兼容性修正

用户在生产终端复制嵌套 here-document 时报告 SQL 结束标记未识别，语法错误发生在数据库命令和切流命令之前；此前命令只有只读检查。已改为括号子 shell 和参数化 `psql -c`，没有 here-document；子 shell 隔离 `set -e` 等设置，整个括号块完整解析后才开始执行。

修改后重新通过 Bash 语法检查（含整段缩进两空格）及精确发布镜像/独立 PG 的完整条件切流实验；全部三个拒绝用例通过，正常切流与专用恢复通过。最新 settings 采样 1108 次全部成功，最大 20.473 ms。见 checked-switch-results.json 和 checked-switch-no-heredoc.log。尚未收到用户新的生产状态。
