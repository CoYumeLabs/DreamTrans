#!/usr/bin/env python3
"""DreamTrans host-local release controller. Docker is invoked without a shell.

The original Compose project, env and volumes are never recreated or rewritten.
The persisted route is the source of truth after interruption/reboot.
"""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
from urllib.parse import unquote, urlparse


class ReleaseError(Exception):
    pass


def progress(step, message):
    color = sys.stderr.isatty() and not os.environ.get('NO_COLOR')
    prefix = f'\033[36m[{step}]\033[0m' if color else f'[{step}]'
    matched = re.fullmatch(r'(\d+)/(\d+)', step)
    if matched:
        current, total = map(int, matched.groups())
        completed = min(16, int(current * 16 / max(total, 1)))
        prefix += ' [' + '#' * completed + '-' * (16 - completed) + ']'
    print(f'{prefix} {message}', file=sys.stderr, flush=True)


def command(*args, input_text=None):
    result = subprocess.run(list(args), input=input_text, text=True, capture_output=True, check=False)
    if result.returncode:
        # CLI arguments/config/engine errors may contain secrets: no raw command dump.
        raise ReleaseError(f'{args[0]} operation failed (exit {result.returncode}); inspect the relevant service locally')
    return result.stdout.strip()


def docker(*args, **kwargs):
    return command('docker', *args, **kwargs)


def inspect(name, kind='container'):
    return json.loads(docker(kind, 'inspect', name))[0]


def atomic(path, data, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode='w', dir=path.parent, delete=False) as f:
        os.fchmod(f.fileno(), mode)
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
        temporary = f.name
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def file_sha256(path):
    digest=hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda:stream.read(1024*1024),b''):
            digest.update(chunk)
    return digest.hexdigest()


def save(path, value):
    atomic(path, json.dumps(value, indent=2) + '\n')


def archive_configuration(root, state_directory, destination):
    """Include companion deployment settings without copying source trees or caches."""
    import tarfile
    patterns = ('.env', '.env.*', 'docker-compose.yml', 'docker-compose.yaml',
                'docker-compose.*.yml', 'docker-compose.*.yaml',
                'compose.yml', 'compose.yaml', 'compose.*.yml', 'compose.*.yaml')
    files = set()
    for directory in (root, root/'yuaction'):
        for pattern in patterns:
            files.update(p for p in directory.glob(pattern) if p.is_file())
    for name in ('backup.sh', 'release.py'):
        if (root/name).is_file():
            files.add(root/name)
    # Config symlinks must restore without depending on the original host.
    with tarfile.open(destination, 'w', dereference=True) as archive:
        for path in sorted(files):
            archive.add(path, arcname=str(path.relative_to(root)))
        archive.add(state_directory, arcname='.bluegreen', filter=lambda info:
                    None if '/migration/' in info.name or info.name.endswith('/lock') else info)


def read(path):
    return json.loads(Path(path).read_text())


def environment(container):
    return dict(item.split('=', 1) for item in container['Config']['Env'] if '=' in item)


def data_mount(container, target):
    matches = [m for m in container['Mounts'] if m['Destination'] == target]
    if len(matches) != 1 or matches[0]['Type'] != 'volume' or not matches[0]['RW']:
        raise ReleaseError(f'{target} must be an existing writable named volume')
    mount = matches[0]
    volume = inspect(mount['Name'], 'volume')
    if volume['Driver'] != 'local' or volume.get('Options'):
        raise ReleaseError('only ordinary local Docker volumes are supported')
    return mount['Name']


def image_id(reference):
    if not re.fullmatch(r'(?:[\w./:-]+@)?sha256:[0-9a-f]{64}', reference):
        raise ReleaseError('use an immutable repository@sha256:digest (or a local sha256 image ID)')
    if '@' in reference:
        progress('1/8', '拉取固定版本镜像 / pull immutable release')
        docker('pull', reference)
    return inspect(reference, 'image')['Id']


