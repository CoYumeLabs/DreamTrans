# Go 运维工具交接

`dreamtransctl` 是独立的 Linux Go 可执行文件（amd64/arm64），由发行镜像携带。生产部署不再调用 Python；Bash 负责依赖引导、旧标准安装和 R2 加密上传编排。历史 Python 控制器仅留在 `scripts/tests/legacy` 作为回归基线，不进入镜像。

## 已有蓝绿主站

先从已通过 CI 的固定提交获取 `scripts/install-ops.sh`，核对发行提交和脚本 SHA-256，再运行：

```bash
sudo bash install-ops.sh MAIN_REPOSITORY@sha256:RELEASE_DIGEST \
  --dir /root/dreamtrans
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans status
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans upgrade
```

安装器从指定镜像 ID 提取 Go 工具和配套 `backup.sh`，原位更新工具，保留已有工具的 `.previous` 副本，不重建容器、不修改 `.env`、Compose、卷、R2 口令或 cron。已有 `.bluegreen/state.json` 的 format 1、未知字段和锁保持兼容。先核对记录的数据库容器及实际卷；身份不符时停止。主站与 Edge 目录角色不符也会拒绝接管。不要同时调用新旧控制器。

`upgrade` 从官方仓库 main 的 `latest` 解析提交和 digest，展示并记录实际固定版本，然后执行兼容迁移、候选检查、切流、观察和排空。镜像、迁移或检查失败不会切走正常旧版本。启用下述后台排空后，观察完成即可退出终端，后台负责旧实例收尾。未配置时保留原来的前台等待与手动 `drain` 行为。未完成发布使用 `resume`，切流前放弃候选使用 `abort`。手动指定固定版本使用 `deploy --image REPOSITORY@sha256:DIGEST`。

## 后台等待与会话迁移

安装新 Go 工具后执行一次（可重复修改；单位为秒）：

```bash
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans configure-drain --handoff-after 600
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans upgrade
```

切流后先给旧连接 10 分钟自然结束。systemd 每 15 秒检查一次；全部连接、HTTP 请求、任务及未确认队列归零时自动停止旧实例，无需人工 `drain`。超时则核对新版就绪和入口路由，向旧版发送协作迁移通知。等待从切流时间起算（包含观察期），时间记录写入 `.bluegreen/state.json`；策略单独保存在 `drain-policy.json`。关掉 SSH 不影响检查，服务器重启后定时器继续。`--handoff-after -1` 表示无限等待自然结束，但仍自动清理已排空实例；`0` 表示观察完成后立即尝试迁移。后台与发布、备份共用锁，忙时留到下一轮；不会自动越过失败的观察或未完成的候选发布。

新版浏览器继续采集音频，先检查主站是否可用，再向旧供应商发送 EndOfStream，等待末尾字幕后连接新版，补发迁移期间有界缓存中的音频。Edge 协议 2 沿用主站确认的持久水位、递增会话代次和预算结算，不另行重置账本或放宽授权。翻译连接等待已提交请求的结果后迁移，新请求保留原幂等 ID。不会迁移 TCP 连接，也不承诺字幕完全没有短暂延迟。

首次引入该功能时，旧服务端或已打开的旧页面可能不支持通知，仍保留连接等其自然结束。前置检查失败也不会主动断开原连接；供应商完成确认失败、迁移中网络故障则进入既有重连流程，界面报告错误。音频缓冲有 30 秒上限，长时间网络分区不能承诺无损；不会把超时强杀描述成无缝迁移。后台任务不强行迁移，继续等待完成。只有全部工作归零才停止旧实例。

```bash
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans status
sudo journalctl -u 'dreamtrans-drain-*.service' --since '30 minutes ago'
```

状态中的 `drain_policy`、`drain_started_at`、`handoff_status` 和各颜色连接/任务/队列数用于定位进度。`old_version_requires_natural_drain` 表示旧服务端不支持，`handoff_check_failed_retrying` 表示新版健康/路由或控制请求未通过，后台会重试。`waiting_for_client_or_task_completion` 仅表示通知已发出，不代表所有用户已经迁移成功。

等待策略和发布状态包含在现有完整备份的 `configuration.tar` 中；恢复到另一台主机时，核对卷和路由后重新执行 `configure-drain`，重建宿主机 systemd 单元。应用镜像升级不会替换宿主机 CLI，已有机器需要先重复运行工具安装器取得支持 `configure-drain` 的版本。

Edge 宿主机使用同样策略：`sudo /opt/dreamtrans-edge/dreamtransctl edge configure-drain --handoff-after 600`。仅处理发布后的旧颜色，不会把管理员手动排空整个节点变成强制迁移。卸载前仍必须完成结果回传、吊销身份，并停用后台定时器。

```bash
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans drain
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans rollback
sudo /root/dreamtrans/dreamtransctl --dir /root/dreamtrans diagnose
sudo INSTALL_DIR=/root/dreamtrans /root/dreamtrans/backup.sh --dry-run
```

`rollback` 只回切兼容应用；不会恢复旧数据库快照或覆盖新写入。旧 `install.sh --update` 仍为标准 Compose 原地升级，检测到蓝绿目录时拒绝运行并提示使用新命令。

工具更新可重复运行上述安装器，不需要再做蓝绿初次转换。旧 Python 文件保留在原目录及历史备份中供审计，不会被 Go 工具调用；不卸载系统 Python，避免影响 Ubuntu 系统组件。

## 已有 Edge

使用同一主站发行镜像中的通用 Go 工具接管 Edge 宿主机（主站镜像只用于提取文件，不运行主站服务）：

```bash
sudo bash install-ops.sh MAIN_REPOSITORY@sha256:RELEASE_DIGEST \
  edge --dir /opt/dreamtrans-edge
sudo /opt/dreamtrans-edge/dreamtransctl edge status
sudo /opt/dreamtrans-edge/dreamtransctl edge upgrade
```

接管会把原有 `dreamtrans-edge-<prefix>` systemd 服务/定时器改为调用 Go，保持原身份、配置和各颜色独立回传队列。Edge 不运行主站备份；队列只能在主站确认回执后清理。`edge upgrade` 从主站取得该节点获准的固定发行版本；不会跟随主站镜像 `latest`。`pause-releases` 暂停自动发布，`resume-releases` 恢复。

新 Edge 继续使用管理页面生成的单行安装命令；引导脚本从指定不可变 Edge 镜像提取 Go 工具，无需安装 Python。注册和供应商凭证仍通过隐藏输入或 0600 文件传入。

创建新节点前，主站的 `EDGE_RELEASE_IMAGE` 必须指向包含 Go 工具的新 Edge 发行 digest。主站升级不会擅自修改已固定的节点发行配置；若仍指定旧的 Python 发行镜像，新引导脚本会拒绝安装，而不会回退到 Python。

## 验证与备份

完整备份格式未改变：`manifest.json`、`database.dump`、`application.tar`、`configuration.tar`，并沿用原加密口令、远端回读校验与保留策略。接管后应运行一次完整 R2 备份并做隔离恢复演练，不能只看 `--dry-run`。

CI 使用 Go race 测试覆盖状态/锁、排空、回切、身份边界和回执核对；真实 Docker 生命周期覆盖旧形态卷转换、升级、候选暂停恢复、写入保留、快照恢复和稳定入口。Python 仅用作 CI 测试驱动与历史行为对比。
