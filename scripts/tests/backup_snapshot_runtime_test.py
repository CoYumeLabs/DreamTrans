#!/usr/bin/env python3
"""Exercise full snapshots against disposable PostgreSQL containers and volumes."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent/'legacy'))
import release


def ready(container):
    for _ in range(60):
        result = subprocess.run(['docker', 'exec', container, 'pg_isready', '-h', '127.0.0.1',
                                 '-U', 'fixture', '-d', 'fixture'], capture_output=True)
        if result.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError('isolated backup fixture database did not start')


def main():
    application_image = sys.argv[1]
    postgres_image = 'pgvector/pgvector:0.8.2-pg16-bookworm'
    prefix = 'dt-snapshot-test-' + uuid.uuid4().hex[:12]
    network, database, restored = (prefix+'-'+part for part in ('net', 'db', 'restored'))
    volumes = [prefix+'-'+part for part in ('pgdata', 'appdata', 'restored-appdata')]
    docker = release.docker
    try:
        docker('network', 'create', '--internal', network)
        for volume in volumes:
            docker('volume', 'create', volume)
        common = ['--memory', '384m', '--cpus', '1', '-e', 'POSTGRES_USER=fixture',
                  '-e', 'POSTGRES_DB=fixture', '-e', 'POSTGRES_HOST_AUTH_METHOD=trust']
        docker('run', '-d', '--name', database, '--network', network, '--network-alias', 'db',
               '--mount', f'type=volume,src={volumes[0]},dst=/var/lib/postgresql/data',
               *common, postgres_image)
        ready(database)
        docker('exec', database, 'psql', '-X', '-v', 'ON_ERROR_STOP=1', '-U', 'fixture', '-d', 'fixture',
               '-c', "CREATE TABLE ledger(id int PRIMARY KEY, balance numeric); INSERT INTO ledger VALUES(1,123.45); CREATE SCHEMA yuaction; CREATE TABLE yuaction.items(id int PRIMARY KEY); INSERT INTO yuaction.items VALUES(7);")
        docker('run', '--rm', '--network', 'none', '--mount', f'type=volume,src={volumes[1]},dst=/application',
               '--entrypoint', '/bin/sh', postgres_image, '-ec',
               'mkdir -p /application/knowledge; printf knowledge-fixture > /application/knowledge/file; printf legacy-sqlite-fixture > /application/rag.db; chmod 640 /application/rag.db')
        with tempfile.TemporaryDirectory(prefix='dt-snapshot-runtime-') as directory:
            root = Path(directory)
            controller = release.Controller(root)
            current = release.inspect(database)
            controller.state = {
                'format': 1, 'database_id': current['Id'], 'database_volume': volumes[0],
                'database_image': current['Image'], 'database_network': network,
                'application_volume': volumes[1], 'active': 'blue',
                'colors': {'blue': {'image': application_image}},
                'database_env': {'PGHOST': 'db', 'PGPORT': '5432', 'PGDATABASE': 'fixture',
                                 'PGUSER': 'fixture', 'PGPASSWORD': 'isolated-fixture'},
            }
            controller.persist()
            configs = {'.env': 'fixture-main-secret', 'compose.production.yml': 'external-production-volume',
                       'compose.restore.yml': 'fixed-restored-image',
                       'yuaction/.env': 'fixture-companion-secret',
                       'yuaction/install.sh': 'fixture-companion-installer',
                       'yuaction/.project': 'fixture-companion-project',
                       'yuaction/.dreamtrans-dir': 'fixture-parent-directory',
                       'yuaction/compose.ghcr.yml': 'companion-compose',
                       'yuaction/compose.bluegreen.yml': 'stable-entry-network'}
            for name, value in configs.items():
                path = root/name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(value)
            output = root/'backup.tar'
            subprocess.run([os.environ['DREAMTRANS_CTL'], '--dir', str(root), 'snapshot', '--output', str(output)], check=True)
            assert output.stat().st_mode & 0o777 == 0o600
            with tarfile.open(output) as archive:
                manifest = json.load(archive.extractfile('manifest.json'))
                assert manifest['application_volume'] == volumes[1]
                assert manifest['database_volume'] == volumes[0]
                for name, expected in manifest['files'].items():
                    data = archive.extractfile(name).read()
                    assert hashlib.sha256(data).hexdigest() == expected
                    (root/name).write_bytes(data)
            with tarfile.open(root/'configuration.tar') as archive:
                for name, expected in configs.items():
                    assert archive.extractfile(name).read().decode() == expected
                assert '.bluegreen/state.json' in archive.getnames()
            docker('run', '--rm', '--network', 'none',
                   '--mount', f'type=volume,src={volumes[2]},dst=/application',
                   '--mount', f'type=bind,src={root},dst=/snapshot,readonly',
                   '--entrypoint', '/bin/sh', postgres_image, '-ec',
                   'tar -xf /snapshot/application.tar -C /application; test "$(cat /application/knowledge/file)" = knowledge-fixture; test "$(cat /application/rag.db)" = legacy-sqlite-fixture; test "$(stat -c %a /application/rag.db)" = 640')
            docker('run', '-d', '--name', restored, '--network', 'none',
                   '--tmpfs', '/var/lib/postgresql/data', *common, postgres_image)
            ready(restored)
            with (root/'database.dump').open('rb') as stream:
                subprocess.run(['docker', 'exec', '-i', restored, 'pg_restore', '--exit-on-error',
                                '--no-owner', '--no-privileges', '-U', 'fixture', '-d', 'fixture'],
                               stdin=stream, check=True, capture_output=True)
            result = docker('exec', restored, 'psql', '-XAt', '-U', 'fixture', '-d', 'fixture', '-c',
                            'SELECT balance FROM ledger WHERE id=1; SELECT id FROM yuaction.items;')
            assert result.splitlines() == ['123.45', '7']
            # Restoring into new resources must not alter the original ledger.
            assert docker('exec', database, 'psql', '-XAt', '-U', 'fixture', '-d', 'fixture',
                          '-c', 'SELECT balance FROM ledger WHERE id=1;') == '123.45'
            print('Full snapshot restored: PostgreSQL, YuAction schema, app files/permissions, deployment configuration and manifest hashes.')
    finally:
        for container in (restored, database):
            subprocess.run(['docker', 'rm', '-fv', container], capture_output=True)
        for volume in volumes:
            subprocess.run(['docker', 'volume', 'rm', volume], capture_output=True)
        subprocess.run(['docker', 'network', 'rm', network], capture_output=True)


if __name__ == '__main__':
    main()
