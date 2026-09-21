"""Exercise built YuAction images on an isolated shared-schema database and port."""
import json
import subprocess
import sys
import time
import urllib.request
import uuid


def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()


def main():
    backend_image, frontend_image = sys.argv[1:]
    prefix = 'yuaction-runtime-' + uuid.uuid4().hex[:10]
    database, backend, frontend = (prefix + '-' + name for name in ('db', 'backend', 'frontend'))
    containers = [frontend, backend, database]
    key = 'runtime-only-creator-' + uuid.uuid4().hex
    try:
        docker('network', 'create', prefix)
        docker('run', '-d', '--name', database, '--network', prefix,
               '--network-alias', 'db', '-e', 'POSTGRES_USER=fixture',
               '-e', 'POSTGRES_DB=fixture', '-e', 'POSTGRES_PASSWORD=fixture-test',
               'postgres:16-alpine')
        for _ in range(60):
            if subprocess.run(['docker', 'exec', database, 'pg_isready', '-h', '127.0.0.1', '-U', 'fixture'],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                break
            time.sleep(1)
        else:
            raise AssertionError('isolated database did not become ready')
        docker('exec', database, 'psql', '-U', 'fixture', '-d', 'fixture', '-v', 'ON_ERROR_STOP=1', '-c',
               'CREATE SCHEMA yuaction; CREATE TABLE public.parent_marker(value int); INSERT INTO public.parent_marker VALUES (42)')

        def start_backend():
            docker('run', '-d', '--name', backend, '--network', prefix, '--network-alias', 'backend',
                   '-e', 'DATABASE_URL=postgres://fixture:fixture-test@db:5432/fixture?sslmode=disable&search_path=yuaction',
                   '-e', 'YUACTION_CREATOR_KEY=' + key, '-e', 'TRUST_PROXY=true', backend_image)
        start_backend()
        docker('run', '-d', '--name', frontend, '--network', prefix,
               '-p', '127.0.0.1::80', frontend_image)
        address = 'http://' + docker('port', frontend, '80/tcp')

        def request(path, body=None, token=None):
            headers = {'Content-Type': 'application/json'}
            if token:
                headers['Authorization'] = 'Bearer ' + token
            req = urllib.request.Request(address + path,
                                         data=json.dumps(body).encode() if body is not None else None,
                                         headers=headers)
            with urllib.request.urlopen(req, timeout=5) as response:
                return json.load(response)

        def ready():
            for _ in range(60):
                try:
                    if request('/api/health')['status'] == 'ok':
                        return
                except (OSError, ValueError):
                    pass
                time.sleep(1)
            raise AssertionError('YuAction frontend port did not proxy a healthy backend')
        ready()
        assert docker('exec', backend, 'id', '-u') == '10001'
        with urllib.request.urlopen(address + '/', timeout=5) as response:
            assert b'id="root"' in response.read()
        created = request('/api/rooms', {'title': 'Monorepo persistence fixture', 'kind': 'classroom'}, key)
        code = created['room']['code']
        before = request('/api/rooms/' + code)
        assert before['title'] == 'Monorepo persistence fixture'
        docker('rm', '-f', backend)
        start_backend()
        # Nginx's static upstream resolves on startup, as in Compose recreation.
        docker('restart', frontend)
        # Docker may allocate a different ephemeral host port on restart.
        address = 'http://' + docker('port', frontend, '80/tcp')
        ready()
        after = request('/api/rooms/' + code)
        assert after['code'] == code and after['revision'] == before['revision']
        request('/api/rooms/' + code + '/host', token=created['hostKey'])
        sql = "SELECT value FROM public.parent_marker; SELECT count(*) FROM yuaction.rooms; SELECT count(*) FROM yuaction.yuaction_schema_migrations; SELECT to_regclass('public.rooms') IS NULL;"
        assert docker('exec', database, 'psql', '-XAt', '-U', 'fixture', '-d', 'fixture', '-c', sql).splitlines() == ['42', '1', '3', 't']
        print('PASS: independent HTTP port, real PostgreSQL migrations, room/host recovery after recreation, parent schema preserved.')
    except Exception:
        for name in containers:
            subprocess.run(['docker', 'logs', '--tail', '30', name], check=False)
        raise
    finally:
        for name in containers:
            subprocess.run(['docker', 'rm', '-fv', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(['docker', 'network', 'rm', prefix], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
