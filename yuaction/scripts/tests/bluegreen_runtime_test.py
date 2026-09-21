"""Actual Docker/Nginx cutover + rollback while sending unique ordered PCM.

Uses an isolated database/volume/network and a fake upstream; never calls a paid
provider. Browser microphone continuity is independently tested by Playwright.
"""
import base64
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid

REPO = Path(__file__).resolve().parents[3]


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT, **kwargs).strip()


def docker(*args):
    return run('docker', *args)


def inspect(name):
    return json.loads(docker('inspect', name))[0]


def wait(check, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, ValueError, subprocess.CalledProcessError):
            pass
        time.sleep(.1)
    raise AssertionError('condition did not become ready')


class WebSocket:
    def __init__(self, address, cookie):
        u = urllib.parse.urlsplit(address)
        self.socket = socket.create_connection((u.hostname, u.port), timeout=20)
        self.reader = self.socket.makefile('rb')
        nonce = base64.b64encode(os.urandom(16)).decode()
        self.socket.sendall((f'GET {u.path}?{u.query} HTTP/1.1\r\nHost: {u.netloc}\r\n'
                             f'Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n'
                             f'Sec-WebSocket-Key: {nonce}\r\nCookie: {cookie}\r\n\r\n').encode())
        status = self.reader.readline()
        assert b' 101 ' in status, status
        while self.reader.readline() != b'\r\n':
            pass

    def send(self, data, opcode=2):
        mask = os.urandom(4)
        size = len(data)
        header = bytes([0x80 | opcode])
        if size < 126:
            header += bytes([0x80 | size])
        elif size < 65536:
            header += b'\xfe' + struct.pack('!H', size)
        else:
            header += b'\xff' + struct.pack('!Q', size)
        self.socket.sendall(header + mask + bytes(v ^ mask[i % 4] for i, v in enumerate(data)))

    def command(self, kind):
        self.send(json.dumps({'type': kind}).encode(), 1)

    def receive(self):
        header = self.reader.read(2)
        assert len(header) == 2, 'unexpected socket close'
        opcode, size = header[0] & 15, header[1] & 127
        if size == 126:
            size = struct.unpack('!H', self.reader.read(2))[0]
        elif size == 127:
            size = struct.unpack('!Q', self.reader.read(8))[0]
        data = self.reader.read(size)
        if opcode == 9:
            self.send(data, 10)
            return self.receive()
        assert opcode == 1, (opcode, data)
        return json.loads(data)

    def close(self):
        self.reader.close()
        self.socket.close()


