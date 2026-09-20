#!/usr/bin/env python3
"""Exercise the Go controller against isolated real Docker/PG/application images.
Python is only the test harness: all lifecycle operations run dreamtransctl.
"""
import hashlib
import json
import os
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

            # A different immutable image with the same compatible application.
            (root / 'Dockerfile').write_text(f'FROM {source}\nLABEL dreamtrans.ops.fixture="{fixture}"\n')
            candidate = fixture + ':candidate'
            images.append(candidate)
            run('docker', 'build', '-t', candidate, directory)
            candidate_id = inspect(candidate)['Id']
            cli('deploy', '--image', candidate_id, '--pause')
            assert state()['phase'] == 'candidate' and state()['active'] == 'blue'
            # Simulate interruption between container creation and entry attachment.
            run('docker', 'network', 'disconnect', entry, prefix + '-green')
            cli('resume', '--observe', '0', '--drain-timeout', '0')
            assert state()['phase'] == 'ready' and state()['active'] == 'green'
            assert entry in inspect(prefix + '-green')['NetworkSettings']['Networks']
            sql("INSERT INTO ops_marker VALUES('after-upgrade')")
            cli('rollback')
            cli('drain', '--drain-timeout', '0')
            assert state()['active'] == 'blue' and state()['phase'] == 'ready'
            assert sql('SELECT count(*) FROM ops_marker') == '2'
            assert run('docker', 'exec', prefix + '-blue', 'cat', '/app/data/marker') == 'retained'
            print('Go candidate pause/resume, switch/drain and rollback preserve new writes', flush=True)

            # Unknown image fails before taking traffic or touching data.
            failed = subprocess.run([binary, '--dir', directory, 'deploy', '--image', 'sha256:' + 'f'*64], capture_output=True)
            assert failed.returncode != 0 and state()['active'] == 'blue'
            assert sql('SELECT count(*) FROM ops_marker') == '2'

            # A container can start yet never become ready; it must not get traffic.
            (root / 'Dockerfile').write_text(f'FROM {source}\nENTRYPOINT ["/bin/sh", "-c", "exit 1"]\n')
            broken = fixture + ':unhealthy'
            images.append(broken)
            run('docker', 'build', '-t', broken, directory)
            failed = subprocess.run([binary, '--dir', directory, 'deploy', '--image', inspect(broken)['Id']],
                                    capture_output=True, timeout=180)
            assert failed.returncode != 0 and state()['active'] == 'blue' and state()['phase'] == 'candidate', failed.stderr.decode()
            cli('abort')
            assert state()['phase'] == 'ready'

            # Forward-only SQL: a committed expansion remains after the next SQL fails.
            contract = json.loads((root / 'bundle/release.json').read_text())
            contract['expand_migrations'] += ['057_ops_fixture.sql', '058_ops_failure.sql']
            (root / 'release.json').write_text(json.dumps(contract))
            (root / '057_ops_fixture.sql').write_text('CREATE TABLE ops_expansion(id integer);\n')
            (root / '058_ops_failure.sql').write_text('SELECT definitely_missing_ops_fixture();\n')
            runner = (root / 'bundle/migrate.sh').read_text()
            assert 'expected_latest_prefix=056' in runner
            (root / 'migrate.sh').write_text(runner.replace('expected_latest_prefix=056', 'expected_latest_prefix=058'))
            (root / 'Dockerfile').write_text(
                f'FROM {source}\nCOPY release.json /usr/share/dreamtrans/release.json\n'
                'COPY migrate.sh /usr/share/dreamtrans/migrate.sh\n'
                'COPY 057_ops_fixture.sql 058_ops_failure.sql /usr/share/dreamtrans/migrations/\n')
            broken = fixture + ':migration-failure'
            images.append(broken)
            run('docker', 'build', '-t', broken, directory)
            failed = subprocess.run([binary, '--dir', directory, 'deploy', '--image', inspect(broken)['Id']],
                                    capture_output=True, timeout=180)
            assert failed.returncode != 0 and state()['active'] == 'blue' and state()['phase'] == 'ready'
            assert sql("SELECT count(*) FROM schema_migrations WHERE version='057_ops_fixture.sql'") == '1'
            assert sql("SELECT count(*) FROM schema_migrations WHERE version='058_ops_failure.sql'") == '0'
            assert sql('SELECT count(*) FROM ops_marker') == '2'
            cli('snapshot', '--output', str(root / 'complete.tar'))
            assert (root / 'complete.tar').stat().st_mode & 0o777 == 0o600
            before_start = inspect(prefix + '-proxy')['State']['StartedAt']
            cli('sync-entry')
            assert inspect(prefix + '-proxy')['State']['StartedAt'] == before_start
            run('docker', 'restart', prefix + '-proxy')
            assert json.loads(cli('status'))['active'] == 'blue'
            print('Go failed release, complete snapshot, idempotent entry and restart checks passed', flush=True)
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
