#!/usr/bin/env bash
# Read-only DreamTrans latency collector; no application/deployment changes.
set +x
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Usage: bash dt-latency-readonly-20260921.sh [--dir /root/dreamtrans]
       [--public-url https://yufolo.com | --no-public] [--rounds 1..5]
       [--interval 1..30]

Creates a private /tmp/dt-latency-readonly.XXXXXXXX directory. Default: three
rounds, 3 seconds between rounds, public URL from state APP_BASE_URL or
https://yufolo.com. Requires Bash, jq, curl, docker, timeout and coreutils.
Run on the production Docker host with permission to read .bluegreen/state.json.
Does not install dependencies, read/source .env, change admission mode, restart
containers, write SQL data, fetch credentials, or collect request bodies/logs.

Each round probes four public GET paths concurrently through the recorded
active container IPv4, the verified loopback proxy, and optional HTTPS origin.
Each curl is a fresh connection, max 15 seconds; ambient curlrc/proxies disabled;
redirects are not followed. HTTP errors/timeouts are recorded as failures.
PG snapshots use verified database container + state credentials via stdin only,
with read-only sessions, a 3-second statement timeout and 1-second lock timeout.
Only wait types/counts/ages and backend PIDs are collected, no SQL/user content.

Limitations: does not exercise authenticated APIs, real audio or browser HTTP/3;
public probes originate from this host. Fast probes cannot rule out intermittent
failures. PG role privileges can hide other sessions; scope is current database.
Pool waits and Go goroutine stacks are unavailable on existing app binaries.
Do not run during a release; changed identity/routing causes collection to stop.
EOF
}
fail() { printf 'ERROR: %s\n' "$1" >&2; exit 1; }
note() { printf '%s\n' "$1" >&2; }
root=/root/dreamtrans
public_override=
no_public=0
rounds=3
interval=3
while (($#)); do
  case "$1" in
    --dir|--public-url|--rounds|--interval)
      (($# >= 2)) || fail 'option requires a value'
      case "$1" in
        --dir) root=$2 ;;
        --public-url) public_override=$2 ;;
        --rounds) rounds=$2 ;;
        --interval) interval=$2 ;;
      esac
      shift 2 ;;
    --no-public) no_public=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) fail 'unknown option; use --help' ;;
  esac
