#!/usr/bin/env bash
# Read-only samples while the browser is actively transcribing.
# Usage: bash 2026-09-23-edge-latency-collect.sh [edge-container] [local-port]
set -euo pipefail
audit_container="${1:-dreamtrans-edge-9b362559-blue}"
audit_port="${2:-16003}"
[[ "$audit_container" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || exit 2
[[ "$audit_port" =~ ^[0-9]{1,5}$ ]] || exit 2

date -u +'%Y-%m-%dT%H:%M:%SZ'
docker inspect --format 'name={{.Name}} image={{.Image}} revision={{index .Config.Labels "org.opencontainers.image.revision"}} status={{.State.Status}} restarts={{.RestartCount}} oom_killed={{.State.OOMKilled}} started={{.State.StartedAt}}' "$audit_container"
docker exec "$audit_container" /app/server deploy-control status

for audit_sample in 1 2 3 4 5 6; do
  printf '\nsample=%s ' "$audit_sample"
  date -u +'%Y-%m-%dT%H:%M:%SZ'
  docker stats --no-stream --format '{{.Name}} cpu={{.CPUPerc}} memory={{.MemUsage}} network={{.NetIO}} block_io={{.BlockIO}} pids={{.PIDs}}' "$audit_container"
  docker exec "$audit_container" /bin/sh -c '
    for f in /sys/fs/cgroup/cpu.stat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.events /sys/fs/cgroup/io.stat /proc/pressure/cpu /proc/pressure/io; do
      if [ -r "$f" ]; then printf "%s\n" "$f"; cat "$f"; fi
    done
  '
  if command -v curl >/dev/null 2>&1; then
    curl --silent --show-error --output /dev/null --max-time 3 --noproxy '*' \
      --write-out 'local_health status=%{http_code} total_seconds=%{time_total}\n' \
      "http://127.0.0.1:${audit_port}/healthz" || true
  fi
  if [[ "$audit_sample" != 6 ]]; then sleep 5; fi
done

docker exec "$audit_container" /app/server deploy-control status
printf '\nRecent structured Edge session logs (no transcript, audio or credentials):\n'
docker logs --since 30m --tail 300 --timestamps "$audit_container" 2>&1 \
  | grep -E 'edge session=' || true
