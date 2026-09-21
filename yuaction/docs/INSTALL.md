# 一键安装与更新

支持 Linux AMD64 / ARM64，需要 `bash`、`curl`、`flock`（util-linux）以及访问 GitHub、GHCR 的网络。已有 Docker 时，当前用户需要访问 Docker 的权限，Compose 版本需为 2.20 或更新。

Debian / Ubuntu 缺少 Docker 时，脚本以 root 从 [Docker 官方 apt 源](https://docs.docker.com/engine/install/ubuntu/#install-using-the-apt-repository)安装 Docker 和 Compose；不会自动升级已有 Docker。其他发行版请先安装 Docker。首次安装与后续更新应使用同一系统用户，或显式指定相同的安装目录。

## 安装

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash
```

默认目录 `~/yuaction`，默认访问 `http://127.0.0.1:11452`，适合配合本机 HTTPS 反向代理。需要直接通过服务器 IP 访问时：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --bind 0.0.0.0 --port 11452
```

此时访问 `http://服务器IP:11452`，公网正式使用请配置 HTTPS。脚本不会配置域名、证书或修改防火墙规则。

首次安装会生成独立的数据库密码、活动创建密钥，写入权限为 `600` 的 `.env`，默认关闭演示模式。查看活动创建密钥：

```bash
bash ~/yuaction/install.sh --show-key
```

未连接 Yufolo 时，在创建活动界面输入该密钥。配置 `YUFOLO_URL` 后使用已有 Yufolo 邮箱密码登录。`YUFOLO_INGEST_KEY` 是可选的外部字幕推送凭证，内置麦克风转录不需要它。不要直接更改已有数据库的 `POSTGRES_PASSWORD`，修改配置文件不会同步修改 PostgreSQL 内部密码。

也可以先下载、检查脚本，再运行：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh -o /tmp/yuaction-install.sh
less /tmp/yuaction-install.sh
bash /tmp/yuaction-install.sh
```

## 从旧独立仓库迁移

首次切换请执行本文的新安装地址，并使用原目录参数。旧官方镜像前缀自动迁移为 `ghcr.io/coyumelabs/dreamtrans-yuaction`，端口、数据和密钥保留。自定义镜像前缀不改。安装器和 Compose 与新仓库镜像提交固定匹配。详见 [合仓说明](../../docs/deployment/yuaction-monorepo.md)。

## 安装到 DreamTrans 子目录

已有通过 Docker Compose 或蓝绿控制器部署并正在运行的 DreamTrans 时：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --dreamtrans-dir /dreamtrans
```

把 `/dreamtrans` 换成实际部署目录，例如 `/root/dreamtrans`。该目录需要有 `.env`、`docker-compose.yml`，且数据库服务名为 `db`（一键安装）或 `postgres`（仓库 Compose）。脚本根据容器的 Compose 工作目录标签识别数据库及其网络；目前要求数据库只连接一个 Docker 网络。

```text
/dreamtrans/
├── .env                  # 原数据库用户名、密码、库名；只读使用
├── docker-compose.yml    # 原 DreamTrans 部署；保持不变
└── yuaction/
    ├── .env              # YuAction 端口、密钥、镜像版本和网络定位
    ├── compose.ghcr.yml   # 关联模式的部署文件
    ├── .dreamtrans-dir   # 记录父目录，更新时自动沿用
    └── install.sh
```

这个模式复用**同一个 PostgreSQL 容器、数据库和数据库账号**。首次安装只新增 `yuaction` schema，应用连接的 `search_path` 限定为 `yuaction`，所以 `rooms` 等表不会建到 DreamTrans 的 `public` schema。schema 只是表的命名和迁移边界；因为共用账号，它不是数据库权限隔离。

父 `.env` 由 Docker Compose 解析，不作为 Shell 执行。只把 `POSTGRES_USER`、`POSTGRES_DB`、`POSTGRES_PASSWORD` 用于数据库连接，不把 JWT、模型服务或支付密钥传给 YuAction 容器。数据库密码不复制到子目录；每次执行安装 / 更新会读取父配置。两个产品自己的 `IMAGE_TAG` 独立，YuAction 的值覆盖父配置中的同名值。该行为基于 [Compose 多 env 文件规则](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/#additional-information-1)。

更新、查看状态和活动创建密钥：

```bash
bash /dreamtrans/yuaction/install.sh --dir /dreamtrans/yuaction --update
bash /dreamtrans/yuaction/install.sh --dir /dreamtrans/yuaction --status
bash /dreamtrans/yuaction/install.sh --dir /dreamtrans/yuaction --show-key
```

此模式不启动第二个数据库，不更改父配置，也不重启 DreamTrans 服务。数据库配置修改后，执行 YuAction 更新让容器重新读取配置；修改父 `.env` 密码仍需先完成数据库内部的密码变更。

更新备份只包含 `yuaction` schema 和 YuAction 配置，不包含 DreamTrans 业务表或父 `.env`。DreamTrans 原有备份需要继续保留；若整库恢复 DreamTrans，也会影响同库的 YuAction 数据。两个应用共享数据库可用性，DreamTrans 数据库停止时 YuAction 也无法读写。

已有独立安装不能直接加此选项切换数据库。请保留原安装和备份，另做数据迁移；本脚本不会自动搬运旧房间。已有 `yuaction` schema 却缺少原安装配置时也会停止，避免用新密钥接管旧数据。

安装 / 更新时还会识别同一部署下的 `app` 或 `dreamtrans` 应用容器，把其内网地址写为子 `.env` 的 `YUFOLO_URL`。不需要复制父配置的 JWT 签名密钥。蓝绿模式识别本目录的固定代理并使用 `http://dreamtrans:8080`；缺少共享网络入口时先运行 `dreamtransctl sync-entry`。标准模式应用容器未运行时保留已有地址，首次安装需手动配置。独立部署也可手动设置 `YUFOLO_URL=https://你的Yufolo服务地址`，后端需可访问该地址。

主持人通过 HTTPS 或 localhost 打开 YuAction，使用现有 Yufolo 邮箱密码登录，创建活动后选择语言并点击「开始转录」。反向代理需支持 WebSocket Upgrade，前端镜像已配置；外层代理也需透传升级请求。登录会话目前保存在 YuAction 服务端内存中，服务重启后重新登录即可找回账号所属活动。详见 [联动说明](YUFOLO_INTEGRATION.md)。

## 更新

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/yuaction/scripts/install.sh | bash -s -- --update
```

重复安装命令也会更新已有安装；`--update` 则要求目录中已经有安装。更新默认跟随最近通过 CI 并发布的 `latest`，保留现有密钥、端口、绑定地址和数据。不要在更新时换目录或系统用户。

执行顺序：

1. 从发布镜像解析提交号，将前后端固定到同一个 `sha-<完整提交号>`，下载该提交对应的 Compose 文件。
2. 拉取所有镜像、校验版本；下载失败或版本不匹配时不替换运行中的服务。
3. 将原配置、安装脚本和 `pg_dump` 数据库备份存到 `backups/<UTC时间>-<进程号>/`；备份失败时停止更新。
4. 更新配置并启动容器，等待数据库和 HTTP API 健康检查通过。

更新可能短暂中断服务。备份不会自动删除，请按需要保留并复制到其他主机；备份中含密钥和业务数据。脚本不会自动执行数据库恢复或版本回退。若新服务启动失败，可先查看日志并按对应版本的数据兼容性决定回退方案。

## 常用选项

```bash
# 自定义目录与端口（所有后续操作继续传相同 --dir）
bash /tmp/yuaction-install.sh --dir /opt/yuaction --port 8080

# 更新自定义目录中的安装
bash /opt/yuaction/install.sh --dir /opt/yuaction --update

# 固定到已发布版本；也接受 sha-<完整提交号>
bash ~/yuaction/install.sh --update --version 0.1.0

# 查看状态 / 最近日志
bash ~/yuaction/install.sh --status
bash ~/yuaction/install.sh --logs

# 禁止脚本安装 Docker
bash /tmp/yuaction-install.sh --no-docker-install
```

版本标签必须已经发布，示例 `0.1.0` 不代表该标签当前存在。再次运行不带 `--version` 的更新命令会重新跟随 `latest`。

同一台服务器安装多个实例时，每个实例使用不同的 `--dir`、`--project` 和 `--port`；项目名首次安装后会保存，更新自动沿用。脚本拒绝接管其他目录的同名项目，以及没有原始 `.env` 的已有数据卷。

`.env` 使用不带引号的单行 `KEY=value` 格式；保留脚本生成的随机密码即可。脚本不会把 `.env` 当作 Shell 执行。

## 备份与恢复

独立模式下，安装目录保存 `.env`、`compose.ghcr.yml`、`install.sh`、`.project` 和 `backups/`；数据库实际数据保存在 Docker 的 `<项目名>_yuaction_postgres` 卷。仅复制安装目录不等于备份当前数据库，更新前生成的 `database.dump` 才是对应时刻的完整逻辑备份。关联 DreamTrans 模式则使用父数据库的数据卷，`database.dump` 只备份 `yuaction` schema。

`database.dump` 是 PostgreSQL custom 格式，可用 PostgreSQL 16 的 `pg_restore --list` 检查，用 `pg_restore` 恢复到单独的空数据库验证。恢复生产数据前先停止写入并核对备份时间。回退应用版本时参考 [Docker 发布与回退说明](DOCKER_RELEASE.md)，不要用 `docker compose down -v`，该选项会删除数据库卷。

此脚本负责命令触发的安装和更新，没有后台自动更新定时任务。

## 蓝绿发布和录音接续

新版安装器使用 Go 运维工具管理蓝绿，入口端口保持不变。已采用蓝绿的安装不要再使用 `docker compose up` 重建应用；Compose 继续保留原数据库定义与备份工具。

旧 Compose 安装首次转换前，结束旧页面的录音，在原安装命令增加 `--adopt-bluegreen`；转换后重新打开新版页面并登录一次。旧页面和旧内存登录态不具备迁移协议，首次转换不承诺无感。之后升级与回滚会保持同一次麦克风授权和采音，自动缓存并接续交接期间的音频。

```bash
bash ~/yuaction/install.sh --update
~/yuaction/dreamtransctl yuaction --dir ~/yuaction status
~/yuaction/dreamtransctl yuaction --dir ~/yuaction rollback
~/yuaction/dreamtransctl yuaction --dir ~/yuaction resume
~/yuaction/dreamtransctl yuaction --dir ~/yuaction drain
```

`rollback` 保留数据库里的新录音、字幕与提问。升级失败不会停止仍持有录音或后台任务的旧实例。创建密钥用于加密共享登录态，保持原 `YUACTION_CREATOR_KEY`；不要在版本更新时轮换它。完整恢复需要保留原数据库、`.env` 与 `.bluegreen` 状态，不能只恢复镜像标签。