done
[[ $rounds =~ ^[1-5]$ ]] || fail 'rounds must be 1..5'
[[ $interval =~ ^([1-9]|[12][0-9]|30)$ ]] || fail 'interval must be 1..30'
[[ $root == /* && $root != *$'\n'* && $root != *$'\r'* ]] || fail 'installation directory must be an absolute path'
for dep in jq curl docker timeout mktemp date cat sleep chmod; do
  command -v "$dep" >/dev/null 2>&1 || fail "required command missing: $dep (nothing was installed)"
done
state=$root/.bluegreen/state.json
[[ -r $state && -f $state ]] || fail 'cannot read installation .bluegreen/state.json'
# No raw state or complete docker inspect is ever printed or saved.
validate_state() {
  jq -e '
    .format == 1 and (.active == "blue" or .active == "green") and
    (.phase == "ready" or .phase == "draining" or .phase == "observing") and
    (.prefix | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,100}$")) and
    (.database_id | type == "string" and test("^[0-9a-f]{64}$")) and
    (.database_volume | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9_.-]*$")) and
    (.application_volume | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9_.-]*$")) and
    (.network | type == "string" and test("^[a-zA-Z0-9][a-zA-Z0-9_.-]*$")) and
    (.database_image | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    (.proxy_image | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    (.colors[.active].image | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    (.port | type == "number" and . >= 1 and . <= 65535 and floor == .)
  ' "$state" >/dev/null 2>&1
}
validate_state || fail 'unsupported, incomplete or changing deployment state; no probes sent'
state_metadata() {
  jq -cS '{prefix,active,phase,port,network,database_id,database_volume,application_volume,database_image,proxy_image,active_image:.colors[.active].image}' "$state" 2>/dev/null
}
baseline=$(state_metadata) || fail 'cannot extract state metadata'
prefix=$(jq -r '.prefix' <<<"$baseline")
active=$(jq -r '.active' <<<"$baseline")
port=$(jq -r '.port' <<<"$baseline")
network=$(jq -r '.network' <<<"$baseline")
db=$(jq -r '.database_id' <<<"$baseline")
app=$prefix-$active
proxy=$prefix-proxy
public_url=
if ((no_public == 0)); then
  if [[ -n $public_override ]]; then
    public_url=$public_override
  else
    public_url=$(jq -er '.application_env.APP_BASE_URL // "https://yufolo.com"' "$state" 2>/dev/null) || fail 'invalid APP_BASE_URL; use --public-url or --no-public'
  fi
  [[ $public_url =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)/?$ ]] || fail 'public URL must be HTTPS hostname only, no credentials/port/path/query'
  public_host=${BASH_REMATCH[1]}
  [[ ${#public_host} -le 253 && $public_host != *..* ]] || fail 'invalid public hostname'
  IFS=. read -r -a labels <<<"$public_host"
  for label in "${labels[@]}"; do
    [[ ${#label} -le 63 && $label =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ ]] || fail 'invalid public hostname label'
  done
  public_url=https://$public_host
else
  public_host=localhost
fi
out=$(mktemp -d /tmp/dt-latency-readonly.XXXXXXXX) || fail 'cannot create private output directory'
chmod 700 "$out"
printf '%s\n' "$baseline" > "$out/state-metadata.json"
printf '%s\n' "$out" > /dev/stderr
interrupt_children() {
  local child
  while IFS= read -r child; do kill "$child" 2>/dev/null || true; done < <(jobs -pr)
  exit 130
}
trap interrupt_children INT TERM
# Explicit safe Docker fields: never .Config.Env, full labels or HostConfig.
inspect_template='{"id":{{json .Id}},"name":{{json .Name}},"image":{{json .Image}},"release":{{json (index .Config.Labels "dreamtrans.release")}},"running":{{json .State.Running}},"status":{{json .State.Status}},"started":{{json .State.StartedAt}},"pid":{{json .State.Pid}},"restart_count":{{json .RestartCount}},"nano_cpus":{{json .HostConfig.NanoCpus}},"cpu_quota":{{json .HostConfig.CpuQuota}},"cpu_period":{{json .HostConfig.CpuPeriod}},"memory":{{json .HostConfig.Memory}},"memory_swap":{{json .HostConfig.MemorySwap}},"mounts":{{json .Mounts}},"networks":{{json .NetworkSettings.Networks}},"ports":{{json .NetworkSettings.Ports}}}'
inspect_safe() {
  timeout 8 docker inspect --type container --format "$inspect_template" "$1" 2>/dev/null |
    jq '{id,name,image,release,running,status,started,pid,restart_count,nano_cpus,cpu_quota,cpu_period,memory,memory_swap,mounts:[.mounts[]|{Type,Name,Source,Destination,RW}],networks:(.networks|with_entries(.value={IPAddress:.value.IPAddress})),ports}' 2>/dev/null
}
verify_containers() {
  local dest=$1
  [[ $(state_metadata) == "$baseline" ]] || fail 'deployment state changed; stopped before the next round'
  inspect_safe "$app" > "$dest/app.json" || fail 'cannot inspect recorded active application'
  inspect_safe "$db" > "$dest/db.json" || fail 'cannot inspect recorded database'
  inspect_safe "$proxy" > "$dest/proxy.json" || fail 'cannot inspect recorded proxy'
  jq -e --argjson s "$baseline" '
    .running and .name == ("/"+$s.prefix+"-"+$s.active) and .release == $s.prefix and .image == $s.active_image and
    any(.mounts[]; .Type == "volume" and .Name == $s.application_volume and .Destination == "/app/data") and
    (.networks[$s.network].IPAddress|type == "string" and length > 0)
  ' "$dest/app.json" >/dev/null 2>&1 || fail 'active application label/image/volume/network mismatch; no probes sent'
  jq -e --argjson s "$baseline" '
    .running and .id == $s.database_id and .image == $s.database_image and
    any(.mounts[]; .Type == "volume" and .Name == $s.database_volume and .Destination == "/var/lib/postgresql/data")
  ' "$dest/db.json" >/dev/null 2>&1 || fail 'database identity/image/volume mismatch; no SQL sent'
  jq -e --argjson s "$baseline" --arg mount "$root/.bluegreen/proxy" '
    .running and .name == ("/"+$s.prefix+"-proxy") and .image == $s.proxy_image and
    any(.mounts[]; .Type == "bind" and .Destination == "/release" and .Source == $mount) and
    (.networks[$s.network].IPAddress|type == "string" and length > 0) and
    any(.ports["8080/tcp"][]?; .HostPort == ($s.port|tostring) and (.HostIp == "127.0.0.1" or .HostIp == "0.0.0.0"))
  ' "$dest/proxy.json" >/dev/null 2>&1 || fail 'proxy image/mount/network/loopback-port mismatch; no probes sent'
  app_ip=$(jq -r --arg n "$network" '.networks[$n].IPAddress' "$dest/app.json")
  [[ $app_ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail 'active container lacks usable IPv4'
  local observed
  observed=$(curl -q --silent --noproxy '*' --proto '=http' --connect-timeout 2 --max-time 3 "http://127.0.0.1:$port/_release" 2>/dev/null) || fail 'cannot read fixed proxy release marker'
  [[ $observed == "$active" ]] || fail 'fixed proxy route differs from recorded active color; no business probes sent'
}
verify_containers "$out"
# Restrict the stdin credential protocol to single-line values. DB/user names
# cannot be libpq connection strings. PGHOST is deliberately loopback INSIDE
# the already identity-verified database container, never an unverified host.
pg_available=1
jq -e '
  .database_env as $e |
  ($e.PGUSER|type == "string" and test("^[A-Za-z0-9_][A-Za-z0-9_.-]*$")) and
  ($e.PGDATABASE|type == "string" and test("^[A-Za-z0-9_][A-Za-z0-9_.-]*$")) and
  ($e.PGPASSWORD|type == "string" and (test("[\r\n\u0000]")|not)) and
  (($e.PGPORT // "5432"|tostring)|test("^[0-9]{1,5}$") and (tonumber >= 1 and tonumber <= 65535))
' "$state" >/dev/null 2>&1 || pg_available=0
cat > "$out/pg-observe.sql" <<'SQL'
BEGIN READ ONLY;
SET LOCAL statement_timeout = '3s';
SET LOCAL lock_timeout = '1s';
SELECT json_build_object('kind','visibility','time',clock_timestamp(),'all_stats',pg_has_role(current_user,'pg_read_all_stats','MEMBER') OR (SELECT rolsuper FROM pg_roles WHERE rolname=current_user));
SELECT json_build_object('kind','activity','time',clock_timestamp(),'state',state,'wait_type',wait_event_type,'wait',wait_event,'count',count(*),'oldest_query_seconds',round(max(extract(epoch from clock_timestamp()-query_start))::numeric,3),'oldest_transaction_seconds',round(max(extract(epoch from clock_timestamp()-xact_start))::numeric,3)) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() GROUP BY state,wait_event_type,wait_event;
SELECT json_build_object('kind','blocked','time',clock_timestamp(),'pid',pid,'blocking_pids',pg_blocking_pids(pid),'wait_type',wait_event_type,'wait',wait_event,'query_age_seconds',round(extract(epoch from clock_timestamp()-query_start)::numeric,3),'transaction_age_seconds',round(extract(epoch from clock_timestamp()-xact_start)::numeric,3)) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND cardinality(pg_blocking_pids(pid))>0;
SELECT json_build_object('kind','locks','time',clock_timestamp(),'lock_type',locktype,'mode',mode,'granted',granted,'count',count(*)) FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) GROUP BY locktype,mode,granted;
COMMIT;
SQL
sample_pg() {
  local dir=$1 rc=0
  if ((pg_available == 0)); then
    printf '{"status":"skipped","reason":"missing_or_unsupported_state_database_credentials"}\n' > "$dir/postgres-status.json"
    return
  fi
  {
    jq -r '.database_env|.PGUSER,.PGDATABASE,.PGPASSWORD,(.PGPORT // "5432"|tostring)' "$state" 2>/dev/null
    cat "$out/pg-observe.sql"
  } | timeout 15 docker exec -i "$db" sh -c '
    IFS= read -r PGUSER || exit 2
    IFS= read -r PGDATABASE || exit 2
    IFS= read -r PGPASSWORD || exit 2
    IFS= read -r PGPORT || exit 2
    export PGUSER PGDATABASE PGPASSWORD PGPORT
    PGHOST=127.0.0.1
    PGCONNECT_TIMEOUT=3
    PGAPPNAME=dreamtrans_readonly_latency_probe
    PGOPTIONS="-c default_transaction_read_only=on -c statement_timeout=3000 -c lock_timeout=1000"
    export PGHOST PGCONNECT_TIMEOUT PGAPPNAME PGOPTIONS
    exec psql -X -w -A -t -q -v ON_ERROR_STOP=1
  ' > "$dir/postgres.jsonl" 2>/dev/null || rc=$?
  jq -n --argjson rc "$rc" '{status:(if $rc==0 then "ok" else "failed" end),exit_code:$rc,credentials:"stdin_only",scope:"current_database",read_only:true,statement_timeout_ms:3000,lock_timeout_ms:1000}' > "$dir/postgres-status.json"
}
sample_http() {
  local dir=$1 route=$2 base=$3 path=$4 idx=$5 started rc=0
  started=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  # -q is first: no ~/.curlrc. No cookies, bearer headers, response bodies,
  # response headers, redirects, arbitrary request paths or ambient proxies.
  curl -q --silent --noproxy '*' --proto '=http,https' --connect-timeout 3 --max-time 15 \
    --output /dev/null --header "Host: $public_host" \
    --write-out '%{http_code}\t%{http_version}\t%{remote_ip}\t%{time_namelookup}\t%{time_connect}\t%{time_appconnect}\t%{time_pretransfer}\t%{time_starttransfer}\t%{time_total}\t%{size_download}\n' \
    "$base$path" > "$dir/$route-$idx.raw" 2>/dev/null || rc=$?
  printf '%s\t%s\t%s\t%s\t' "$started" "$route" "$path" "$rc" > "$dir/$route-$idx.tsv"
  cat "$dir/$route-$idx.raw" >> "$dir/$route-$idx.tsv"
}
sample_resources() {
  local dir=$1 rc=0 role id file
  {
    printf 'sample_time=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
    for file in /proc/loadavg /proc/meminfo /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io; do
      if [[ -r $file ]]; then printf '\n%s\n' "$file"; cat "$file"; fi
    done
    printf '\n/proc/stat aggregate cpu\n'
    read -r -a cpu_line < /proc/stat
    printf '%s ' "${cpu_line[@]}"; printf '\n'
  } > "$dir/host-resources.txt"
  timeout 8 docker stats --no-stream --format '{{json .}}' "$app" "$db" "$proxy" > "$dir/docker-stats.jsonl" 2>/dev/null || rc=$?
  printf '{"exit_code":%s}\n' "$rc" > "$dir/docker-stats-status.json"
  for role in app db proxy; do
    case "$role" in app) id=$app;; db) id=$db;; proxy) id=$proxy;; esac
    rc=0
    # shellcheck disable=SC2016 # Expand paths inside the container, not on the host.
    timeout 5 docker exec "$id" sh -c '
      for p in /sys/fs/cgroup/cpu.stat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/cpu/cpu.stat /sys/fs/cgroup/cpu,cpuacct/cpu.stat /sys/fs/cgroup/memory.events /proc/pressure/cpu /proc/pressure/memory /proc/pressure/io; do
        if [ -r "$p" ]; then printf "\n%s\n" "$p"; cat "$p"; fi
      done
    ' > "$dir/$role-cgroup.txt" 2>/dev/null || rc=$?
    printf '{"exit_code":%s}\n' "$rc" > "$dir/$role-cgroup-status.json"
  done
}
printf 'sample_start_utc\troute\tpath\tcurl_exit\thttp_status\thttp_version\tremote_ip\tdns_seconds\tconnect_seconds\ttls_seconds\tpretransfer_seconds\tfirst_byte_seconds\ttotal_seconds\tbytes\n' > "$out/http.tsv"
paths=(/healthz /readyz /api/system/settings /api/system/access)
for ((round=1; round<=rounds; round++)); do
  dir=$out/round-$round
  mkdir -m 700 "$dir"
  verify_containers "$dir"
  note "Collecting round $round/$rounds (maximum 15 seconds per HTTP request)."
  pids=()
  sample_pg "$dir" & pids+=("$!")
  sample_resources "$dir" & pids+=("$!")
  for idx in "${!paths[@]}"; do
    sample_http "$dir" direct "http://$app_ip:8080" "${paths[$idx]}" "$idx" & pids+=("$!")
    sample_http "$dir" proxy "http://127.0.0.1:$port" "${paths[$idx]}" "$idx" & pids+=("$!")
    if ((no_public == 0)); then
      sample_http "$dir" public "$public_url" "${paths[$idx]}" "$idx" & pids+=("$!")
    fi
  done
  for pid in "${pids[@]}"; do wait "$pid" || note 'A collector subprocess failed; inspect sample status files.'; done
  for file in "$dir"/*.tsv; do [[ -f $file ]] && cat "$file" >> "$out/http.tsv"; done
  # Retain bounded samples if an operator deploys during collection; never
  # silently compare one color against another under a single label.
  [[ $(state_metadata) == "$baseline" ]] || fail 'deployment changed during sampling; retained earlier results, stopped'
  ((round == rounds)) || sleep "$interval"
done
jq -n --arg time "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg url "$public_url" --argjson rounds "$rounds" '{completed_at:$time,rounds:$rounds,public_url:$url,collection_finished:true,application_changed:false,warning:"Collection completion is not a passing health/performance verdict; inspect HTTP exit/status and PostgreSQL status."}' > "$out/collection.json"
note "Read-only collection finished: $out"
printf '%s\n' "$out"
