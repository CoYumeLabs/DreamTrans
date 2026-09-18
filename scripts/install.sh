#!/usr/bin/env bash
# YuAction installer. Keep main() at the end so a truncated pipe cannot execute
# a partially downloaded script. User configuration is data, never shell code.
set -Eeuo pipefail
umask 077

repository="CoYumeLabs/YuAction"
install_dir="${YUACTION_INSTALL_DIR:-${HOME}/yuaction}"
project="yuaction"
project_given=false
action="install"
requested_tag="latest"
requested_port=""
requested_bind=""
allow_docker_install=true
work_dir=""
backup_dir=""

log() { printf '[YuAction] %s\n' "$*"; }
die() { printf '[YuAction] 错误：%s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'USAGE'
YuAction 一键安装 / 更新

  bash install.sh                         首次安装；已安装则更新
  bash install.sh --update                更新现有安装
  bash install.sh --version 0.1.0          安装 / 更新到指定镜像版本
  bash install.sh --status                查看容器状态
  bash install.sh --logs                  查看最近 100 行日志
  bash install.sh --show-key              显示活动创建密钥

选项：
  --dir PATH        安装目录，默认 $HOME/yuaction
  --port PORT       首次安装端口，默认 11452；更新默认保留
  --bind ADDRESS    首次安装绑定地址，默认 127.0.0.1；对外访问可用 0.0.0.0
  --project NAME    Docker Compose 项目名，默认 yuaction；安装后不可变更
  --version TAG     latest、完整 sha-提交号 或版本标签；默认跟随 latest
  --no-docker-install  Docker 缺失时直接报错，不安装系统软件
  --help            显示帮助

支持 Linux AMD64 / ARM64。Debian / Ubuntu 上缺少 Docker 时从官方 apt 源安装，
需要 root。其他发行版需预先安装 Docker 与 Compose v2（2.20+）。
更新前会备份配置及数据库；不会执行 down -v、清理镜像或删除数据卷。
USAGE
}

parse_args() {
  while (($#)); do
    case "$1" in
      --update) action="update"; shift ;;
      --status) action="status"; shift ;;
      --logs) action="logs"; shift ;;
      --show-key) action="key"; shift ;;
      --no-docker-install) allow_docker_install=false; shift ;;
      --dir|--port|--bind|--project|--version)
        (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || die "$1 缺少参数"
        case "$1" in
          --dir) install_dir="$2" ;;
          --port) requested_port="$2" ;;
          --bind) requested_bind="$2" ;;
          --project) project="$2"; project_given=true ;;
          --version) requested_tag="${2#v}" ;;
        esac
        shift 2 ;;
      --help|-h) usage; exit 0 ;;
      *) die "未知参数：$1（使用 --help 查看帮助）" ;;
    esac
  done
  [[ "$project" =~ ^[a-z0-9][a-z0-9_-]{0,49}$ ]] || die "项目名只能包含小写字母、数字、横线与下划线"
  [[ "$requested_tag" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]] || die "镜像标签格式不正确"
  [[ -n "$install_dir" && "$install_dir" != / ]] || die "请指定独立安装目录"
}

cleanup() {
  local result=$?
  trap - EXIT
  if [[ -n "$work_dir" && -d "$work_dir" ]]; then rm -rf -- "$work_dir"; fi
  if ((result != 0)) && [[ -n "$backup_dir" ]]; then
    printf '[YuAction] 更新未完成，已有备份保留在：%s\n' "$backup_dir" >&2
  fi
  exit "$result"
}

fetch() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --retry 3 --connect-timeout 15 --max-time 120 "$1" -o "$2"
}

env_value() {
  # Generated .env values are unquoted single-line values. Do not source .env.
  awk -v key="$1" 'index($0, key "=") == 1 {sub(/^[^=]*=/, ""); sub(/\r$/, ""); print; exit}' "$2"
}
set_env_value() {
  local key="$1" value="$2" file="$3"
  awk -v key="$key" -v value="$value" '
    index($0, key "=") == 1 {if (!done) print key "=" value; done=1; next}
    {print}
    END {if (!done) print key "=" value}
  ' "$file" > "$file.new"
  mv -- "$file.new" "$file"
}
random_key() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }

