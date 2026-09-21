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

            edge_ref = 'ghcr.io/coyumelabs/dreamtrans@sha256:b271112c2ef1010ce088e202465077e9f5464fc44ca340597c71284021dc1449'
            cli('configure-edge', '--image', edge_ref, '--proxy-image', proxy,
                '--main', 'https://main.example.test', '--observe', '0', '--drain-timeout', '0')
            assert state()['active'] == 'green' and state()['phase'] == 'ready'
            seed = state()['application_env']['EDGE_SIGNING_SEED']
            assert len({c['image'] for c in state()['colors'].values()}) == 1
            recovery = str(Path(__file__).resolve().parents[1] / 'recover-e163-release.sh')
            sql("INSERT INTO edge_nodes(name,region,endpoint,max_connections,mode) VALUES('fixture','test','https://edge.example.test',1,'enabled')")
            before = (root / '.bluegreen/state.json').read_bytes()
            refused = subprocess.run(['bash', recovery, directory], capture_output=True, text=True, timeout=60)
            assert refused.returncode != 0, refused.stdout
            assert (root / '.bluegreen/state.json').read_bytes() == before
            assert inspect(prefix + '-green')['State']['Running']
            print('Recovery refused enabled Edge without changing live state', flush=True)
            sql("UPDATE edge_nodes SET mode='disabled'")
            sql("INSERT INTO ops_marker VALUES('after-double-release')")
            old_ref = 'ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90'
            tools = root / 'fail-once-bin'
            tools.mkdir()
            wrapper = tools / 'docker'
            wrapper.write_text('#!/bin/sh\nif [ "$1" = run ]; then\nfor arg in "$@"; do\n'
                               'if [ "$arg" = "$RECOVERY_OLD_ID" ] && [ ! -e "$RECOVERY_FAILURE_MARKER" ]; then\n'
                               'touch "$RECOVERY_FAILURE_MARKER"; exit 77\nfi\ndone\nfi\n'
                               'exec "$RECOVERY_REAL_DOCKER" "$@"\n')
            wrapper.chmod(0o700)
            failure_env = dict(os.environ, PATH=str(tools) + ':' + os.environ['PATH'],
                               RECOVERY_OLD_ID=inspect(old_ref)['Id'],
                               RECOVERY_FAILURE_MARKER=str(root / 'failure-injected'),
                               RECOVERY_REAL_DOCKER=shutil.which('docker'))
            interrupted = subprocess.run(['bash', recovery, directory], env=failure_env,
                                         capture_output=True, text=True, timeout=60)
            assert interrupted.returncode != 0 and (root / 'failure-injected').exists(), interrupted.stderr
            assert state()['phase'] == 'candidate' and state()['active'] == 'green'
            assert inspect(prefix + '-green')['State']['Running']
            print('Candidate startup failure retained the active service; retrying persisted recovery', flush=True)
            print(run('bash', recovery, directory), flush=True)
            restored = state()
            assert restored['active'] == 'blue' and restored['phase'] == 'ready'
            old_ref = 'ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90'
            assert restored['colors']['blue']['image'] == inspect(old_ref)['Id']
            assert restored['colors']['green']['image'] == image
            assert restored['colors']['blue']['contract'].get('provider_credentials', 0) == 0
            assert restored['colors']['green']['contract']['provider_credentials'] == 1
            assert restored['application_env']['EDGE_ROUTING_ENABLED'] == 'false'
            assert restored['application_env']['EDGE_SIGNING_SEED'] == seed
            assert restored['database_volume'] == volumes[0] and restored['application_volume'] == volumes[1]
            assert sql('SELECT count(*) FROM ops_marker') == '2'
            assert sql('SELECT count(*) FROM schema_migrations') == '57'
            assert (root / '.env').read_text() == env_sentinel
            assert run('docker','exec',prefix+'-blue','cat','/app/data/marker') == 'retained'
            started = inspect(prefix + '-blue')['State']['StartedAt']
            run('bash', recovery, directory)
            assert inspect(prefix + '-blue')['State']['StartedAt'] == started
            cli('rollback')
            cli('drain', '--drain-timeout', '0')
            assert state()['active'] == 'green' and sql('SELECT count(*) FROM ops_marker') == '2'
            print('Distinct old release restored, additive migrations/writes/volumes/secrets retained; repeat and forward rollback passed', flush=True)
    finally:
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