def main():
    source_backend, source_frontend = sys.argv[1:]
    fixture = 'yua-bg-' + uuid.uuid4().hex[:10]
    network, volume = fixture + '-dbnet', fixture + '-data'
    db, legacy, frontend = (fixture + '-' + name for name in ('db', 'legacy', 'frontend'))
    containers = [frontend, legacy, db]
    images = []
    entry = None
    upstream = None
    producer = None
    stop = threading.Event()
    ws = None
    try:
        with tempfile.TemporaryDirectory(prefix='yuaction-bg-runtime-') as directory:
            root = Path(directory)
            binary = root / 'dreamtransctl'
            artifact = docker('create', source_backend)
            try:
                docker('cp', artifact + ':/usr/share/dreamtrans/dreamtransctl', str(binary))
            finally:
                docker('rm', '-v', artifact)
            binary.chmod(0o700)
            def cli_args(*args):
                return [str(binary), 'yuaction', '--dir', str(root), *args]
            def cli(*args):
                return run(*cli_args(*args))
            def state():
                return json.loads((root / '.bluegreen/state.json').read_text())
            docker('network', 'create', network)
            docker('volume', 'create', volume)
            docker('run', '-d', '--name', db, '--network', network, '--network-alias', 'db',
                   '--mount', 'type=volume,src=' + volume + ',dst=/var/lib/postgresql/data',
                   '-e', 'POSTGRES_USER=fixture', '-e', 'POSTGRES_DB=fixture', '-e', 'POSTGRES_PASSWORD=fixture', 'postgres:16-alpine')
            wait(lambda: subprocess.run(['docker', 'exec', db, 'pg_isready', '-h', '127.0.0.1', '-U', 'fixture'], capture_output=True).returncode == 0)
            fixture_binary = Path(os.environ.get('YUACTION_RUNTIME_FIXTURE_BINARY', str(root / 'upstream.test')))
            if 'YUACTION_RUNTIME_FIXTURE_BINARY' not in os.environ:
                run('go', 'test', '-c', '-tags=e2e', '-o', str(fixture_binary), './internal/app', cwd=REPO / 'yuaction/backend', env=dict(os.environ, CGO_ENABLED='0'))
            upstream = fixture + '-upstream'
            containers.append(upstream)
            docker('run', '-d', '--name', upstream, '--network', network, '--network-alias', 'upstream',
                   '-p', '127.0.0.1::18086', '--mount', f'type=bind,src={fixture_binary},dst=/tmp/upstream.test,readonly',
                   '-e', 'YUACTION_DOCKER_FIXTURE_ADDR=0.0.0.0:18086', '--entrypoint', '/tmp/upstream.test',
                   source_backend, '-test.run', '^TestDockerUpstreamFixture$', '-test.timeout', '0')
            fixture_port = inspect(upstream)['NetworkSettings']['Ports']['18086/tcp'][0]['HostPort']
            def metrics():
                with urllib.request.urlopen(f'http://127.0.0.1:{fixture_port}/__fixture/stats') as response:
                    return json.load(response)
            wait(lambda: metrics()['bytes'] == 0)
            docker('run', '-d', '--name', legacy, '--network', network, '--network-alias', 'backend',
                   '-e', 'DATABASE_URL=postgres://fixture:fixture@db:5432/fixture?sslmode=disable',
                   '-e', 'YUACTION_CREATOR_KEY=' + 'k' * 40,
                   '-e', 'YUFOLO_URL=http://upstream:18086', '-e', 'TRUST_PROXY=true', source_backend)
            docker('run', '-d', '--name', frontend, '--network', network, '-p', '127.0.0.1::80', source_frontend)
            port = inspect(frontend)['NetworkSettings']['Ports']['80/tcp'][0]['HostPort']
            base = 'http://127.0.0.1:' + port
            wait(lambda: json.load(urllib.request.urlopen(base + '/api/health'))['status'] == 'ok')
            prefix = 'yuaction-' + hashlib.sha256(str(root).encode()).hexdigest()[:12]
            entry = prefix + '-entry'
            containers += [prefix + '-' + name for name in ('proxy', 'blue', 'blue-frontend', 'green', 'green-frontend')]
            proxy = 'nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10'
            cli('init', '--app', legacy, '--frontend', frontend, '--database', db, '--port', port,
                '--database-network', network, '--image', inspect(source_backend)['Id'], '--frontend-image', inspect(source_frontend)['Id'], '--proxy-image', proxy, '--maintenance')
            assert state()['active'] == 'blue' and state()['phase'] == 'ready'
            assert state()['database_volume'] == volume
            assert not inspect(legacy)['State']['Running']
            jar = http.cookiejar.CookieJar()
            client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
            def api(path, value=None):
                request = urllib.request.Request(base + path, headers={'Content-Type': 'application/json'}, data=None if value is None else json.dumps(value).encode())
                with client.open(request, timeout=10) as response:
                    return json.load(response)
            api('/api/auth/login', {'email': 'teacher@example.com', 'password': 'test-password'})
            room = api('/api/rooms', {'title': 'Real blue green microphone', 'kind': 'classroom'})['room']['code']
            api('/api/rooms/' + room + '/transcription', {'sourceLanguage': 'cmn'})
            cookie = '; '.join(c.name + '=' + c.value for c in jar)
            ws_url = base + '/api/rooms/' + room + '/audio?sampleRate=48000&protocol=1&capture=runtime-continuous-recording'
            ws = WebSocket(ws_url, cookie)
            assert ws.receive()['type'] == 'ready'
            lock = threading.Lock()
            current = [ws]
            queued, captured, errors = [], bytearray(), []
            def capture():
                sequence = 0
                try:
                    while not stop.is_set():
                        sequence += 1
                        pcm = struct.pack('<I', sequence) * 480
                        with lock:
                            captured.extend(pcm)
                            if current[0] is None:
                                queued.append(pcm)
                            else:
                                current[0].send(pcm)
                        time.sleep(.02)
                except Exception as error:
                    errors.append(error)
            producer = threading.Thread(target=capture)
            producer.start()
            wait(lambda: metrics()['bytes'] > 10000)
            # Distinct immutable images with the same source contract exercise
            # creation of both colors rather than the no-op same-image path.
            newer = []
            for source, role in ((source_backend, 'backend'), (source_frontend, 'frontend')):
                tag = fixture + '-' + role + ':new'
                images.append(tag)
                run('docker', 'build', '-t', tag, '-', input=f'FROM {source}\nLABEL org.opencontainers.image.revision={"b"*40}\n')
                newer.append(inspect(tag)['Id'])
            # A mismatched candidate is rejected before route/recording changes.
            before_state = (root / '.bluegreen/state.json').read_bytes()
            rejected = subprocess.run(cli_args('deploy', '--image', newer[0], '--frontend-image', inspect(source_frontend)['Id']), capture_output=True)
            assert rejected.returncode != 0
            assert (root / '.bluegreen/state.json').read_bytes() == before_state
            assert metrics()['starts'] == 1 and not errors
            proxy_id = inspect(prefix + '-proxy')['Id']
            for operation in [('deploy', '--image', newer[0], '--frontend-image', newer[1], '--observe', '0'), ('rollback',)]:
                output = open(root / ('-'.join(operation[:1]) + '.log'), 'w+')
                child = subprocess.Popen(cli_args(*operation, '--drain-timeout', '30'), stdout=output, stderr=output)
                assert ws.receive()['type'] == 'handoff'
                # Offers are retryable while a client preflights the new route.
                # Deliver another offer deterministically before acknowledging.
                docker('exec', prefix + '-' + state()['previous'], '/app/yuaction', 'deploy-control', 'handoff')
                with lock:
                    current[0] = None
                    ws.command('handoff')
                deadline = time.monotonic() + 20
                repeated_offers = 0
                while True:
                    message = ws.receive()
                    if message == {'type': 'handoff', 'version': 1}:
                        repeated_offers += 1
                        assert time.monotonic() < deadline, 'handoff never completed'
                        continue
                    assert message == {'type': 'migrated', 'version': 1}, message
                    break
                assert repeated_offers >= 1, 'duplicate-offer scenario was not exercised'
                ws.close()
                ws = WebSocket(ws_url, cookie)
                assert ws.receive()['type'] == 'ready'
                with lock:
                    for pcm in queued:
                        ws.send(pcm)
                    queued.clear()
                    current[0] = ws
                assert child.wait(timeout=45) == 0, output.seek(0) or output.read()
                assert state()['phase'] == 'ready'
                assert inspect(prefix + '-proxy')['Id'] == proxy_id
                assert api('/api/auth/me')['id'] == 'teacher'
                time.sleep(.3)
            stop.set();producer.join(timeout=5)
            assert not errors, errors
            ws.command('stop');assert ws.receive()['type'] == 'stopped'
            ws.close();ws = None
            wait(lambda: metrics()['connections'] == 0)
            result = metrics()
            checksum = 14695981039346656037
            for value in captured:
                checksum = ((checksum ^ value) * 1099511628211) & ((1 << 64) - 1)
            assert result['bytes'] == len(captured), (result, len(captured))
            assert result['hash'] == f'{checksum:016x}', result
            assert result['starts'] == 3 and result['maxConnections'] == 1, result
            assert result['archives'] == 3, result
            assert state()['database_volume'] == volume
            print('PASS: real fixed-port Nginx upgrade/rollback, continuous ordered PCM with exact hash, single paid stream, shared login, finals archived, old colors drained, database retained.')
    except Exception:
        for name in containers:
            subprocess.run(['docker', 'logs', '--tail', '20', name], check=False)
        raise
    finally:
        stop.set()
        if producer:
            producer.join(timeout=5)
        if ws:
            ws.close()
        for name in reversed(containers):
            subprocess.run(['docker', 'rm', '-fv', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for name in (entry, network):
            if name:
                subprocess.run(['docker', 'network', 'rm', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(['docker', 'volume', 'rm', volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for name in images:
            subprocess.run(['docker', 'rmi', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
