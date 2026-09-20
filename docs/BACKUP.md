# 自动备份到 Cloudflare R2

完成蓝绿转换并安装新版 `backup.sh` 和 `dreamtransctl` 后，现有定时任务自动生成
`dreamtrans-时间.full.tar.enc`。它包含控制器记录的生产数据库导出、完整正式应用卷
（包括知识库文件及仍保留的 SQLite）、主站和 `yuaction/` 下的 `.env`/`.env.*`、
Compose YAML 文件、备份/发布工具及 `.bluegreen` 状态。还保留 YuAction 的
`install.sh`、`.project`、`.dreamtrans-dir`，用于恢复原更新器和主站关联。
配置符号链接保存实际内容。
原数据库容器和 external 应用卷从状态文件读取，不按默认卷名猜测，也不重建数据卷。

数据库导出覆盖该数据库的全部 schema，包括与主站共库的 YuAction schema；
**其他数据库、YuAction 的独立文件卷、配置引用的其他目录不在这份快照内**，必须
在部署审核中单独确认并安排备份。当前生产交接应核对 YuAction 的 `dbname` 与主站一致。

每次上传后都会从 R2 下载密文并比对 SHA-256；下载失败或内容不一致时，任务失败，
保留旧备份，不执行远端或本地清理。这个检查证明上传内容一致，不能替代定期隔离恢复。
完整快照在私有临时目录内生成，完成后才原子发布，避免留下看似成功的半份备份。
手动与定时备份共用宿主机锁，已有任务运行时拒绝启动第二份，避免覆盖或提前清理。

尚未转换蓝绿的旧模式只导出指定 PostgreSQL 数据库、`.env` 和 `docker-compose.yml`，
**不包含应用卷**。`--dry-run` 会明确显示实际采用的模式；不能把旧模式当作完整应用备份。
宿主机使用随发行镜像提供的静态 Go `dreamtransctl`，不需要 Python；pg_dump 和 openssl 在数据库容器里跑，上传用
rclone 的容器镜像。

## 已迁移生产环境的交接

保留现有 `.env` 中的 R2 凭证和 `BACKUP_PASSPHRASE`，不要重新生成口令。
把已验证的新版 `backup.sh` 与 `dreamtransctl` 安装到原定时任务所调用的安装目录。
如果原任务已经调用 `/root/dreamtrans/backup.sh`，原位更新脚本即可保留执行时间，
不需要新增第二条任务。仅转换应用容器不会自动更新宿主机旧脚本。

转换完成后执行 `INSTALL_DIR=/root/dreamtrans /root/dreamtrans/backup.sh --dry-run`，
确认输出包含 application volume 和 main/YuAction configuration，再手动运行一次。
只有出现 `remote download checksum verified` 与 `full deployment backup complete`，
才能确认本次完整备份已上传并通过回读校验。失败时现有 `BACKUP_HEALTHCHECK_URL`
会收到失败通知；未配置监控地址时不会自动产生邮件告警。

## 完整快照的恢复演练

每次首次启用或存储布局变化后，以及日常运维安排的定期演练中，应执行：

1. 从 R2 下载 `.full.tar.enc`，使用原口令和下文相同的 OpenSSL 参数解密。
2. 解包到权限为 0700 的独立目录，按 `manifest.json` 的 `files` 校验三个内层文件：
   `database.dump`、`application.tar`、`configuration.tar`。
3. 在不连接生产网络、不开放公网端口的新 PostgreSQL 实例中，用
   `pg_restore --exit-on-error --no-owner --no-privileges` 恢复 `database.dump`。
4. 将 `application.tar` 恢复到全新的测试卷，保留所有者和权限；检查知识库文件。
   将 `configuration.tar` 恢复到独立目录，核对正式卷名、Compose 叠加文件、
   YuAction 网络配置及密钥文件是否齐全。不要直接启动其中保存的生产配置。
5. 以替换为隔离数据库、测试卷和测试凭证的配置启动兼容版本，核对迁移、用户/账本
   和历史记录、知识库及 YuAction。保存演练日期、镜像、校验结果和差异。

灾难恢复时，旧 `.bluegreen/state.json` 中的容器 ID、网络和主机路径不能直接套用
到新主机；须按恢复后的实际资源重建并验证映射。镜像回切不能恢复数据库旧快照覆盖新写入。

## 一次性准备

1. Cloudflare 控制台 → R2 → 创建一个存储桶，例如 `dreamtrans-backups`。
2. R2 → Manage R2 API Tokens → 创建令牌，权限选 **Object Read & Write**，
   只授权这个桶。记下 Access Key ID 和 Secret Access Key。
