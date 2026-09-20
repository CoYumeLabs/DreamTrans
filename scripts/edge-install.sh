#!/usr/bin/env bash
# Bootstrap is published with a SHA-256 digest by the authenticated main-site UI.
# All subsequent controller code is extracted from the specified immutable image.
set -Eeuo pipefail
[[ $(id -u) == 0 ]] || { echo '请以 root 运行 Edge 安装器' >&2; exit 1; }
[[ ${1:-} == *@sha256:* ]] || { echo '用法：edge-install.sh repository@sha256:digest [Edge CLI 参数]' >&2; exit 1; }
image=$1
shift
if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
    [[ -f /etc/os-release ]] || { echo '仅支持 Ubuntu Linux' >&2; exit 1; }
    . /etc/os-release
    [[ $ID == ubuntu && ( $VERSION_ID == 24.04 || $VERSION_ID == 26.04 ) ]] || { echo '自动依赖安装支持 Ubuntu 24.04/26.04；其他系统请先安装 Docker/Compose' >&2; exit 1; }
    printf '[1/6] 安装 Docker/Compose，不修改其他应用配置\n'
    apt-get update
    apt-get install -y docker.io docker-compose-v2 ca-certificates
    systemctl enable --now docker
fi
printf '[2/6] 拉取并验证不可变镜像\n'
docker pull "$image"
work=$(mktemp -d /tmp/dreamtrans-edge-bootstrap.XXXXXX)
container=$(docker create "$image")
cleanup() { docker rm -v "$container" >/dev/null 2>&1 || true; rm -rf -- "$work"; }
trap cleanup EXIT
docker cp "$container:/usr/share/dreamtrans/dreamtransctl" "$work/dreamtransctl"
chmod 0700 "$work/dreamtransctl"
"$work/dreamtransctl" edge --image "$image" "$@"