ensure_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    $allow_docker_install || die "请先安装 Docker 和 Docker Compose v2"
    [[ "$EUID" == 0 ]] || die "缺少 Docker。请以 root 运行此命令，或先安装 Docker。"
    [[ -f /etc/os-release ]] || die "无法识别系统，请先安装 Docker"
    local distro codename arch
    distro=$(awk -F= '$1=="ID" {gsub(/"/, "", $2); print $2}' /etc/os-release)
    codename=$(awk -F= '$1=="VERSION_CODENAME" {gsub(/"/, "", $2); print $2}' /etc/os-release)
    [[ "$distro" == ubuntu || "$distro" == debian ]] || die "此系统请先安装 Docker 与 Compose v2，再运行本脚本"
    [[ "$codename" =~ ^[a-z]+$ ]] || die "无法确定系统版本，请先安装 Docker"
    log "从 Docker 官方 apt 源安装 Docker 与 Compose…"
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl
    install -m 0755 -d /etc/apt/keyrings
    fetch "https://download.docker.com/linux/$distro/gpg" "$work_dir/docker.asc"
    install -m 0644 "$work_dir/docker.asc" /etc/apt/keyrings/docker.asc
    arch=$(dpkg --print-architecture)
    cat > /etc/apt/sources.list.d/yuaction-docker.sources <<APT
Types: deb
URIs: https://download.docker.com/linux/$distro
Suites: $codename
Components: stable
Architectures: $arch
Signed-By: /etc/apt/keyrings/docker.asc
APT
    chmod 0644 /etc/apt/sources.list.d/yuaction-docker.sources
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    systemctl enable --now docker
  fi
  docker info >/dev/null 2>&1 || die "Docker 未运行或当前用户无权访问；请启动 Docker 并确认访问权限"
  local version major minor
  version=$(docker compose version --short 2>/dev/null) || die "已有 Docker，但缺少 Compose v2 插件；请先安装 docker-compose-plugin"
  version="${version#v}"
  [[ "$version" =~ ^([0-9]+)\.([0-9]+) ]] || die "无法识别 Docker Compose 版本"
  major="${BASH_REMATCH[1]}"; minor="${BASH_REMATCH[2]}"
  ((major > 2 || (major == 2 && minor >= 20))) || die "需要 Docker Compose 2.20 或更新版本"
}

compose_at() {
  local config_dir="$1"; shift
  # A caller's exported variables must not override saved database credentials.
  env -u POSTGRES_PASSWORD -u YUACTION_CREATOR_KEY -u YUFOLO_INGEST_KEY \
    -u YUACTION_DEMO -u APP_BIND -u APP_PORT -u IMAGE_TAG -u IMAGE_PREFIX \
    -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_ENV_FILES \
    docker compose --project-name "$project" --project-directory "$install_dir" \
      --env-file "$config_dir/.env" -f "$config_dir/compose.ghcr.yml" "$@"
}

check_project_owner() {
  local ids first working_dir
  ids=$(docker ps -aq --filter "label=com.docker.compose.project=$project")
  if [[ -n "$ids" ]]; then
    first="${ids%%$'\n'*}"
    working_dir=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$first")
    [[ "$working_dir" == "$install_dir" ]] || die "项目 $project 已属于 $working_dir；使用原安装目录，或指定其他 --project"
  fi
  if [[ ! -f "$install_dir/.env" ]] && docker volume inspect "${project}_yuaction_postgres" >/dev/null 2>&1; then
    die "已有数据库卷但缺少 .env；请恢复原配置，不能重新生成数据库密码"
  fi
}

