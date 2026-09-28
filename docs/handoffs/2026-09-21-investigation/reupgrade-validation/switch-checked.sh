#!/usr/bin/env bash
# Specific candidate experiment only. This is not a universal rollback wrapper.
(
set -Eeuo pipefail
dt_root=${1:-/root/dreamtrans}
dt_ctl=$dt_root/dreamtransctl
dt_state=$dt_root/.bluegreen/state.json
dt_status=$("$dt_ctl" --dir "$dt_root" status)
if ! jq -e '
  .phase=="candidate" and .active=="green" and .target=="blue" and
  .colors.green.mode=="active" and .colors.blue.mode=="standby" and
  all(.colors[]; .requests==0 and .websockets==0 and .tasks==0 and .pending==0)
' <<<"$dt_status" >/dev/null; then
  printf '%s\n' 'STOP: candidate state changed or unfinished work exists.' "$dt_status"
  exit 1
fi
dt_old=$(docker image inspect --format '{{.Id}}' ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90)
dt_new=$(docker image inspect --format '{{.Id}}' ghcr.io/coyumelabs/dreamtrans@sha256:a6fcde016bec5d69d78f4bdf0fcdbb9bf85df3c6755f4c1c6b529bbb3661b64d)
jq -e --arg old "$dt_old" --arg new "$dt_new" '
  .colors.green.image==$old and .colors.blue.image==$new and
  .application_env.EDGE_ROUTING_ENABLED=="false" and
  .colors.blue.application_env.EDGE_ROUTING_ENABLED=="false"
' "$dt_state" >/dev/null
dt_port=$(jq -er '.port' "$dt_state")
dt_route=$(curl -q -fsS --noproxy '*' --connect-timeout 2 --max-time 3 "http://127.0.0.1:$dt_port/_release")
[[ $dt_route == green ]]
dt_memory=$(awk '/MemAvailable:/{print int($2/1024)}' /proc/meminfo)
[[ $dt_memory -ge 1536 ]] || { echo 'STOP: recovery requires at least 1536 MiB available.'; exit 1; }
dt_db=$(jq -er '.database_id' "$dt_state")
dt_dbname=$(jq -er '.database_env.PGDATABASE' "$dt_state")
dt_sql="BEGIN READ ONLY; SET LOCAL statement_timeout='5s'; SELECT (SELECT count(*) FROM edge_nodes WHERE mode NOT IN ('disabled','revoked') OR heartbeat_at>now()-interval '2 minutes'), (SELECT count(*) FROM edge_sessions WHERE status<>'closed'); ROLLBACK;"
dt_edge=$(docker exec "$dt_db" sh -c 'exec psql -XqAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1" -c "$2"' sh "$dt_dbname" "$dt_sql")
printf 'Edge blocking_nodes|unsettled_sessions=%s; MemAvailable=%s MiB\n' "$dt_edge" "$dt_memory"
[[ $dt_edge == '0|0' ]] || { echo 'STOP: Edge prevents the verified recovery path.'; exit 1; }
echo 'Preflight passed. Switching this candidate once; use external latency observation.'
if ! "$dt_ctl" --dir "$dt_root" resume --observe 0 --drain-timeout 0; then
  "$dt_ctl" --dir "$dt_root" status
  exit 1
fi
"$dt_ctl" --dir "$dt_root" status
)
