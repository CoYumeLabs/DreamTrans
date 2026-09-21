# 自动 Docker 发布

工作流统一位于 DreamTrans 仓库根目录：`.github/workflows/ci.yml` 完成两产品全部验证后，调用 `.github/workflows/docker-build.yml` 发布镜像。原 YuAction 独立工作流已移除。

推送 main、版本标签或手动运行主分支会验证并发行；PR 只验证。主站、Edge、YuAction 后端和前端采用同一提交号。全部镜像构建成功后才提升 main 的 latest，提升前重新检查远端 main。跨镜像标签更新不是原子操作；安装器按后端镜像的提交标签拉取匹配前端。

完整迁移和部署边界见 [合仓说明](../../docs/deployment/yuaction-monorepo.md)。YuAction 保留端口与独立容器，仍使用原地更新。

## 镜像与标签

```text
ghcr.io/coyumelabs/dreamtrans-yuaction-backend:latest
ghcr.io/coyumelabs/dreamtrans-yuaction-frontend:latest
```

- 每次发布都有 `sha-<完整40位commit SHA>` 标签，前后端一致，便于固定版本和回退。
- 推送 `v0.1.0` 这样的 SemVer 标签时，额外生成 `0.1.0` 镜像标签；预发布版本保留后缀。
- 镜像附带源码、提交信息、构建 provenance 和 SBOM；各组件摘要记录在 Actions 运行摘要中。
- 使用仓库的 `GITHUB_TOKEN` 和仅发布任务授予的 `packages: write`，不需要新增 Docker Hub 密码。

## 拉取与运行

推荐直接使用 [一键安装 / 更新脚本](INSTALL.md)，它会生成配置、固定匹配的前后端版本，并在更新前备份数据库。下方命令用于手动管理 Compose 部署。

服务器需要 Docker 和 Docker Compose，只需 `compose.ghcr.yml` 与 `.env`，不需要 Go / Node 或本地编译。

```bash
cp .env.example .env
# 设置 POSTGRES_PASSWORD、YUACTION_CREATOR_KEY（分别生成随机值）
# 设置 IMAGE_TAG=sha-<目标提交>，或测试时使用 latest
docker compose -f compose.ghcr.yml pull
docker compose -f compose.ghcr.yml up -d --no-build
```

默认访问 `http://127.0.0.1:11452`。对外访问可沿用本机 HTTPS 反向代理，或按部署环境调整 `APP_BIND` / `APP_PORT`。数据库不暴露宿主端口，数据保存在同一个 `yuaction_postgres` 卷中。

源代码构建用 `compose.yml`；拉取已发布镜像用 `compose.ghcr.yml`。两者使用相同 project 名、服务名、环境变量与数据卷，在同一个部署目录切换不需要删除数据库。

### 首次 GHCR 访问

这些是 DreamTrans 工作流创建的新容器包。GitHub 仓库公开不意味着首次创建的容器包自动公开。如果匿名拉取提示 denied，请将组织 Packages 中的两个镜像设为 Public，或使用拥有该包读取权限的账号登录 GHCR：

```bash
# 交互输入 GitHub 用户名，以及具有 read:packages 权限的 PAT classic。
docker login ghcr.io
```

如果组织策略禁止工作流创建包，需要管理员允许仓库使用 `GITHUB_TOKEN` 发布容器包。具体错误可在发布任务日志中查看。

## 更新与回退

修改 `.env` 的 `IMAGE_TAG`，然后执行：

```bash
docker compose -f compose.ghcr.yml pull
docker compose -f compose.ghcr.yml up -d --no-build
docker compose -f compose.ghcr.yml ps
```

回退时把 `IMAGE_TAG` 改为之前成功部署的 SHA 标签，再执行同样命令。保留数据库卷；后续出现数据库 schema 迁移时，应先核对该版本的数据兼容性。

当前自动化截止于镜像发布，不会登录服务器替换运行中的容器。服务器自动更新需要另外配置目标环境和部署凭证。

参考：[GitHub 容器注册表](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)、[Docker 多平台构建](https://docs.docker.com/build/building/multi-platform/)。