validate_config() {
  local file="$1" port bind password creator prefix
  password=$(env_value POSTGRES_PASSWORD "$file")
  creator=$(env_value YUACTION_CREATOR_KEY "$file")
  [[ -n "$password" && ${#creator} -ge 32 ]] || die "现有 .env 缺少数据库密码或有效的活动创建密钥；请恢复配置"
  port=$(env_value APP_PORT "$file"); port="${port:-11452}"
  [[ "$port" =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || die "端口应为 1–65535"
  bind=$(env_value APP_BIND "$file"); bind="${bind:-127.0.0.1}"
  [[ "$bind" =~ ^[0-9a-fA-F:.]+$ ]] || die "绑定地址应为 IP 地址"
  prefix=$(env_value IMAGE_PREFIX "$file"); prefix="${prefix:-ghcr.io/coyumelabs/yuaction}"
  [[ "$prefix" =~ ^[a-z0-9][a-z0-9./_-]+$ ]] || die "IMAGE_PREFIX 格式不正确"
}

wait_for_api() {
  local bind port url
  bind=$(env_value APP_BIND "$install_dir/.env"); bind="${bind:-127.0.0.1}"
  port=$(env_value APP_PORT "$install_dir/.env"); port="${port:-11452}"
  case "$bind" in 0.0.0.0) bind=127.0.0.1 ;; ::) bind=::1 ;; esac
  [[ "$bind" != *:* ]] || bind="[$bind]"
  url="http://$bind:$port/api/health"
  local i
  for i in {1..30}; do
    if curl --fail --silent --noproxy '*' --max-time 3 "$url" > "$work_dir/health.json"; then
      if grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' "$work_dir/health.json"; then return 0; fi
    fi
    sleep 2
  done
  die "服务未通过健康检查。运行 bash '$install_dir/install.sh' --logs --dir '$install_dir' --project '$project' 查看日志。"
}

install_or_update() {
  local existing=false prefix revision backend_revision frontend_revision previous_tag stamp
  if [[ -f "$install_dir/.env" ]]; then
    [[ -f "$install_dir/compose.ghcr.yml" ]] || die "已有 .env 但缺少 compose.ghcr.yml；请恢复原部署文件"
    existing=true
    log "更新现有安装：$install_dir（保留配置与数据库）"
    cp -- "$install_dir/.env" "$work_dir/.env"
  else
    [[ "$action" != update ]] || die "指定目录尚未安装 YuAction，请先运行安装命令"
    log "首次安装：$install_dir"
    cat > "$work_dir/.env" <<ENV
POSTGRES_PASSWORD=$(random_key)
YUACTION_CREATOR_KEY=$(random_key)
YUFOLO_INGEST_KEY=
YUACTION_DEMO=false
APP_BIND=127.0.0.1
APP_PORT=11452
IMAGE_TAG=latest
ENV
  fi
  [[ -z "$requested_port" ]] || set_env_value APP_PORT "$requested_port" "$work_dir/.env"
  [[ -z "$requested_bind" ]] || set_env_value APP_BIND "$requested_bind" "$work_dir/.env"
  validate_config "$work_dir/.env"
  prefix=$(env_value IMAGE_PREFIX "$work_dir/.env"); prefix="${prefix:-ghcr.io/coyumelabs/yuaction}"

  # Resolve ONE published revision, then pull both components by that SHA. The
  # two mutable latest tags need not be promoted at exactly the same instant.
  log "解析已发布版本：$requested_tag"
  docker pull "${prefix}-backend:${requested_tag}" || die "镜像拉取失败，当前服务保持不变"
  revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "${prefix}-backend:${requested_tag}")
  [[ "$revision" =~ ^[a-f0-9]{40}$ ]] || die "镜像缺少有效提交信息，停止更新"
  set_env_value IMAGE_TAG "sha-$revision" "$work_dir/.env"
  log "下载与镜像版本匹配的部署文件：${revision:0:7}"
  fetch "https://raw.githubusercontent.com/$repository/$revision/compose.ghcr.yml" "$work_dir/compose.ghcr.yml"
  fetch "https://raw.githubusercontent.com/$repository/main/scripts/install.sh" "$work_dir/install.sh"
  bash -n "$work_dir/install.sh"
  compose_at "$work_dir" config --quiet
  compose_at "$work_dir" pull || die "新版镜像未全部拉取成功，当前服务保持不变"
  backend_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "${prefix}-backend:sha-$revision")
  frontend_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "${prefix}-frontend:sha-$revision")
  [[ "$backend_revision" == "$revision" && "$frontend_revision" == "$revision" ]] || die "前后端镜像版本不一致，停止更新"

  if $existing; then
    stamp="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    backup_dir="$install_dir/backups/$stamp"
    mkdir -p -- "$backup_dir"
    cp -- "$install_dir/.env" "$backup_dir/.env"
    cp -- "$install_dir/compose.ghcr.yml" "$backup_dir/compose.ghcr.yml"
    [[ ! -f "$install_dir/install.sh" ]] || cp -- "$install_dir/install.sh" "$backup_dir/install.sh"
    log "备份数据库与配置：$backup_dir"
    compose_at "$install_dir" up -d --no-recreate --wait --wait-timeout 120 db
    compose_at "$install_dir" exec -T db pg_dump -U yuaction -d yuaction -Fc > "$backup_dir/database.dump" || die "数据库备份失败，停止更新"
    [[ -s "$backup_dir/database.dump" ]] || die "数据库备份为空，停止更新"
    previous_tag=$(env_value IMAGE_TAG "$install_dir/.env")
    printf '%s\n' "$previous_tag" > "$backup_dir/image-tag.txt"
  fi

  install -m 0600 "$work_dir/.env" "$install_dir/.env"
  install -m 0600 "$work_dir/compose.ghcr.yml" "$install_dir/compose.ghcr.yml"
  install -m 0700 "$work_dir/install.sh" "$install_dir/install.sh"
  printf '%s\n' "$project" > "$install_dir/.project"
  log "启动 YuAction…"
  compose_at "$install_dir" up -d --no-build --wait --wait-timeout 120
  wait_for_api
  log "安装 / 更新成功：${revision:0:7}"
  local bind port
  bind=$(env_value APP_BIND "$install_dir/.env"); bind="${bind:-127.0.0.1}"
  port=$(env_value APP_PORT "$install_dir/.env"); port="${port:-11452}"
  if [[ "$bind" == 0.0.0.0 || "$bind" == :: ]]; then
    log "访问：http://<服务器IP>:$port（请确保网络允许访问该端口）"
  else
    [[ "$bind" != *:* ]] || bind="[$bind]"
    log "访问：http://$bind:$port"
  fi
  log "活动创建密钥已保存到 $install_dir/.env"
  log "查看密钥：bash '$install_dir/install.sh' --dir '$install_dir' --project '$project' --show-key"
  log "再次运行同一安装命令即可更新。"
}

