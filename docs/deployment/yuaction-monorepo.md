# YuAction 合入 DreamTrans

YuAction 源代码位于 `yuaction/`，导入源为原仓库提交 `5561118`。合并提交保留原仓库作为第二父提交，历史没有 squash。用 `git log --all --oneline` 查看原提交；导入前的路径仍按原仓库记录。原仓库和本地 `/root/AnyQA/YuAction` 没有自动删除或归档。

## 开发与发行

- DreamTrans：根目录 `backend/`、`frontend/`；YuAction：`yuaction/backend/`、`yuaction/frontend/`。两套 Go / npm 模块和运行进程保留。
- 根目录 `.github/workflows/ci.yml` 是两产品的统一检查入口，含 YuAction 完整 Go race、数据库迁移、安装器生命周期、浏览器和镜像运行时验证。
- 导入时升级 YuAction 的 pgx、Excelize 和 Go 扩展库到安全修复版本；本地后端需要 Go 1.26+，CI／Docker 固定为 1.26.5。
- 所有检查通过后才调用 `docker-build.yml`，发布同一 Git 提交的主站、Edge、YuAction 后端和前端。发行清单 `release-images.json` 记录四个固定 digest。
- YuAction 镜像为 `ghcr.io/coyumelabs/dreamtrans-yuaction-backend` 和 `ghcr.io/coyumelabs/dreamtrans-yuaction-frontend`；`sha-<完整提交>` 对应本仓库源代码。旧仓库镜像不覆盖。
- 四个镜像全部构建成功才提升 main 的 latest 标签。跨镜像标签不是原子事务；安装器先解析后端提交，再按同一 SHA 拉取前后端，不依赖两个 latest 同时变化。需要完全固定版本时使用清单中的 digest。
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

`dreamtransctl upgrade` 当前只升级 DreamTrans。YuAction 安装器仍原地重建其容器；在没有活动录音的维护时段更新，更新后主持人重新登录。代码合仓不等于两个服务已经支持联合蓝绿。

YuAction 尚未实现区域 Edge 接入或 `DeploymentHandoff` 协作迁移。主站启用区域 Edge 调度后不再接受旧 `/ws/speechmatics` 新连接，现有 YuAction 不能在这种配置下启动录音。需后续统一转录接入；本次合仓没有声称修复该功能兼容缺口。

## 后续维护

新功能与修复只向 DreamTrans 仓库提交。旧仓库停更/归档需在统一发行验证后单独处理。保留独立入口与现场权限；共享账号、转录和知识库实现，以及联合发布的状态持久化与排空，作为后续明确变更推进。
