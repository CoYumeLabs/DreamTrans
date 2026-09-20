#!/usr/bin/env python3
"""Verify companion DNS survives recreation using only disposable Docker resources."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent/'legacy'))
import release


def main():
    application_image = sys.argv[1]
    proxy_reference = 'nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10'
    database_image = 'pgvector/pgvector:0.8.2-pg16-bookworm'
    prefix = 'dt-entry-test-' + uuid.uuid4().hex[:12]
    network, entry, database = (prefix+'-'+part for part in ('original', 'entry', 'database'))
    volumes = [prefix+'-'+part for part in ('pgdata', 'appdata')]
    containers = [prefix+'-'+part for part in ('proxy', 'blue', 'green', 'legacy')] + [database]
    docker = release.docker
    try:
        try:
            proxy_image = release.inspect(proxy_reference, 'image')['Id']
        except release.ReleaseError:
            docker('pull', proxy_reference)
            proxy_image = release.inspect(proxy_reference, 'image')['Id']
        for name in (network, entry):
            docker('network', 'create', '--internal', name)
        for name in volumes:
            docker('volume', 'create', name)
        docker('run', '-d', '--name', database, '--network', network,
               '--mount', f'type=volume,src={volumes[0]},dst=/var/lib/postgresql/data',
               '-e', 'POSTGRES_USER=fixture', '-e', 'POSTGRES_DB=fixture',
               '-e', 'POSTGRES_HOST_AUTH_METHOD=trust', database_image)
        for _ in range(60):
            ready = subprocess.run(['docker', 'exec', database, 'pg_isready', '-U', 'fixture'], capture_output=True)
            if ready.returncode == 0:
                break
            time.sleep(1)
        else:
            raise AssertionError('fixture database did not become ready')
        docker('exec', database, 'psql', '-U', 'fixture', '-d', 'fixture', '-c',
               'CREATE TABLE marker(value int); INSERT INTO marker VALUES (42);')
        database_id = release.inspect(database)['Id']
        with tempfile.TemporaryDirectory(prefix='dt-entry-runtime-') as directory:
            c = release.Controller(directory)
            c.state = {'format': 1, 'prefix': prefix, 'network': entry, 'database_network': network,
                       'database_id': database_id, 'database_volume': volumes[0],
                       'database_env': {'PGUSER': 'fixture', 'PGDATABASE': 'fixture'},
                       'application_volume': volumes[1], 'proxy_image': proxy_image,
                       'port': 0, 'bind': '127.0.0.1'}
            for color in ('blue', 'green'):
                config = Path(directory)/(color+'.conf')
                config.write_text('events {} http { server { listen 8080; location / { return 200 "'+color+'"; } } }')
                docker('run', '-d', '--name', prefix+'-'+color, '--network', entry,
                       '--mount', f'type=bind,src={config},dst=/fixture.conf,readonly',
                       proxy_image, 'nginx', '-g', 'daemon off;', '-c', '/fixture.conf')
            docker('run', '-d', '--name', prefix+'-legacy', '--network', network,
                   '--network-alias', 'dreamtrans', '--entrypoint', '/bin/sh', proxy_image, '-c', 'sleep 300')
            try:
                c.ensure_proxy('blue')
            except release.ReleaseError as error:
                assert 'another running container' in str(error)
            else:
                raise AssertionError('running legacy alias conflict was not rejected')
            assert network not in release.inspect(c.name('proxy'))['NetworkSettings']['Networks']
            docker('stop', prefix+'-legacy')
            c.ensure_proxy('blue')

            def companion_request(expected):
                # Every request uses a freshly created container on the original
                # network, with no special entry-network attachment to inherit.
                for attempt in range(15):
                    try:
                        value = docker('run', '--rm', '--network', network, '--entrypoint', 'wget',
                                       application_image, '-qO-', '-T', '5', 'http://dreamtrans:8080/')
                        assert value == expected, value
                        return
                    except release.ReleaseError:
                        if attempt == 14:
                            raise
                        time.sleep(1)

            companion_request('blue')
            before = release.inspect(c.name('proxy'))['State']['StartedAt']
            c.state.update(active='blue', phase='ready')
            c.persist()
            release.command(os.environ['DREAMTRANS_CTL'], '--dir', directory, 'sync-entry')
            assert release.inspect(c.name('proxy'))['State']['StartedAt'] == before
            docker('restart', c.name('proxy'))
            companion_request('blue')
            # Exercise the diagnostic on the actual isolated database. Port zero
            # requests fail harmlessly; no public entrance is contacted.
            diagnostics = release.command(os.environ['DREAMTRANS_CTL'], '--dir', directory, 'diagnose')
            assert 'Read-only PostgreSQL statistics' in diagnostics
            assert 'No index changes' in diagnostics
            c.ensure_proxy('green')
            companion_request('green')
            docker('rm', '-f', c.name('proxy'))
            c.ensure_proxy('green')
            companion_request('green')
            assert release.inspect(database)['Id'] == database_id
            assert docker('exec', database, 'psql', '-XAt', '-U', 'fixture', '-d', 'fixture',
                          '-c', 'SELECT value FROM marker') == '42'
            print('Stable companion entry verified: alias conflict refusal, idempotent repair, recreated clients, proxy restart/recreation, blue/green routing, unchanged database.')
    finally:
        for name in containers:
            subprocess.run(['docker', 'rm', '-fv', name], capture_output=True)
        for name in volumes:
            subprocess.run(['docker', 'volume', 'rm', name], capture_output=True)
        for name in (network, entry):
            subprocess.run(['docker', 'network', 'rm', name], capture_output=True)


if __name__ == '__main__':
    main()