@contextlib.contextmanager
def release_bundle(image):
    name = docker('create', image)
    try:
        with tempfile.TemporaryDirectory(prefix='dreamtrans-release-') as directory:
            docker('cp', f'{name}:/usr/share/dreamtrans/.', directory)
            yield Path(directory)
    finally:
        docker('rm', '-v', name)  # only the never-started extraction container's anonymous volume


def check_contract(new, old=None):
    if new.get('protocol') != 1 or new.get('state_epoch') != 1:
        raise ReleaseError('unsupported release/state protocol; staged conversion required')
    if old and old.get('state_epoch') != new['state_epoch']:
        raise ReleaseError('data epochs are not rollback compatible')
    if old and (new.get('edge_protocol_min', 1) > old.get('edge_protocol_min', 1) or new.get('edge_protocol_max', 1) < old.get('edge_protocol_max', 1)):
        raise ReleaseError('candidate cannot serve protocols already authorized by the active release')
    if not isinstance(new.get('expand_migrations'), list):
        raise ReleaseError('release lacks reviewed expand-only migration manifest')


def check_memory(contract):
    available = next(int(line.split()[1]) for line in Path('/proc/meminfo').read_text().splitlines() if line.startswith('MemAvailable:')) // 1024
    required = max(128, int(contract['minimum_free_memory_mb']))
    if available < required:
        raise ReleaseError(f'可用内存 {available} MiB < {required} MiB；停止发布，不停止当前版本')
    progress('2/8', f'内存检查 {available} MiB 可用，需要 {required} MiB')