main() {
  parse_args "$@"
  [[ "$(uname -s)" == Linux ]] || die "此脚本用于 Linux 服务器；其他系统请使用仓库中的 Compose 文件"
  case "$(uname -m)" in x86_64|aarch64|arm64) ;; *) die "目前镜像支持 AMD64 和 ARM64" ;; esac
  command -v curl >/dev/null 2>&1 || die "需要 curl；请先安装 curl 后重试"
  command -v flock >/dev/null 2>&1 || die "需要 flock（util-linux 软件包）"
  [[ ! -L "$install_dir" ]] || die "安装目录不能是符号链接"
  if [[ -d "$install_dir" && ! -f "$install_dir/.env" ]]; then
    local entry
    for entry in "$install_dir"/* "$install_dir"/.[!.]* "$install_dir"/..?*; do
      [[ ! -e "$entry" || "${entry##*/}" == .install.lock ]] || die "请使用空目录，或包含现有 .env 的 YuAction 安装目录"
    done
  fi
  mkdir -p -- "$install_dir"
  install_dir=$(cd -- "$install_dir" && pwd -P)
  [[ "$install_dir" != / ]] || die "不能使用系统根目录安装"
  chmod 0700 "$install_dir"
  exec 9> "$install_dir/.install.lock"
  flock -n 9 || die "另一个安装 / 更新进程正在操作此目录"
  local managed_file
  for managed_file in .env compose.ghcr.yml install.sh .project backups; do
    [[ ! -L "$install_dir/$managed_file" ]] || die "安装文件不能是符号链接：$managed_file"
  done
  if [[ -f "$install_dir/.project" ]]; then
    local saved_project
    saved_project=$(cat "$install_dir/.project")
    [[ "$saved_project" =~ ^[a-z0-9][a-z0-9_-]{0,49}$ ]] || die "保存的项目名无效"
    if $project_given; then
      [[ "$saved_project" == "$project" ]] || die "--project 与现有安装不一致"
    else
      project="$saved_project"
    fi
  fi
  if [[ "$action" == key ]]; then
    [[ -f "$install_dir/.env" ]] || die "此目录尚未安装"
    env_value YUACTION_CREATOR_KEY "$install_dir/.env"
    return
  fi
  work_dir=$(mktemp -d "$install_dir/.install.XXXXXXXX")
  trap cleanup EXIT
  ensure_docker
  check_project_owner
  case "$action" in
    status|logs)
      [[ -f "$install_dir/.env" && -f "$install_dir/compose.ghcr.yml" ]] || die "此目录尚未安装"
      if [[ "$action" == status ]]; then compose_at "$install_dir" ps; else compose_at "$install_dir" logs --tail 100; fi ;;
    install|update) install_or_update ;;
  esac
}

main "$@"
