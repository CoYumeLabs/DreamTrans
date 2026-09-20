#!/usr/bin/env python3
"""Read-only main-site resource and PostgreSQL diagnostics; never print query text or credentials."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
import urllib.error
import urllib.request


DATABASE_REPORT = """
BEGIN READ ONLY;
SET LOCAL statement_timeout = '5s';
SET LOCAL lock_timeout = '1s';
SELECT name,setting,unit FROM pg_settings WHERE name IN
 ('shared_buffers','work_mem','effective_cache_size','max_connections',
  'track_io_timing','max_parallel_workers_per_gather','jit') ORDER BY name;
SELECT datname,numbackends,blks_read,blks_hit,temp_files,
 pg_size_pretty(temp_bytes) AS temp_written,deadlocks,stats_reset
 FROM pg_stat_database WHERE datname=current_database();
SELECT state,wait_event_type,wait_event,count(*) AS connections,
 max(clock_timestamp()-query_start) FILTER (WHERE state='active') AS active_age
 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
 GROUP BY state,wait_event_type,wait_event ORDER BY connections DESC;
SELECT schemaname,relname,n_live_tup,n_dead_tup,n_mod_since_analyze,
 seq_scan,idx_scan,last_analyze,last_autoanalyze,
 pg_size_pretty(pg_total_relation_size(relid)) AS total_size
 FROM pg_stat_user_tables ORDER BY pg_total_relation_size(relid) DESC LIMIT 20;
SELECT tablename,indexname,indexdef FROM pg_indexes
 WHERE schemaname='public' AND tablename IN
 ('transcripts','sessions','usage_logs','balance_transactions')
 ORDER BY tablename,indexname;
SELECT EXISTS(SELECT 1 FROM pg_extension
 WHERE extname='pg_stat_statements') AS statement_statistics_installed;
COMMIT;
"""


def command(args, *, input_text=None, timeout=45):
    result = subprocess.run(args, input=input_text, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        # Engine errors and database errors may include configuration values.
        raise RuntimeError(f'{args[0]} diagnostic failed (exit {result.returncode}); no configuration changed')
    return result.stdout.strip()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def report(root):
    state = json.loads((root/'.bluegreen/state.json').read_text())
    if state.get('role') == 'edge' or not state.get('active'):
        raise RuntimeError('requires a completed main-site conversion')
    database = state['database_id']
    identity = json.loads(command(['docker', 'inspect', database]))[0]
    mounts = [m for m in identity['Mounts'] if m['Destination'] == '/var/lib/postgresql/data']
    if len(mounts) != 1 or mounts[0].get('Name') != state['database_volume']:
        raise RuntimeError('recorded database volume does not match; diagnostics stopped')
    print('[1/4] Host resources (current sample; not EC2 credit history)', flush=True)
    print('UTC:', time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()))
    print('Load averages:', ', '.join(f'{value:.2f}' for value in os.getloadavg()))
    for line in Path('/proc/meminfo').read_text().splitlines():
        if line.split(':', 1)[0] in ('MemTotal', 'MemAvailable', 'SwapTotal', 'SwapFree'):
            print(line)
    print('Install filesystem free GiB:', round(shutil.disk_usage(root).free/1024**3, 2))
    if shutil.which('vmstat'):
        print(command(['vmstat', '1', '3']))
    print('[2/4] Active application, proxy and database resources', flush=True)
    print(command(['docker', 'stats', '--no-stream', '--format',
                   '{{.Name}} CPU={{.CPUPerc}} MEMORY={{.MemUsage}} BLOCK_IO={{.BlockIO}} PIDS={{.PIDs}}',
                   database, state['prefix']+'-'+state['active'], state['prefix']+'-proxy']))
    print('[3/4] Local entry latency (does not measure browser/Cloudflare)', flush=True)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    bind = state.get('bind', '127.0.0.1')
    host = '127.0.0.1' if bind in ('', '0.0.0.0') else bind
    if ':' in host:
        host = '['+host+']'
    port = int(state['port'])
    for path in ('/healthz', '/readyz'):
        for attempt in range(2):
            start = time.monotonic()
            try:
                with opener.open(f'http://{host}:{port}{path}', timeout=5) as response:
                    response.read(4096)
                    status = str(response.status)
            except (urllib.error.URLError, OSError, TimeoutError):
                status = 'failed/timeout'
            print(f'{path} sample={attempt+1} status={status} ms={(time.monotonic()-start)*1000:.1f}')
    print('[4/4] Read-only PostgreSQL statistics and existing indexes', flush=True)
    settings = state['database_env']
    print(command(['docker', 'exec', '-i', database, 'psql', '-X', '-P', 'pager=off',
                   '-v', 'ON_ERROR_STOP=1', '-U', settings['PGUSER'], '-d', settings['PGDATABASE']],
                  input_text=DATABASE_REPORT))
    print('No index changes, ANALYZE, restarts, query text, or account content included.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dir', default='/root/dreamtrans')
    args = parser.parse_args()
    try:
        report(Path(args.dir).resolve())
    except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
