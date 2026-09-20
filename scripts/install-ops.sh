#!/usr/bin/env bash
# Adopt an existing installation without recreating its containers or data.
set -Eeuo pipefail
[[ $(id -u) == 0 ]] || { echo '请以 root 运行'; exit 1; }
reference=ghcr.io/coyumelabs/dreamtrans:latest
if [[ $# -gt 0 && "$1" != --* ]]; then reference=$1; shift; fi
docker pull "$reference"
image=$(docker image inspect --format '{{.Id}}' "$reference")
[[ "$image" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 1
work=$(mktemp -d /tmp/dreamtrans-ops.XXXXXX)
container=$(docker create "$image")
cleanup() { docker rm -v "$container" >/dev/null 2>&1 || true; rm -rf -- "$work"; }
trap cleanup EXIT
for name in dreamtransctl backup.sh; do
    docker cp "$container:/usr/share/dreamtrans/$name" "$work/$name"
done
chmod 0700 "$work/dreamtransctl"
"$work/dreamtransctl" "$@" install-tools --backup-file "$work/backup.sh"
