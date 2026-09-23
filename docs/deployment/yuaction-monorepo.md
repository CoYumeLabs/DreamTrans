# YuAction 合入 DreamTrans

YuAction 源代码位于 `yuaction/`，导入源为原仓库提交 `5561118`。合并提交保留原仓库作为第二父提交，历史没有 squash。用 `git log --all --oneline` 查看原提交；导入前的路径仍按原仓库记录。原仓库和本地 `/root/AnyQA/YuAction` 没有自动删除或归档。

## 开发与发行

- DreamTrans：根目录 `backend/`、`frontend/`；YuAction：`yuaction/backend/`、`yuaction/frontend/`。两套 Go / npm 模块和运行进程保留。
- 主站使用 `.github/workflows/ci.yml`，YuAction 使用独立的 `.github/workflows/yuaction.yml`；后者保留完整 Go race、数据库迁移、安装器生命周期、浏览器和镜像运行时验证。共享转录客户端和运维组件变更会触发双方验证。
- 导入时升级 YuAction 的 pgx、Excelize 和 Go 扩展库到安全修复版本；本地后端需要 Go 1.26+，CI／Docker 固定为 1.26.5。
- 每个产品各自全部检查通过后调用 `docker-build.yml`，并行推送此前构建与验证的镜像产物，不重新构建。主站与 Edge 使用同一提交；YuAction 前后端使用同一 YuAction 提交。各产品发行清单 `release-images.json` 分别记录自己的固定 digest。
- YuAction 镜像为 `ghcr.io/coyumelabs/dreamtrans-yuaction-backend` 和 `ghcr.io/coyumelabs/dreamtrans-yuaction-frontend`；`sha-<完整提交>` 对应本仓库源代码。旧仓库镜像不覆盖。
- 每个产品的所有镜像验证和平台发布成功后才提升自己的 latest。跨镜像标签不是原子事务；YuAction 安装器先解析后端提交，再按同一 SHA 拉取前端，不依赖两个 latest 同时变化。主站更新 latest 前保证匹配的 Edge 已发布。需要完全固定版本时使用清单中的 digest。
- 发布新 GHCR 包后需确认匿名可拉取；私有组织部署可以沿用 `docker login ghcr.io`。

## 已有安装迁移

主仓库新版镜像发布后，从新地址运行一次安装器。沿用原安装目录、Compose 项目和绑定端口，不创建新数据库：

```bash
# 与 DreamTrans 共用数据库的安装，替换成实际父目录。
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --dreamtrans-dir /root/dreamtrans --update

# 独立安装，替换成实际目录。
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --dir /root/yuaction --update
```

缺省镜像前缀及旧官方 `ghcr.io/coyumelabs/yuaction` 前缀迁移到新包，自定义 registry 前缀保留。安装器仍在替换容器前备份配置和数据库；更新过的安装器与 Compose 从解析出的同一发行提交获取，不再从另一个仓库或浮动 main 混取。旧版回退使用备份的旧 Compose / 配置及原镜像；新仓库不提供旧 YuAction 提交号的镜像。

YuAction 默认端口仍为 `11452`，自定义 `APP_PORT`、`APP_BIND`、域名和房间链接保留。关联模式继续使用同一 PostgreSQL 的 `yuaction` schema；独立模式继续使用原独立数据库卷。

蓝绿主站的关联安装识别本安装目录的固定代理，并使用 `http://dreamtrans:8080`。若代理尚未接入原共享网络，先执行主站 `dreamtransctl sync-entry`。标准 Compose 主站继续沿用现有应用容器发现方式。

## 此次合仓不改变运行升级协议

采用蓝绿后，`dreamtransctl upgrade` 会继续升级安装目录下的 `yuaction`，YuAction 解析自己独立验证的最新发行版，再固定匹配的前后端；不再要求与主站同一提交，仍保留独立端口。YuAction 自身使用同一个 Go 运维工具的 `yuaction` 子命令；固定 Nginx 入口只 reload 路由，不重建正在承载 WebSocket 的入口。登录令牌以原创建密钥派生的 AES-GCM 密钥加密保存到共享数据库，刷新跨实例串行执行。录音与后台任务用数据库锁确定归属，排空结束后才停止旧容器。

浏览器收到交接请求后先验证接续接口，在原麦克风继续采音的同时缓存新音频；旧实例等待最终字幕保存、上游流结束并释放录音锁后确认交接。新实例接续同一活动和 Yufolo 会话，再发送缓存。回滚使用相同交接流程，不恢复旧数据库。缓存上限为 60 秒 PCM；异常断网或上游无法完成保存会明确报错，不将不确定音频静默重放。

首次从旧 Compose 版本转换需要维护窗口：先结束旧版页面录音，运行原安装命令并添加 `--adopt-bluegreen`，然后打开新版页面登录。旧版内存登录态和已经加载的浏览器代码无法在服务端升级时自动改写；安装器不会把这种首次转换称为无感迁移。之后的版本升级无需刷新页面、重新授权麦克风或重新登录。

```bash
/root/dreamtrans/dreamtransctl --dir /root/dreamtrans upgrade
/root/dreamtrans/yuaction/dreamtransctl yuaction --dir /root/dreamtrans/yuaction status
/root/dreamtrans/yuaction/dreamtransctl yuaction --dir /root/dreamtrans/yuaction rollback
```

候选失败用 `abort` 保留当前服务，发布被中断用 `resume`。有 systemd 时安装器配置自动排空；其他环境前台等待后保留未完成任务的旧实例，使用 `drain` 继续检查，始终不强制切断录音。

YuAction 目前通过主站 `/ws/speechmatics` 转录，并转发主站的 `DeploymentHandoff` 请求。启用区域 Edge 调度后，主站仍接受此接口的新连接，并与 Edge 授权共用用户并发上限；YuAction 当前仍通过主站接入。本次蓝绿验证覆盖 YuAction 当前的主站转录链路，不将区域 Edge 支持列为已实现。

## 后续维护

新功能与修复只向 DreamTrans 仓库提交。旧仓库停更/归档需在统一发行验证后单独处理。保留独立入口与现场权限；共享转录显示模型、登录持久化、录音交接和运维协议，保留各自的现场权限边界。
