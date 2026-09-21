#!/usr/bin/env bash
# Incident recovery for faf654e -> e163d7b after a configuration-only release
# replaced the inactive color. This is deliberately NOT a generic downgrade.
set -Eeuo pipefail
umask 077
root=${1:-/root/dreamtrans}
[[ $(id -u) == 0 ]] || { echo 'Run as root' >&2; exit 1; }
test -s "$root/.bluegreen/state.json"
test -x "$root/dreamtransctl"
if ! command -v jq >/dev/null; then
    echo '[准备] 从系统软件源安装 jq，用于校验恢复状态'
    apt-get update -qq
    apt-get install -y --no-install-recommends jq
fi
for tool in docker jq flock sha256sum sort diff sync; do
    command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 1; }
done
root=$(realpath "$root")
state="$root/.bluegreen/state.json"
ctl="$root/dreamtransctl"
test -s "$state"
test -x "$ctl"
exec 9>"$root/.bluegreen/lock"
flock -n 9 || { echo 'Another lifecycle operation is running; retry later.' >&2; exit 1; }
old_ref=ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90
edge_ref=ghcr.io/coyumelabs/dreamtrans@sha256:b271112c2ef1010ce088e202465077e9f5464fc44ca340597c71284021dc1449
old_revision=e163d7b82d44bc2646f2758f82c4229f08c9e8cf
new_revision=faf654eff025e4ab2ef1fa5081b719c74e4fd01e
work=$(mktemp -d "$root/.bluegreen/recovery-e163.XXXXXX")
extract=
db_pid=
cleanup() {
    result=$?
    trap - EXIT
    if [[ -n "$extract" ]]; then docker rm -v "$extract" >/dev/null 2>&1 || true; fi
    if [[ -n "$db_pid" ]]; then
        printf 'ROLLBACK;\n\\q\n' >&8 2>/dev/null || true
        exec 8>&-
        wait "$db_pid" 2>/dev/null || true
    fi
    if [[ $result != 0 ]]; then
        echo "[停止] 恢复未完成；审计目录：$work。不要删除状态或数据库；可重跑此恢复脚本。" >&2
    fi
    exit "$result"
}
trap cleanup EXIT
echo '[1/5] 核对固定旧镜像、数据库身份与发布状态'
docker pull "$old_ref" >/dev/null
old_id=$(docker image inspect --format '{{.Id}}' "$old_ref")
docker image inspect "$old_id" | jq -e --arg rev "$old_revision" '.[0].Config.Labels | .["org.opencontainers.image.revision"]==$rev and .["org.opencontainers.image.source"]=="https://github.com/CoYumeLabs/DreamTrans"' >/dev/null
jq -e '.format==1 and ((.role // "main")=="main") and (.active=="blue" or .active=="green")' "$state" >/dev/null
active=$(jq -r '.active' "$state")
active_image=$(jq -r --arg color "$active" '.colors[$color].image' "$state")
if [[ "$active_image" == "$old_id" ]]; then
    echo '[完成] 已在指定旧版本；保留已有排空过程。'
    flock -u 9
    "$ctl" --dir "$root" status
    exit 0
fi
jq -e '.phase=="ready" or (.phase=="candidate" and .recovery_e163==true)' "$state" >/dev/null
prefix=$(jq -r '.prefix' "$state")
[[ "$prefix" =~ ^dreamtrans-[a-f0-9]{10}$ ]]
docker image inspect "$active_image" | jq -e --arg rev "$new_revision" '.[0].Config.Labels | .["org.opencontainers.image.revision"]==$rev and .["org.opencontainers.image.source"]=="https://github.com/CoYumeLabs/DreamTrans"' >/dev/null
docker inspect "$prefix-$active" | jq -e --arg image "$active_image" '.[0] | .Image==$image and .State.Running' >/dev/null
db=$(jq -r '.database_id' "$state")
db_name=$(jq -r '.database_env.PGDATABASE' "$state")
db_volume=$(jq -r '.database_volume' "$state")
app_volume=$(jq -r '.application_volume' "$state")
docker inspect "$db" | jq -e --arg id "$db" --arg vol "$db_volume" '.[0] | .Id==$id and .State.Running and ([.Mounts[]|select(.Destination=="/var/lib/postgresql/data" and .Type=="volume" and .RW and .Name==$vol)]|length)==1' >/dev/null
docker inspect "$prefix-$active" | jq -e --arg vol "$app_volume" '.[0] | ([.Mounts[]|select(.Destination=="/app/data" and .Type=="volume" and .RW and .Name==$vol)]|length)==1' >/dev/null
available=$(awk '/MemAvailable:/{print int($2/1024)}' /proc/meminfo)
[[ "$available" -ge 1536 ]] || { echo 'Insufficient memory; current service retained.' >&2; exit 1; }
for release in old current; do
    image="$old_id"
    [[ "$release" == old ]] || image="$active_image"
    extract=$(docker create "$image")
    mkdir "$work/$release"
    docker cp "$extract:/usr/share/dreamtrans/." "$work/$release" >/dev/null
    docker rm -v "$extract" >/dev/null
    extract=