class Controller:
    def __init__(self, directory):
        self.root = Path(directory).resolve()
        self.path = self.root / '.bluegreen'
        self.path.mkdir(mode=0o700, exist_ok=True)
        os.chmod(self.path, 0o700)
        self.state_file = self.path / 'state.json'
        self.state = read(self.state_file) if self.state_file.exists() else None

    def persist(self):
        save(self.state_file, self.state)

    def name(self, color):
        return self.state['prefix'] + '-' + color

    def container_exists(self, suffix):
        return self.name(suffix) in docker('container', 'ls', '-a', '--format', '{{.Names}}').splitlines()

    def wait_ready(self, color, attempts=60):
        for _ in range(attempts):
            try:
                self.probe(color)
                return
            except ReleaseError:
                time.sleep(1)
        raise ReleaseError('instance did not become ready; release remains recoverable')

    def ensure_initial_color(self, image, contract):
        # A controller can die after docker run but before recording the color.
        # Adopt only our exact image and persistence mount; never delete it.
        if not self.container_exists('blue'):
            self.start_color('blue', image, contract)
            return
        current = inspect(self.name('blue'))
        if current['Image'] != image:
            raise ReleaseError('initial instance image differs from recorded intent')
        if self.state.get('role') == 'edge':
            expected = str(self.path/'blue'/'spool')
            if not any(m['Destination'] == '/spool' and m['Source'] == expected for m in current['Mounts']):
                raise ReleaseError('initial Edge journal differs from recorded intent')
        elif data_mount(current, '/app/data') != self.state['application_volume']:
            raise ReleaseError('initial application volume differs from recorded intent')
        if self.state['network'] not in current['NetworkSettings']['Networks']:
            docker('network', 'connect', self.state['network'], self.name('blue'))
        if not current['State']['Running']:
            docker('start', self.name('blue'))
        self.state['colors'].setdefault('blue', {'image': image, 'contract': contract})
        self.persist()
        self.wait_ready('blue')

    def ensure_proxy(self, color):
        self.write_route(color)
        if self.container_exists('proxy'):
            current = inspect(self.name('proxy'))
            if current['Image'] != self.state['proxy_image'] or not any(
                m['Destination'] == '/release' and m['Source'] == str(self.path/'proxy')
                for m in current['Mounts']
            ):
                raise ReleaseError('existing proxy differs from recorded installation')
            if not current['State']['Running']:
                docker('start', self.name('proxy'))
            else:
                docker('exec', self.name('proxy'), 'nginx', '-t', '-c', '/release/nginx.conf')
                docker('exec', self.name('proxy'), 'nginx', '-s', 'reload', '-c', '/release/nginx.conf')
        else:
            docker('run', '-d', '--name', self.name('proxy'), '--restart', 'unless-stopped',
                '--network', self.state['network'], '--network-alias', 'dreamtrans',
                '-p', f'{self.state["bind"]}:{self.state["port"]}:8080',
                '--mount', f'type=bind,src={self.path/"proxy"},dst=/release,readonly',
                '--log-opt', 'max-size=10m', '--log-opt', 'max-file=3',
                self.state['proxy_image'], 'nginx', '-g', 'daemon off;', '-c', '/release/nginx.conf')
        for _ in range(30):
            try:
                if self.route_color() == color:
                    return
            except ReleaseError:
                pass
            time.sleep(1)
        raise ReleaseError('initial proxy route did not become ready; rerun initial conversion to resume')

    def control(self, color, action='status'):
        return json.loads(docker('exec', self.name(color), '/app/server', 'deploy-control', action))

    def assert_database(self):
        current = inspect(self.state['database_id'])
        if current['Id'] != self.state['database_id'] or data_mount(current, '/var/lib/postgresql/data') != self.state['database_volume']:
            raise ReleaseError('database identity or production volume changed; stop and investigate')
        if not current['State']['Running']:
            raise ReleaseError('existing database is not running; this command never recreates it')
        inspect(self.state['application_volume'], 'volume')

    def pg(self, sql):
        env = self.state['database_env']
        args = ['exec', '-i']
        for k, v in env.items():
            args.extend(['-e', f'{k}={v}'])
        return docker(*args, self.state['database_id'], 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', input_text=sql)

    def migrate(self, bundle, contract, initial=False):
        self.assert_database()
        rows = self.pg('SELECT version,checksum FROM schema_migrations ORDER BY version;')
        applied = dict(line.split('|') for line in rows.splitlines() if line)
        migrations = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in (bundle/'migrations').glob('*.sql')}
        for version, checksum in applied.items():
            if migrations.get(version) != checksum:
                raise ReleaseError(f'applied migration {version} differs from release bundle')
        pending = set(migrations) - set(applied)
        if pending - set(contract['expand_migrations']):
            raise ReleaseError('pending migrations lack explicit expand-only compatibility: ' + ', '.join(sorted(pending)))
        progress('3/8', f'执行兼容迁移：{len(pending)} 个；保留数据库与卷')
        # Copy into this controller's directory so both Docker daemon and controller see it.
        stage = self.path / 'migration'
        stage.mkdir(exist_ok=True)
        import shutil
        shutil.copytree(bundle/'migrations', stage/'migrations', dirs_exist_ok=True)
        shutil.copy2(bundle/'migrate.sh', stage/'migrate.sh')
        args = ['run', '--rm', '--network', self.state['database_network'], '--mount', f'type=bind,src={stage},dst=/release,readonly']
        for k, v in self.state['database_env'].items():
            args += ['-e', f'{k}={v}']
        args += ['-e', 'MIGRATIONS_DIR=/release/migrations', '--entrypoint', '/bin/sh', self.state['database_image'], '/release/migrate.sh']
        docker(*args)
        self.state['schema'] = migrations
        self.persist()

    def write_route(self, color):
        # Directory mount + atomic rename: live nginx sees the new inode on reload.
        route = self.path/'proxy'/'nginx.conf'
        config = f'''worker_processes auto;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events {{ worker_connections 4096; }}
http {{
  access_log off;
  map $http_upgrade $connection_upgrade {{ default upgrade; '' close; }}
  server {{
    listen 8080;
    client_max_body_size 110m;
    location = /_release {{ default_type text/plain; return 200 "{color}"; }}
    location / {{
      resolver 127.0.0.11 valid=5s ipv6=off;
      set $backend {self.name(color)}:8080;
      proxy_pass http://$backend;
      proxy_http_version 1.1;
      proxy_set_header Host $host;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
      proxy_set_header X-Forwarded-Proto $http_x_forwarded_proto;
      proxy_buffering off;
      proxy_read_timeout 24h;
      proxy_send_timeout 24h;
      proxy_next_upstream off;
    }}
  }}
}}
'''
        atomic(route, config, 0o644)

    def route_color(self):
        return docker('exec', self.name('proxy'), 'wget', '-qO-', 'http://127.0.0.1:8080/_release')

    def switch(self, color):
        self.state['phase'] = 'switching'
        self.state['target'] = color
        self.persist()
        old = self.state.get('active')
        self.control(color, 'active')
        self.write_route(color)
        try:
            docker('exec', self.name('proxy'), 'nginx', '-t', '-c', '/release/nginx.conf')
            docker('exec', self.name('proxy'), 'nginx', '-s', 'reload', '-c', '/release/nginx.conf')
            for _ in range(20):
                if self.route_color() == color:
                    break
                time.sleep(.5)
            else:
                raise ReleaseError('proxy did not acknowledge the new route')
        except Exception:
            if old:
                self.write_route(old)
                docker('exec', self.name('proxy'), 'nginx', '-s', 'reload', '-c', '/release/nginx.conf')
            self.control(color, 'draining')
            raise
        self.state['active'] = color
        self.state['previous'] = old
        self.state['phase'] = 'observing'
        self.persist()
        if old and old != color:
            self.control(old, 'draining')

    def probe(self, color):
        if not inspect(self.name(color))['State']['Running']:
            raise ReleaseError('candidate exited')
        docker('exec', self.name(color), 'wget', '-qO-', 'http://127.0.0.1:8080/readyz')
        status = self.control(color)
        if status['protocol'] != 1:
            raise ReleaseError('control protocol mismatch')

    def start_color(self, color, image, contract):
        name = self.name(color)
        if color in self.state['colors']:
            existing = inspect(name)
            if existing['State']['Running']:
                status = self.control(color)
                if not status['drained']:
                    raise ReleaseError('inactive color still owns work; drain it before another release')
                docker('stop', '--timeout', '-1', name)
            docker('rm', name)  # never remove its production mount
        mode_path = self.path/color
        mode_path.mkdir(exist_ok=True)
        os.chown(mode_path, 10001, 10001)
        atomic(mode_path/'mode', 'standby\n')
        os.chown(mode_path/'mode', 10001, 10001)
        env = dict(self.state['application_env'])
        env.update(RAG_STORAGE='postgres', ALLOW_ANONYMOUS_API='false', DREAMTRANS_DEPLOYMENT_MODE='standby', DREAMTRANS_DEPLOYMENT_STATE='/deployment/mode', PORT='8080')
        env_file = mode_path/'application.env'
        atomic(env_file, ''.join(f'{k}={v}\n' for k, v in env.items()))
        args = ['run', '-d', '--name', name, '--restart', 'unless-stopped', '--network', self.state['database_network'], '--env-file', str(env_file),
                '--mount', f'type=volume,src={self.state["application_volume"]},dst=/app/data',
                '--mount', f'type=bind,src={mode_path},dst=/deployment',
                '--log-opt', 'max-size=10m', '--log-opt', 'max-file=3', '--label', f'dreamtrans.release={self.state["prefix"]}', image]
        docker(*args)
        docker('network', 'connect', self.state['network'], name)
        self.state['colors'][color] = {'image': image, 'contract': contract}
        self.state['phase'] = 'candidate'
        self.state['target'] = color
        self.persist()
        progress('4/8', f'启动 {color}；待命实例不领取后台任务')
        for _ in range(60):
            try:
                self.probe(color)
                break
            except ReleaseError:
                time.sleep(1)
        else:
            raise ReleaseError('candidate readiness failed; active route retained')
        self.control(color, 'canary')
        try:
            for path, marker in [('/pro', 'pro-root'), ('/api/system/access', 'auth')]:
                body = docker('exec', name, 'wget', '-qO-', 'http://127.0.0.1:8080'+path)
                if marker not in body:
                    raise ReleaseError('candidate functional smoke check failed')
        finally:
            self.control(color, 'standby')
        progress('5/8', '就绪与小范围功能检查完成')

    def init(self, args):
        if self.state:
            self.assert_database()
            if self.state.get('initial_image') and not self.state.get('active'):
                if not args.maintenance:
                    raise ReleaseError('interrupted initial conversion requires --maintenance')
                self.resume_initial()
                return
            progress('✓', '已初始化；保留现有节点、配置与数据，请使用 status/resume/deploy')
            return
        if not args.maintenance:
            raise ReleaseError('首次转换需要 --maintenance：请先结束现有转录，转换期间 16002 暂时不可用')
        app, db = inspect(args.app), inspect(args.database)
        if not db['State']['Running']:
            raise ReleaseError('existing production database must be running')
        app_env = environment(app)
        dsn = urlparse(app_env.get('DATABASE_URL', ''))
        if dsn.scheme not in ('postgres', 'postgresql') or not dsn.hostname or not dsn.password:
            raise ReleaseError('existing app must contain a complete PostgreSQL DATABASE_URL')
        networks = set(app['NetworkSettings']['Networks']) & set(db['NetworkSettings']['Networks'])
        network = args.database_network or (next(iter(networks)) if len(networks) == 1 else '')
        if network not in networks:
            raise ReleaseError('specify --database-network shared by the existing application and database')
        bindings = app['HostConfig']['PortBindings'].get('8080/tcp', [])
        if len(bindings) != 1 or bindings[0]['HostPort'] != str(args.port):
            raise ReleaseError('existing application does not own the requested entrance port')
        # Reject unknown persistence mounts instead of quietly dropping an operator's storage.
        if any(m['Destination'] != '/app/data' for m in app['Mounts']):
            raise ReleaseError('additional application mounts need an explicit migration review')
        image = image_id(args.image)
        proxy_image = image_id(args.proxy_image)
        with release_bundle(image) as bundle:
            contract = read(bundle/'release.json')
            check_contract(contract)
            check_memory(contract)
            prefix = 'dreamtrans-' + hashlib.sha256(str(self.root).encode()).hexdigest()[:10]
            self.state = {'format': 1, 'prefix': prefix, 'network': prefix+'-entry', 'database_network': network,
                'database_id': db['Id'], 'database_image': db['Image'], 'database_volume': data_mount(db, '/var/lib/postgresql/data'),
                'application_volume': data_mount(app, '/app/data'), 'application_env': app_env,
                'legacy_id': app['Id'], 'legacy_restart': app['HostConfig']['RestartPolicy']['Name'],
                'proxy_image': proxy_image, 'port': args.port, 'bind': bindings[0]['HostIp'] or '127.0.0.1',
                'database_env': {'PGHOST': dsn.hostname, 'PGPORT': str(dsn.port or 5432), 'PGDATABASE': unquote(dsn.path.lstrip('/')), 'PGUSER': unquote(dsn.username or ''), 'PGPASSWORD': unquote(dsn.password)},
                'active': None, 'previous': None, 'colors': {}, 'phase': 'initializing', 'initial_image': image, 'initial_contract': contract}
            self.persist()
            self.migrate(bundle, contract, initial=True)
        docker('network', 'create', '--label', f'dreamtrans.release={prefix}', self.state['network'])
        progress('维护', '停止旧写入，最终导入 SQLite 与配置；原卷保持原样')
        docker('update', '--restart=no', app['Id'])
        docker('stop', '--timeout', '-1', app['Id'])
        self.state['phase'] = 'importing'
        self.persist()
        self.finish_init(image, contract)

    def resume_initial(self):
        image=self.state['initial_image']
        contract=self.state['initial_contract']
        if self.state['phase']=='initializing':
            with release_bundle(image) as bundle:
                self.migrate(bundle,contract,initial=True)
            networks=docker('network','ls','--format','{{.Name}}').splitlines()
            if self.state['network'] not in networks:
                docker('network','create','--label',f'dreamtrans.release={self.state["prefix"]}',self.state['network'])
            docker('update','--restart=no',self.state['legacy_id'])
            docker('stop','--timeout','-1',self.state['legacy_id'])
            self.state['phase']='importing';self.persist()
        self.finish_init(image,contract)

    def finish_init(self, image, contract):
        env_file = self.path/'import.env'
        atomic(env_file, ''.join(f'{k}={v}\n' for k, v in self.state['application_env'].items()))
        docker('run', '--rm', '--network', self.state['database_network'], '--env-file', str(env_file), '--mount', f'type=volume,src={self.state["application_volume"]},dst=/app/data', '--entrypoint', '/app/server', image, 'deploy-import')
        self.ensure_initial_color(image, contract)
        self.control('blue', 'active')
        self.ensure_proxy('blue')
        self.state.update(active='blue', phase='ready')
        self.persist()
        progress('✓', f'固定入口就绪；YuAction 接入网络 {self.state["network"]}，YUFOLO_URL=http://dreamtrans:8080')

    def deploy(self, args):
        self.assert_database()
        if self.state['phase'] not in ('ready', 'draining'):
            raise ReleaseError('unfinished release; run resume, abort before cutover, or rollback after cutover')
        image = image_id(args.image)
        active = self.state['active']
        color = 'green' if active == 'blue' else 'blue'
        with release_bundle(image) as bundle:
            contract = read(bundle/'release.json')
            check_contract(contract, self.state['colors'][active]['contract'])
            check_memory(contract)
            self.migrate(bundle, contract)
        self.start_color(color, image, contract)
        if args.pause:
            progress('暂停', f'{color} 已验证，执行 resume 切换；当前版本继续服务')
            return
        self.switch(color)
        self.observe(args.observe)
        self.drain(args.drain_timeout)

    def observe(self, seconds):
        progress('6/8', f'新请求已切换；观察 {seconds}s，旧连接继续运行')
        until = time.monotonic()+seconds
        try:
            while time.monotonic() < until:
                self.probe(self.state['active'])
                if self.route_color() != self.state['active']:
                    raise ReleaseError('route changed during observation')
                time.sleep(2)
        except ReleaseError:
            self.rollback()
            raise ReleaseError('observation failed; compatible previous image restored; database writes retained') from None
        self.state['phase'] = 'draining'
        self.persist()

    def drain(self, timeout):
        old = self.state.get('previous')
        if not old:
            return
        if not inspect(self.name(old))['State']['Running']:
            if self.state.get('role') == 'edge' and not self.state['colors'][old].get('empty_spool'):
                raise ReleaseError('stopped Edge journal has not been acknowledged; restart the old instance before draining')
            self.state['phase']='ready';self.persist();return
        until = time.monotonic()+timeout
        while True:
            status = self.control(old, 'draining')
            progress('7/8', f'{old} 排空：WebSocket={status["websockets"]} HTTP={status["requests"]} 任务={status["tasks"]}')
            if status['drained']:
                # Record the acknowledgement before stopping; a crash after stop
                # must not turn an unverified journal into a reusable color.
                if self.state.get('role') == 'edge':
                    self.state['colors'][old]['empty_spool'] = True
                    self.persist()
                docker('stop', '--timeout', '-1', self.name(old))
                self.state['phase'] = 'ready'
                self.persist()
                progress('8/8', '发布完成；旧镜像保留用于兼容回切')
                return
            if time.monotonic() >= until:
                self.state['phase'] = 'draining'
                self.persist()
                progress('待排空', '超时保留旧实例；稍后运行 drain，不强制终止转录')
                return
            time.sleep(3)

    def verify_stopped_candidate(self, color):
        # A main candidate has never received the public route. Its durable
        # work remains in PostgreSQL; never delete its shared application mount.
        if self.state.get('role') == 'edge':
            raise ReleaseError('stopped Edge journal requires an explicit empty check')

    def abort(self):
        if self.state['phase']!='candidate' or self.state['target']==self.state['active']:
            raise ReleaseError('abort is only valid before cutover; use rollback after cutover')
        target=self.state['target']
        running=inspect(self.name(target))['State']['Running']
        if running:
            snapshot=self.control(target,'draining')
            if not snapshot['drained']:
                raise ReleaseError('candidate still owns work; drain it before abort')
        else:
            # Also disables Docker's restart policy while the offline journal
            # is inspected. Never race a restarting owner of the same spool.
            docker('stop','--timeout','-1',self.name(target))
            self.verify_stopped_candidate(target)
        if self.state.get('role') == 'edge':
            self.state['colors'][target]['empty_spool']=True
            self.persist()
        if running:
            docker('stop','--timeout','-1',self.name(target))
        if self.state.get('previous') == target:
            self.state['previous']=None  # This slot now holds the failed candidate.
        self.state.update(phase='ready',target=None)
        self.persist()
        progress('中止','候选实例已停止；原版本与数据库新写入保留')

    def rollback(self):
        old = self.state.get('previous')
        if not old:
            raise ReleaseError('no compatible previous managed release (legacy SQLite image is not a rollback target)')
        self.assert_database()
        check_contract(self.state['colors'][old]['contract'], self.state['colors'][self.state['active']]['contract'])
        if not inspect(self.name(old))['State']['Running']:
            docker('start', self.name(old))
        self.wait_ready(old)
        self.switch(old)
        self.state['phase'] = 'draining'
        self.persist()
        progress('回切', '旧镜像接收新请求；数据库迁移与新写入保留；另一实例等待排空')

    def resume(self, args):
        self.assert_database()
        phase = self.state['phase']
        if phase in ('initializing', 'importing'):
            raise ReleaseError('initial maintenance was interrupted; rerun init --maintenance with the same installation arguments')
        if phase == 'candidate':
            self.probe(self.state['target'])
            self.switch(self.state['target'])
        elif phase == 'switching':
            # Repeat persisted intent; nginx config is atomically written and survives reboot.
            self.switch(self.state['target'])
        if self.state['phase'] == 'observing':
            self.observe(args.observe)
        if self.state['phase'] == 'draining':
            self.drain(args.drain_timeout)

    def snapshot(self, output):
        import tarfile
        self.assert_database()
        if self.state.get('active') not in self.state.get('colors', {}):
            raise ReleaseError('initial conversion is incomplete; keep the pre-conversion backup')
        destination=Path(output).resolve()
        if destination.exists():
            raise ReleaseError('snapshot output already exists')
        destination.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='.snapshot-',dir=destination.parent) as directory:
            stage=Path(directory)
            # One live PostgreSQL session holds the file-retention lock across dump + tar.
            argv=['docker','exec','-i']
            for key,value in self.state['database_env'].items():argv+=['-e',f'{key}={value}']
            argv += [self.state['database_id'],'psql','-XAt','-v','ON_ERROR_STOP=1']
            holder=subprocess.Popen(argv,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
            try:
                holder.stdin.write("SELECT 'locked' FROM pg_advisory_lock(1146243412,54);\n");holder.stdin.flush()
                import selectors
                selector=selectors.DefaultSelector();selector.register(holder.stdout,selectors.EVENT_READ)
                deadline=time.monotonic()+30
                while True:
                    if not selector.select(max(0,deadline-time.monotonic())):
                        raise ReleaseError('backup could not acquire the file-retention lock')
                    line=holder.stdout.readline()
                    if not line:raise ReleaseError('backup lock session failed')
                    if line.strip()=='locked':break
                envfile=stage/'database.env'
                atomic(envfile,''.join(f'{k}={v}\n' for k,v in self.state['database_env'].items()))
                # Named production volume is mounted read-only; no ownership changes.
                docker('run','--rm','--network',self.state['database_network'],'--env-file',str(envfile),
                    '--mount',f'type=volume,src={self.state["application_volume"]},dst=/application,readonly',
                    '--mount',f'type=bind,src={stage},dst=/snapshot','--entrypoint','/bin/sh',self.state['database_image'],
                    '-ec','pg_dump -Fc > /snapshot/database.dump; tar -C /application -cf /snapshot/application.tar .; pg_restore --list /snapshot/database.dump >/dev/null; tar -tf /snapshot/application.tar >/dev/null')
                if holder.poll() is not None:raise ReleaseError('backup lock connection was lost; snapshot not publishable')
                archive_configuration(self.root, self.path, stage/'configuration.tar')
                manifest={'format':1,'database_volume':self.state['database_volume'],'application_volume':self.state['application_volume'],'active_image':self.state['colors'][self.state['active']]['image'],'files':{name:file_sha256(stage/name) for name in ('database.dump','application.tar','configuration.tar')}}
                save(stage/'manifest.json',manifest)
                snapshot=stage/'snapshot.tar'
                with tarfile.open(snapshot,'w') as archive:
                    for name in ('database.dump','application.tar','configuration.tar','manifest.json'):archive.add(stage/name,arcname=name)
                os.chmod(snapshot,0o600)
                os.replace(snapshot,destination)
            finally:
                if holder.poll() is None:
                    holder.stdin.close()
                    try:holder.wait(timeout=5)
                    except subprocess.TimeoutExpired:holder.terminate();holder.wait(timeout=5)
        progress('备份','数据库、应用卷与部署配置快照已创建并附带校验清单')

    def status(self):
        if not self.state:
            return {'initialized': False}
        status = {k:self.state.get(k) for k in ('phase','active','previous','target','network','port','database_volume','application_volume')}
        status['colors'] = {}
        for color, release in self.state['colors'].items():
            info = {'image': release['image']}
            try:
                info.update(self.control(color))
            except ReleaseError:
                info['mode'] = 'stopped/unreachable'
            status['colors'][color] = info
        return status


def main():
    p = argparse.ArgumentParser(description='DreamTrans 蓝绿发布：不会创建或替换生产数据库/数据卷')
    p.add_argument('--dir', default='/root/dreamtrans')
    sub = p.add_subparsers(dest='action', required=True)
    init = sub.add_parser('init', help='首次维护窗口转换')
    init.add_argument('--app', required=True)
    init.add_argument('--database', required=True)
    init.add_argument('--database-network')
    init.add_argument('--image', required=True)
    init.add_argument('--proxy-image', required=True)
    init.add_argument('--port', type=int, default=16002)
    init.add_argument('--maintenance', action='store_true')
    for action in ('deploy','resume','drain'):
        parser = sub.add_parser(action)
        parser.add_argument('--observe', type=int, default=60)
        parser.add_argument('--drain-timeout', type=int, default=60)
        if action == 'deploy':
            parser.add_argument('--image', required=True)
            parser.add_argument('--pause', action='store_true')
    sub.add_parser('rollback')
    sub.add_parser('abort')
    sub.add_parser('status')
    backup=sub.add_parser('snapshot')
    backup.add_argument('--output',required=True)
    args = p.parse_args()
    controller = Controller(args.dir)
    with (controller.path/'lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:
            raise ReleaseError('another release command holds the deployment lock') from None
        if args.action == 'status':
            print(json.dumps(controller.status(), ensure_ascii=False, indent=2))
        elif args.action == 'init':
            controller.init(args)
        elif not controller.state:
            raise ReleaseError('run init in a maintenance window first')
        elif args.action == 'snapshot':
            controller.snapshot(args.output)
        elif args.action == 'abort':
            controller.abort()
        elif args.action == 'rollback':
            controller.rollback()
        elif args.action == 'drain':
            controller.drain(args.drain_timeout)
        else:
            getattr(controller,args.action)(args)


if __name__ == '__main__':
    try:
        main()
    except (ReleaseError, OSError, ValueError, KeyError) as error:
        progress('失败', str(error))
        sys.exit(1)
