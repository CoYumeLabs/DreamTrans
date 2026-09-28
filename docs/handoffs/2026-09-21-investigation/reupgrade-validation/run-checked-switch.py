#!/usr/bin/env python3
"""Verify the incident recovery using both exact published releases and an isolated DB."""
import hashlib
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid


def run(*args, input_text=None):
    result = subprocess.run(args, input=input_text, text=True,
                            capture_output=True, timeout=180)
    if result.returncode:
        raise RuntimeError(f'{args[0]} failed: {result.stderr[-2000:]}')
    return result.stdout.strip()


def inspect(name):
    return json.loads(run('docker', 'inspect', name))[0]


def main():
    assert os.geteuid() == 0, 'Run this disposable host lifecycle test as root'
    binary = os.environ['DREAMTRANS_CTL']
    source = sys.argv[1]
    pg_image = 'pgvector/pgvector:0.8.2-pg16-bookworm'
    proxy = 'nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10'
    fixture = 'dt-go-ops-' + uuid.uuid4().hex[:10]
    network = fixture + '-database'
    database, legacy = fixture + '-db', fixture + '-legacy'
    volumes = [fixture + '-pgdata', fixture + '-appdata']
    containers = [database, legacy]
    images = []
    entry = None
    try:
        image = inspect(source)['Id']
        try:
            proxy_image = inspect(proxy)['Id']
        except RuntimeError:
            run('docker', 'pull', proxy)
            proxy_image = inspect(proxy)['Id']
        with tempfile.TemporaryDirectory(prefix='dt-go-runtime-') as directory:
            root = Path(directory)
            prefix = 'dreamtrans-' + hashlib.sha256(str(root).encode()).hexdigest()[:10]
            entry = prefix + '-entry'
            containers += [prefix + '-' + name for name in ('blue', 'green', 'proxy')]
            env_sentinel = 'COMPOSE_FILE=docker-compose.yml:compose.restore.yml:compose.production.yml\nOPERATOR_SECRET=fixture-preserved\n'
            (root / '.env').write_text(env_sentinel)
            (root / 'compose.production.yml').write_text('production-volume-sentinel\n')
            (root / 'yuaction').mkdir()
            (root / 'yuaction/.env').write_text('YUFOLO_URL=http://dreamtrans:8080\n')
            for volume in volumes:
                run('docker', 'volume', 'create', volume)
            run('docker', 'network', 'create', network)
            run('docker', 'run', '-d', '--name', database,
                '--network', network, '--network-alias', 'db',
                '--mount', f'type=volume,src={volumes[0]},dst=/var/lib/postgresql/data',
                '-e', 'POSTGRES_USER=fixture', '-e', 'POSTGRES_DB=fixture',
                '-e', 'POSTGRES_PASSWORD=fixture', pg_image)
            for _ in range(60):
                if subprocess.run(['docker', 'exec', database, 'pg_isready',
                                   '-h', '127.0.0.1', '-U', 'fixture'],
                                  capture_output=True).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise AssertionError('isolated database did not start')
            extract = run('docker', 'create', image)
            try:
                run('docker', 'cp', extract + ':/usr/share/dreamtrans/.', str(root / 'bundle'))
            finally:
                run('docker', 'rm', '-v', extract)
            db_env = ['-e', 'PGHOST=db', '-e', 'PGUSER=fixture',
                      '-e', 'PGDATABASE=fixture', '-e', 'PGPASSWORD=fixture']
            run('docker', 'run', '--rm', '--network', network, *db_env,
                '-e', 'MIGRATIONS_DIR=/release/migrations',
                '--mount', f'type=bind,src={root / "bundle"},dst=/release,readonly',
                '--entrypoint', '/bin/sh', pg_image, '/release/migrate.sh')
            run('docker', 'run', '--rm', '--user', '0:0', '--entrypoint', '/bin/sh',
                '--mount', f'type=volume,src={volumes[1]},dst=/data', image,
                '-ec', 'echo retained > /data/marker; chown -R 10001:10001 /data')
            app_env = ['-e', 'DATABASE_URL=postgres://fixture:fixture@db:5432/fixture?sslmode=disable',
                       '-e', 'JWT_SECRET=0123456789abcdef0123456789abcdef',
                       '-e', 'JWT_REFRESH_SECRET=fedcba9876543210fedcba9876543210',
                       '-e', 'ALLOW_ANONYMOUS_API=false',
                       '-e', 'SM_API_KEY=fixture', '-e', 'PORT=8080']
            run('docker', 'run', '-d', '--name', legacy, '--network', network,
                '--network-alias', 'dreamtrans', '-p', '127.0.0.1::8080',
                '--mount', f'type=volume,src={volumes[1]},dst=/app/data', *app_env, image)
            port = inspect(legacy)['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']
            for _ in range(60):
                if subprocess.run(['docker', 'exec', legacy, 'wget', '-qO-',
                                   'http://127.0.0.1:8080/readyz'], capture_output=True).returncode == 0:
                    break
                time.sleep(1)
            else:
                raise AssertionError('legacy fixture app not ready')

            def cli(*args):
                return run(binary, '--dir', directory, *args)

            def state():
                return json.loads((root / '.bluegreen/state.json').read_text())

            def sql(query):
                return run('docker', 'exec', database, 'psql', '-XAt', '-U', 'fixture', '-d', 'fixture', '-c', query)

            cli('init', '--app', legacy, '--database', database,
                '--database-network', network, '--port', port,
                '--image', image, '--proxy-image', proxy_image, '--maintenance')
            first = state()
            assert first['phase'] == 'ready' and first['active'] == 'blue'
            assert first['database_volume'] == volumes[0] and first['application_volume'] == volumes[1]
            assert (root / '.env').read_text() == env_sentinel
            assert not inspect(legacy)['State']['Running']
            assert 'dreamtrans' in inspect(prefix + '-proxy')['NetworkSettings']['Networks'][network]['Aliases']
            preserved_state = (root / '.bluegreen/state.json').read_bytes()
            (root / 'backup.sh').write_text('previous helper\n')
            cli('install-tools', '--backup-file', str(root / 'bundle/backup.sh'))
            cli('install-tools', '--backup-file', str(root / 'bundle/backup.sh'))
            assert (root / '.bluegreen/state.json').read_bytes() == preserved_state
            assert (root / '.env').read_text() == env_sentinel
            assert (root / 'dreamtransctl').stat().st_mode & 0o777 == 0o700
            sql("CREATE TABLE ops_marker(value text); INSERT INTO ops_marker VALUES('before-upgrade');")
            print('Go initial conversion preserved volumes, configuration and companion alias', flush=True)

            old_ref = 'ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90'
            new_ref = 'ghcr.io/coyumelabs/dreamtrans@sha256:a6fcde016bec5d69d78f4bdf0fcdbb9bf85df3c6755f4c1c6b529bbb3661b64d'
            recovery = '/tmp/dt-production-recovery/scripts/recover-e163-release.sh'
            old_id = inspect(old_ref)['Id']
            run('bash', recovery, directory)
            assert state()['active'] == 'green' and state()['phase'] == 'ready'
            assert state()['colors']['green']['image'] == old_id
            assert not inspect(prefix+'-blue')['State']['Running']
            green_id = inspect(prefix+'-green')['Id']
            green_started = inspect(prefix+'-green')['State']['StartedAt']
            schema = sql('SELECT version,checksum FROM schema_migrations ORDER BY version')
            cli_hash = hashlib.sha256((root/'dreamtransctl').read_bytes()).hexdigest()
            config_before = state()['application_env']
            print('Matched production: green exact old active; blue exact new stopped; 57 migrations', flush=True)
            import threading
            import urllib.request
            stop_probe = threading.Event()
            samples = []
            stage = 'baseline'
            def probe():
                while not stop_probe.is_set():
                    sample = {'stage':stage}
                    began = time.perf_counter()
                    try:
                        with urllib.request.urlopen('http://127.0.0.1:'+port+'/api/system/settings',timeout=5) as response:
                            sample['status'] = response.status
                            response.read()
                    except Exception as exc:
                        sample['error'] = str(exc)
                    sample['ms'] = round((time.perf_counter()-began)*1000,3)
                    samples.append(sample)
                    stop_probe.wait(.03)
            probe_thread = threading.Thread(target=probe,daemon=True)
            probe_thread.start()
            time.sleep(1)
            stage = 'pause_candidate'
            cli('deploy','--image',new_ref,'--pause')
            paused = json.loads(cli('status'))
            assert paused['active']=='green' and paused['phase']=='candidate'
            assert paused['colors']['blue']['mode']=='standby'
            assert paused['colors']['green']['mode']=='active'
            assert run('docker','exec',prefix+'-proxy','wget','-qO-','http://127.0.0.1:8080/_release')=='green'
            assert inspect(prefix+'-green')['Id']==green_id and inspect(prefix+'-green')['State']['StartedAt']==green_started
            assert state()['application_env']==config_before
            assert sql('SELECT version,checksum FROM schema_migrations ORDER BY version')==schema
            assert hashlib.sha256((root/'dreamtransctl').read_bytes()).hexdigest()==cli_hash
            sql("INSERT INTO ops_marker VALUES('during-paused-candidate')")
            time.sleep(2)
            stage = 'abort_candidate'
            cli('abort')
            assert state()['active']=='green' and state()['phase']=='ready'
            assert not inspect(prefix+'-blue')['State']['Running']
            assert inspect(prefix+'-green')['Id']==green_id and inspect(prefix+'-green')['State']['StartedAt']==green_started
            assert state()['application_env']==config_before
            assert sql('SELECT count(*) FROM ops_marker')=='2'
            print('Paused candidate retained green route, container, CLI, env, schema; abort stopped only blue and retained new writes',flush=True)
            stage = 'pause_again'
            cli('deploy','--image',new_ref,'--pause')
            stage = 'resume_new'
            switch_script = '/tmp/dt-reupgrade-preflight-20260921/switch-checked.sh'
            saved_candidate = (root/'.bluegreen/state.json').read_bytes()
            changed_candidate = json.loads(saved_candidate)
            changed_candidate['colors']['blue']['application_env']['EDGE_ROUTING_ENABLED']='true'
            (root/'.bluegreen/state.json').write_text(json.dumps(changed_candidate))
            rejected_candidate = (root/'.bluegreen/state.json').read_bytes()
            refused_env = subprocess.run(['bash',switch_script,directory],capture_output=True,text=True,timeout=30)
            assert refused_env.returncode!=0
            assert (root/'.bluegreen/state.json').read_bytes()==rejected_candidate
            (root/'.bluegreen/state.json').write_bytes(saved_candidate)
            print('Preflight rejected enabled Edge in candidate-specific environment',flush=True)
            fail_tools=root/'curl-fail-bin'
            fail_tools.mkdir()
            (fail_tools/'curl').write_text('#!/bin/sh\nprintf green\nexit 28\n')
            (fail_tools/'curl').chmod(0o700)
            refused_route=subprocess.run(['bash',switch_script,directory],env=dict(os.environ,PATH=str(fail_tools)+':'+os.environ['PATH']),capture_output=True,text=True,timeout=30)
            assert refused_route.returncode!=0
            assert (root/'.bluegreen/state.json').read_bytes()==saved_candidate
            print('Preflight rejected failed route transfer even with green response text',flush=True)
            sql("INSERT INTO edge_nodes(name,region,endpoint,max_connections,mode) VALUES('fixture','test','https://edge.example.test',1,'enabled')")
            before_guard = (root/'.bluegreen/state.json').read_bytes()
            blocked = subprocess.run(['bash',switch_script,directory],capture_output=True,text=True,timeout=30)
            assert blocked.returncode!=0 and 'STOP: Edge' in blocked.stdout,blocked.stdout+blocked.stderr
            assert (root/'.bluegreen/state.json').read_bytes()==before_guard
            assert run('docker','exec',prefix+'-proxy','wget','-qO-','http://127.0.0.1:8080/_release')=='green'
            sql("UPDATE edge_nodes SET mode='disabled'")
            print('New read-only guard refused active Edge without switching or modifying release state',flush=True)
            print(run('bash',switch_script,directory),flush=True)
            assert state()['active']=='blue' and state()['phase']=='ready'
            assert state()['colors']['blue']['image']==image
            assert not inspect(prefix+'-green')['State']['Running']
            before_rollback = (root/'.bluegreen/state.json').read_bytes()
            refused = subprocess.run([binary,'--dir',directory,'rollback'],capture_output=True,text=True,timeout=30)
            assert refused.returncode!=0
            assert 'credential' in refused.stderr.lower(),refused.stderr
            assert (root/'.bluegreen/state.json').read_bytes()==before_rollback
            print('Normal rollback rejected capability downgrade without changing state',flush=True)
            stage = 'special_recovery'
            run('bash',recovery,directory)
            assert state()['active']=='green' and state()['phase']=='ready'
            assert state()['colors']['green']['image']==old_id
            assert state()['database_volume']==volumes[0] and state()['application_volume']==volumes[1]
            assert sql('SELECT count(*) FROM ops_marker')=='2'
            assert sql('SELECT version,checksum FROM schema_migrations ORDER BY version')==schema
            assert (root/'.env').read_text()==env_sentinel
            time.sleep(1)
            stop_probe.set()
            probe_thread.join(10)
            assert not probe_thread.is_alive()
            failures = [s for s in samples if s.get('status')!=200]
            result = {'valid':not failures,'old_id':old_id,'new_id':image,'samples':len(samples),'failures':failures,'max_ms':max(s['ms'] for s in samples),'stages':{name:{'samples':len([s for s in samples if s['stage']==name]),'max_ms':max(s['ms'] for s in samples if s['stage']==name)} for name in sorted(set(s['stage'] for s in samples))},'ordinary_rollback_error':refused.stderr.strip(),'limitations':['No WebSockets or tasks; recovery requires ready phase and disabled Edge.','Isolated synthetic DB; not a reproduction of original latency.']}
            Path('/tmp/dt-reupgrade-preflight-20260921/checked-switch-results.json').write_text(json.dumps(result,indent=2)+'\n')
            assert not failures,failures
            print(json.dumps(result,indent=2),flush=True)
    finally:
        if "stop_probe" in locals():
            stop_probe.set()
            probe_thread.join(10)
        for container in reversed(containers):
            subprocess.run(['docker', 'rm', '-fv', container], capture_output=True)
        for volume in volumes:
            subprocess.run(['docker', 'volume', 'rm', volume], capture_output=True)
        for name in (entry, network):
            if name:
                subprocess.run(['docker', 'network', 'rm', name], capture_output=True)
        for image in images:
            subprocess.run(['docker', 'image', 'rm', image], capture_output=True)


if __name__ == '__main__':
    main()
