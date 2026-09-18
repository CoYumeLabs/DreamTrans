# 一键安装与更新

支持 Linux AMD64 / ARM64，需要 `bash`、`curl`、`flock`（util-linux）以及访问 GitHub、GHCR 的网络。已有 Docker 时，当前用户需要访问 Docker 的权限，Compose 版本需为 2.20 或更新。

Debian / Ubuntu 缺少 Docker 时，脚本以 root 从 [Docker 官方 apt 源](https://docs.docker.com/engine/install/ubuntu/#install-using-the-apt-repository)安装 Docker 和 Compose；不会自动升级已有 Docker。其他发行版请先安装 Docker。首次安装与后续更新应使用同一系统用户，或显式指定相同的安装目录。

## 安装

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/YuAction/main/scripts/install.sh | bash
```

默认目录 `~/yuaction`，默认访问 `http://127.0.0.1:11452`，适合配合本机 HTTPS 反向代理。需要直接通过服务器 IP 访问时：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/YuAction/main/scripts/install.sh | bash -s -- --bind 0.0.0.0 --port 11452
```

此时访问 `http://服务器IP:11452`，公网正式使用请配置 HTTPS。脚本不会配置域名、证书或修改防火墙规则。

首次安装会生成独立的数据库密码、活动创建密钥，写入权限为 `600` 的 `.env`，默认关闭演示模式。查看活动创建密钥：

```bash
bash ~/yuaction/install.sh --show-key
```

在创建活动界面输入该密钥。Yufolo 接入密钥默认留空，需要接入时在 `.env` 中配置 `YUFOLO_INGEST_KEY`。不要直接更改已有数据库的 `POSTGRES_PASSWORD`，修改配置文件不会同步修改 PostgreSQL 内部密码。

也可以先下载、检查脚本，再运行：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/YuAction/main/scripts/install.sh -o /tmp/yuaction-install.sh
less /tmp/yuaction-install.sh
bash /tmp/yuaction-install.sh
```

## 更新

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/YuAction/main/scripts/install.sh | bash -s -- --update
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

安装目录保存 `.env`、`compose.ghcr.yml`、`install.sh`、`.project` 和 `backups/`；数据库实际数据保存在 Docker 的 `<项目名>_yuaction_postgres` 卷。仅复制安装目录不等于备份当前数据库，更新前生成的 `database.dump` 才是对应时刻的完整逻辑备份。

`database.dump` 是 PostgreSQL custom 格式，可用 PostgreSQL 16 的 `pg_restore --list` 检查，用 `pg_restore` 恢复到单独的空数据库验证。恢复生产数据前先停止写入并核对备份时间。回退应用版本时参考 [Docker 发布与回退说明](DOCKER_RELEASE.md)，不要用 `docker compose down -v`，该选项会删除数据库卷。

此脚本负责命令触发的安装和更新，没有后台自动更新定时任务。