3. R2 概览页右侧能看到 **Account ID**。
安装脚本已经在 `~/dreamtrans/.env` 末尾留了一段注释掉的备份配置（旧安装
跑一次 `--update` 也会补上）。把前四行的 `#` 去掉并填上值：

```dotenv
R2_ACCOUNT_ID=你的账户 ID
R2_ACCESS_KEY_ID=...
R2_SECRET_ACCESS_KEY=...
R2_BUCKET=dreamtrans-backups
```

`BACKUP_PASSPHRASE` 不用填，下一步自动生成。`BACKUP_RETENTION_DAYS` 默认 30
天；`BACKUP_HEALTHCHECK_URL` 可选，填 healthchecks.io 之类的地址后，漏跑或
失败时监控会发邮件。

备份脚本只读取备份需要的配置，不会把 `.env` 当作 Shell 脚本执行。备份配置
使用单行值，支持单引号、双引号及行尾注释；值中的 `$VAR`、`${VAR}`、反引号
和 `$()` 都按字面量读取，不展开变量或执行命令。请直接填写完整的配置值。

然后再跑一次更新：

```bash
curl -fsSL https://raw.githubusercontent.com/CoYumeLabs/DreamTrans/main/scripts/install.sh | bash -s -- --update --dir ~/dreamtrans
```

更新流程检测到 R2 配置后会自动：把最新的 `backup.sh` 放进安装目录、生成一个
40 位随机口令写进 `.env` 并在屏幕上打印一次、装好每天 03:15 的定时任务。
**把打印出来的口令立刻存到这台服务器以外的地方（密码管理器）。** `.env` 里
那份会随服务器一起丢失，没有口令，备份文件无法解开。

不想等定时任务，可以立刻手动跑一次确认链路通了：

```bash
~/dreamtrans/backup.sh --dry-run
~/dreamtrans/backup.sh
~/dreamtrans/backup.sh --list
```

看到 `backup complete` 且 `--list` 列出两个文件即可。之后每天 03:15（服务器
时间）执行，日志在 `~/dreamtrans/backups/backup.log`。本地保留最近 7 份，远端
按 `BACKUP_RETENTION_DAYS` 保留。`crontab -l` 能看到定时任务；手动管理可用
`~/dreamtrans/backup.sh --install-cron`。

## 旧模式数据库恢复示例（仅在隔离环境演练）

在一台装好 Docker 的隔离测试机器上，先按安装文档拉起一个空测试实例，
然后：

```bash
cd ~/dreamtrans
# 1. 从 R2 取回文件（也可以直接在 Cloudflare 控制台下载）
docker run --rm -e RCLONE_CONFIG_R2_TYPE=s3 -e RCLONE_CONFIG_R2_PROVIDER=Cloudflare \
  -e RCLONE_CONFIG_R2_ACCESS_KEY_ID=$R2_ACCESS_KEY_ID -e RCLONE_CONFIG_R2_SECRET_ACCESS_KEY=$R2_SECRET_ACCESS_KEY \
  -e RCLONE_CONFIG_R2_ENDPOINT=https://$R2_ACCOUNT_ID.r2.cloudflarestorage.com \
  -v "$PWD/restore:/restore" rclone/rclone:1.68 copy r2:$R2_BUCKET/dreamtrans /restore

# 2. 解密
export BACKUP_PASSPHRASE=你的口令
openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass env:BACKUP_PASSPHRASE \
  -in restore/dreamtrans-20260906-031500.dump.enc -out restore/db.dump
openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass env:BACKUP_PASSPHRASE \
  -in restore/dreamtrans-20260906-031500.config.tar.enc | tar -xf - -C restore/

# 3. 停应用，清空并重建数据库，导入
docker compose stop app
docker compose exec -T db psql -U dreamtrans -d postgres -c 'DROP DATABASE dreamtrans;' -c 'CREATE DATABASE dreamtrans;'
docker compose exec -T db pg_restore -U dreamtrans -d dreamtrans --no-owner < restore/db.dump
docker compose start app
```

如果主机上没有 openssl，把解密那两步换成
`docker compose exec -T db openssl ...` 并用 `-in /dev/stdin` 从管道读入即可。

## 检查清单

- 第一次跑完后，务必在另一台机器上做一次完整恢复演练，确认口令和流程都对。
- `backup.log` 里连续出现 ERROR 时 cron 不会通知你；配置
  `BACKUP_HEALTHCHECK_URL` 让监控在漏跑或失败时发邮件。
- 更换 `BACKUP_PASSPHRASE` 后，旧备份仍需旧口令才能解开，请把新旧口令都保存好。