done
cmp "$ctl" "$work/current/dreamtransctl"
jq -e '.protocol==1 and .state_epoch==1 and .edge_configuration==1 and .edge_protocol_min==1 and .edge_protocol_max==2 and (.provider_credentials // 0)==0' "$work/old/release.json" >/dev/null
echo '[2/5] 验证迁移完全一致；仅保留新版新增的两个索引'
for migration in "$work/old/migrations/"*.sql; do
    cmp "$migration" "$work/current/migrations/$(basename "$migration")"
done
[[ $(find "$work/old/migrations" -maxdepth 1 -name '*.sql' | wc -l) == 56 ]]
[[ $(find "$work/current/migrations" -maxdepth 1 -name '*.sql' | wc -l) == 57 ]]
printf '%s  %s\n' 5ac99cf71d12bc99080e757d3dabbe1084709fcbd0e793e8e81962f8289e8526 "$work/current/migrations/057_edge_provider_audit.sql" | sha256sum -c - >/dev/null
docker exec "$db" sh -c 'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1" -c "SELECT version,checksum FROM schema_migrations ORDER BY version"' sh "$db_name" > "$work/database-migrations"
(cd "$work/current/migrations"; sha256sum *.sql | awk '{print $2 "|" $1}' | LC_ALL=C sort) > "$work/image-migrations"
diff -u "$work/image-migrations" "$work/database-migrations"
echo '[3/5] 锁定 Edge 调度变更，拒绝存在活动节点或未结算会话的回退'
coproc RECOVERY_DB { docker exec -i "$db" sh -c 'exec psql -XqAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"' sh "$db_name" 2>"$work/edge-guard.log"; }
db_pid=$RECOVERY_DB_PID
exec 8>&"${RECOVERY_DB[1]}"
exec 7<&"${RECOVERY_DB[0]}"
cat >&8 <<'SQL'
SET lock_timeout='3s';
SET statement_timeout='5s';
BEGIN;
LOCK TABLE edge_nodes,edge_sessions IN SHARE MODE;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM edge_nodes WHERE mode NOT IN ('disabled','revoked') OR heartbeat_at>now()-interval '2 minutes')
 OR EXISTS(SELECT 1 FROM edge_sessions WHERE status<>'closed') THEN
  RAISE EXCEPTION 'Edge is still active or has unsettled sessions; refusing capability downgrade';
 END IF;
END $$;
SELECT 'EDGE_RECOVERY_LOCKED';
SQL
read -r -t 15 guard <&7
[[ "$guard" == EDGE_RECOVERY_LOCKED ]]
echo '[4/5] 保存恢复记录，准备旧版候选；当前应用继续服务'
cp -p "$state" "$work/state.before.json"
docker inspect "$prefix-$active" > "$work/active.before.json"
target=blue
[[ "$active" != blue ]] || target=green
if [[ $(jq -r '.phase' "$state") == ready ]]; then
    if docker inspect "$prefix-$target" > "$work/inactive.before.json" 2>/dev/null; then
        recorded=$(jq -r --arg color "$target" '.colors[$color].image' "$state")
        jq -e --arg image "$recorded" --arg prefix "$prefix" '.[0] | (.State.Running|not) and .Image==$image and .Config.Labels["dreamtrans.release"]==$prefix' "$work/inactive.before.json" >/dev/null
        docker rm "$prefix-$target" >/dev/null
    fi
    jq --arg active "$active" --arg target "$target" --arg image "$old_id" --arg ref "$old_ref" --arg edge "$edge_ref" --slurpfile contract "$work/old/release.json" '
      .colors[$active].application_env //= .application_env |
      .application_env.EDGE_ROUTING_ENABLED="false" |
      .application_env.EDGE_RELEASE_IMAGE=$edge |
      .colors[$target]={image:$image,contract:$contract[0],application_env:.application_env} |
      .phase="candidate" | .target=$target | .resolved_image=$ref | .recovery_e163=true
    ' "$state" > "$work/state.candidate.json"
    chmod 600 "$work/state.candidate.json"
    sync -f "$work/state.candidate.json"
    mv "$work/state.candidate.json" "$state"
    sync -f "$root/.bluegreen"
else
    jq -e --arg target "$target" --arg image "$old_id" '.target==$target and .colors[$target].image==$image and .application_env.EDGE_ROUTING_ENABLED=="false"' "$state" >/dev/null
fi
kill -0 "$db_pid"
# resume reloads state and acquires the same lifecycle lock; the database guard
# remains held until it finishes. Never falsify contracts or erase migrations.
flock -u 9
echo '[5/5] 通过原 Go 控制器检查候选、切流和排空；不恢复数据库快照'
"$ctl" --dir "$root" resume --observe 15 --drain-timeout 0
kill -0 "$db_pid"
"$ctl" --dir "$root" status
echo "恢复命令已完成；核对 active 镜像为 $old_id。审计目录：$work"
echo '已有长连接继续在新版排空；新会话进入旧版。Edge 调度保持关闭。'
