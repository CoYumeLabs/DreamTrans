#!/usr/bin/env python3
"""Regional Edge lifecycle. Credentials use hidden input or protected files.
The application never receives Docker socket access; release control runs locally.
"""
import argparse
from contextlib import closing
import fcntl
import getpass
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import socket
import sqlite3
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import urlparse

from release import Controller, ReleaseError, atomic, check_contract, check_memory, command, docker, image_id, inspect, progress, read, release_bundle, save


def call(config, path, payload):
    url = config['main_url'].rstrip('/') + '/api/edge-control/' + path
    # Identify the lifecycle client instead of urllib's generic default, which
    # Cloudflare Browser Integrity Check can reject before the main site sees it.
    req = urllib.request.Request(url, json.dumps(payload).encode(), {'Content-Type':'application/json', 'Authorization':'Edge '+config.get('identity',''), 'User-Agent':'DreamTrans-Edge/1.0'})
    # Never follow an identity-bearing request to another origin.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=15) as response:
            server_time = response.headers.get('Date')
            if server_time:
                from email.utils import parsedate_to_datetime
                if abs(time.time()-parsedate_to_datetime(server_time).timestamp()) > 10:
                    raise ReleaseError('本机时钟与主站相差超过 10 秒，请同步时钟')
            return json.load(response)
    except urllib.error.HTTPError as error:
        raise ReleaseError(f'主站请求被拒绝，HTTP {error.code}') from None
    except urllib.error.URLError:
        raise ReleaseError('主站 HTTPS 不可用；停止授权或安装') from None


class EdgeController(Controller):
    def assert_database(self):
        if self.state.get('role') != 'edge':
            raise ReleaseError('directory is not an Edge installation')
        if not (self.root/'config'/'edge.json').is_file():
            raise ReleaseError('existing node configuration is missing')

    def migrate(self, bundle, contract, initial=False):
        check_contract(contract)
        progress('3/8', 'Edge 无主站数据库迁移；保留每个实例尚未回传的独立队列')

    def start_color(self, color, image, contract):
        name = self.name(color)
        directory = self.path/color
        directory.mkdir(exist_ok=True)
        if color in self.state['colors']:
            if inspect(name)['State']['Running']:
                if not self.control(color)['drained']:
                    raise ReleaseError('old color still has sessions or unacknowledged results; release paused')
                docker('stop', '--timeout', '-1', name)
            # Inspect the stopped journal through the old binary before replacing any image.
            # A stopped instance is reusable only after a previously recorded successful drain.
            if not self.state['colors'][color].get('empty_spool'):
                raise ReleaseError('stopped spool has not been verified empty; resume/drain first')
            docker('rm', name)
        for sub in ('spool','deployment'):
            (directory/sub).mkdir(exist_ok=True)
            os.chown(directory/sub,10001,10001)
        atomic(directory/'deployment'/'mode','standby\n')
        os.chown(directory/'deployment'/'mode',10001,10001)
        docker('run','-d','--name',name,'--restart','unless-stopped','--network',self.state['network'],
            '--memory',f'{max(128,int(contract.get("container_memory_mb",256)))}m',
            '--memory-swap',f'{max(128,int(contract.get("container_memory_mb",256)))}m','--pids-limit','128',
            '--mount',f'type=bind,src={self.root/"config"/"edge.json"},dst=/config/edge.json,readonly',
            '--mount',f'type=bind,src={directory/"spool"},dst=/spool',
            '--mount',f'type=bind,src={directory/"deployment"},dst=/deployment',
            '-e','DREAMTRANS_DEPLOYMENT_MODE=standby','-e','DREAMTRANS_DEPLOYMENT_STATE=/deployment/mode',
            '-e',f'APP_VERSION={image}', '--log-opt','max-size=10m','--log-opt','max-file=3',image)
        self.state['colors'][color]={'image':image,'contract':contract,'empty_spool':False}
        self.state.update(phase='candidate',target=color)
        self.persist()
        for _ in range(90):
            try:
                self.probe(color)
                progress('5/8',f'{color} 主站身份与供应商连接检查通过')
                return
            except ReleaseError:
                time.sleep(1)
        raise ReleaseError('candidate did not become ready; current route retained')

    def drain(self, timeout):
        old=self.state.get('previous')
        super().drain(timeout)
        if old and self.state['phase']=='ready':
            self.state['colors'][old]['empty_spool']=True
            self.persist()

    def ensure_entry_network(self):
        network=self.state['network']
        if network in docker('network','ls','--format','{{.Name}}').splitlines():
            existing=inspect(network,'network')
            if existing.get('Labels',{}).get('dreamtrans.release')!=self.state['prefix']:
                raise ReleaseError('existing entry network is not owned by this node; installation refused')
        else:
            docker('network','create','--label',f'dreamtrans.release={self.state["prefix"]}',network)

    def install(self,args):
        if self.state:
            if self.state.get('phase')=='uninstalled':
                raise ReleaseError('installation was uninstalled; keep this audit directory, rotate registration and use a new --dir')
            self.assert_database()
            if not self.state.get('active'):
                self.ensure_entry_network()
                self.finish_install(args)
                return
            progress('✓','已有安装：保留节点身份、配置与队列；使用 status/resume/upgrade')
            return
        if platform.system()!='Linux' or platform.machine() not in ('x86_64','aarch64'):
            raise ReleaseError('supported hosts: Linux amd64/arm64')
        base=urlparse(args.main or '')
        if base.scheme!='https' or base.hostname is None or base.username or base.query or base.fragment:
            raise ReleaseError('--main must be a complete HTTPS main-site origin')
        if not args.proxy_image:
            raise ReleaseError('--proxy-image repository@sha256:digest is required')
        with socket.socket() as check:
            check.bind(('127.0.0.1',args.port))
        image=image_id(args.image);proxy=image_id(args.proxy_image)
        with release_bundle(image) as bundle:
            contract=read(bundle/'release.json');check_contract(contract);check_memory(contract)
        progress('注册','凭证仅通过 HTTPS 正文传递，不写入普通日志')
        token=Path(args.registration_file).read_text().strip() if args.registration_file else getpass.getpass('一次性注册凭证（隐藏）：')
        config={'main_url':args.main}
        registration=call(config,'register',{'token':token})
        config.update(registration)
        provider=Path(args.provider_key_file).read_text().strip() if args.provider_key_file else getpass.getpass('此节点独立 Speechmatics Key（隐藏）：')
        origins=args.origins.split(',') if args.origins else [args.main.rstrip('/')]
        config.update(provider_key=provider,origins=origins,maximum=args.maximum,training=args.training)
        directory=self.root/'config';directory.mkdir(mode=0o700,exist_ok=True)
        save(directory/'edge.json',config);os.chown(directory/'edge.json',10001,10001);os.chmod(directory/'edge.json',0o400)
        prefix='dreamtrans-edge-'+registration['node_id'][:8]
        self.state={'format':1,'role':'edge','prefix':prefix,'network':prefix+'-entry','port':args.port,'bind':'127.0.0.1','proxy_image':proxy,'active':None,'previous':None,'colors':{},'phase':'initializing','initial_image':image,'initial_contract':contract}
        self.persist()
        self.ensure_entry_network()
        self.finish_install(args)

    def finish_install(self,args):
        self.ensure_initial_color(self.state['initial_image'],self.state['initial_contract'])
        self.control('blue','active');self.ensure_proxy('blue')
        if args.tunnel_image:
            tunnel_image=image_id(args.tunnel_image)
            tunnel=read(self.root/'config'/'edge.json').get('tunnel_token') or (Path(args.tunnel_token_file).read_text().strip() if args.tunnel_token_file else getpass.getpass('此节点专用 Tunnel Token（隐藏）：'))
            atomic(self.root/'config'/'tunnel.token',tunnel,0o400)
            # cloudflared's non-root image identity is 65532; never expose the account API token.
            os.chown(self.root/'config'/'tunnel.token',65532,65532)
            if self.container_exists('tunnel'):
                existing=inspect(self.name('tunnel'))
                if existing['Image']!=tunnel_image:
                    raise ReleaseError('existing Tunnel image differs; preserve it and investigate')
                if not existing['State']['Running']:docker('start',self.name('tunnel'))
            else:
                docker('run','-d','--name',self.name('tunnel'),'--restart','unless-stopped','--network',self.state['network'],
                    '--mount',f'type=bind,src={self.root/"config"/"tunnel.token"},dst=/run/tunnel.token,readonly',
                    '--log-opt','max-size=10m','--log-opt','max-file=3',tunnel_image,'tunnel','--no-autoupdate','run','--token-file','/run/tunnel.token')
            self.state['tunnel']=True
        self.state.update(active='blue',phase='ready');self.persist()
        progress('✓',f'节点已安装，入口 127.0.0.1:{self.state["port"]}；在主站启用调度前确认独立 Tunnel 指向 http://dreamtrans:8080')

    def verify_stopped_candidate(self, color):
        spool=self.path/color/'spool'
        with (spool/'owner.lock').open('a') as owner:
            try:fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise ReleaseError('candidate spool still has an owner; abort refused') from None
            database=spool/'outbox.db'
            if not database.exists():
                if any(p.name!='owner.lock' for p in spool.iterdir()):
                    raise ReleaseError('unknown candidate spool files; abort refused')
                return
            try:
                with sqlite3.connect(database.resolve().as_uri()+'?mode=ro',uri=True) as connection:
                    pending=connection.execute('SELECT count(*) FROM events').fetchone()[0]
            except sqlite3.Error:
                raise ReleaseError('candidate journal cannot be verified; abort refused, data retained') from None
            if pending:
                raise ReleaseError('candidate has unacknowledged results; abort refused, data retained')

    def uninstall(self):
        config=read(self.root/'config'/'edge.json')
        call(config,'self-mode',{'mode':'draining'})
        for color in self.state['colors']:
            if inspect(self.name(color))['State']['Running']:
                status=self.control(color,'draining')
                if not status['drained']:
                    raise ReleaseError(f'{color} still has sessions/results; uninstall refused, data retained')
            elif not self.state['colors'][color].get('empty_spool'):
                raise ReleaseError('stopped journal not verified empty; uninstall refused')
        call(config,'self-mode',{'mode':'revoked'})
        unit='dreamtrans-edge-'+self.state['prefix']
        if (Path('/etc/systemd/system')/(unit+'.timer')).exists():
            command('systemctl','disable','--now',unit+'.timer')
        for suffix in list(self.state['colors'])+['proxy']+(['tunnel'] if self.state.get('tunnel') else []):
            docker('stop','--timeout','-1',self.name(suffix));docker('rm',self.name(suffix))
        self.state['phase']='uninstalled';self.persist()
        progress('✓','节点身份已吊销，容器已卸载；配置与审计目录保留，未直接删除队列')

    def reconcile_spool(self, color, config):
        spool=self.path/color/'spool'
        with (spool/'owner.lock').open('a') as owner:
            try:fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise ReleaseError('spool still has a writer; reconciliation refused') from None
            database=spool/'outbox.db'
            if not database.is_file():
                raise ReleaseError('retained journal missing; reconciliation refused')
            with closing(sqlite3.connect(database.resolve().as_uri()+'?mode=rw',uri=True)) as connection:
                connection.execute('PRAGMA synchronous=FULL')
                if connection.execute('PRAGMA integrity_check').fetchone()[0]!='ok':
                    raise ReleaseError('retained journal failed integrity check')
                audit=self.path/'reconciliation-audit'
                audit.mkdir(mode=0o700,exist_ok=True)
                import tempfile
                fd,backup_path=tempfile.mkstemp(prefix=color+'-',suffix='.db',dir=audit)
                os.close(fd)
                with closing(sqlite3.connect(backup_path)) as backup:connection.backup(backup)
                digest=hashlib.sha256(Path(backup_path).read_bytes()).hexdigest()
                progress('对账',f'{color} 审计备份 SHA256={digest}')
                count=0
                # Preserve the original Go-encoded bytes; Python re-encoding can
                # change floats, escaping or omitted fields and invalidate a receipt.
                rows=connection.execute('SELECT session_id,generation,sequence,payload FROM events WHERE blocked=1 ORDER BY session_id,generation,sequence').fetchall()
                for session,generation,sequence,payload in rows:
                    event=json.loads(payload)
                    ack=call(config,'archive',event)
                    expected={'session_id':session,'generation':generation,'sequence':sequence,'event_id':event['event_id'],'payload_hash':hashlib.sha256(payload.encode()).hexdigest()}
                    if ack.get('archived') is not True or ack.get('disposition') not in ('fenced','closed','already_committed') or any(ack.get(k)!=v for k,v in expected.items()):
                        raise ReleaseError('archive receipt mismatch; unacknowledged records retained')
                    with connection:
                        deleted=connection.execute('DELETE FROM events WHERE session_id=? AND generation=? AND sequence=? AND payload=? AND blocked=1',(session,generation,sequence,payload)).rowcount
                        if deleted!=1:raise ReleaseError('journal changed during reconciliation')
                        connection.execute('DELETE FROM counters WHERE session_id=? AND generation=? AND NOT EXISTS(SELECT 1 FROM events WHERE session_id=? AND generation=?)',(session,generation,session,generation))
                    count+=1
                progress('对账',f'{color}: 主站已确认归档 {count} 条，剩余 {connection.execute("SELECT count(*) FROM events").fetchone()[0]} 条')

    def reconcile(self):
        if self.state.get('phase') not in ('ready','draining'):
            raise ReleaseError('reconciliation requires a stable route: ready or draining phase')
        config=read(self.root/'config'/'edge.json')
        call(config,'self-mode',{'mode':'draining'})
        running=[]
        restore=dict(self.state.get('reconciliation_restore',{}))
        for color in self.state['colors']:
            info=inspect(self.name(color))
            if info['State']['Running']:
                status=self.control(color,'draining')
                if status['requests'] or status['websockets'] or status['tasks']:
                    raise ReleaseError('live sessions/tasks remain; retry after they finish, nothing was terminated')
                policy=info['HostConfig']['RestartPolicy']
                restart=policy['Name'] or 'no'
                if restart=='on-failure' and policy.get('MaximumRetryCount'):
                    restart+=':'+str(policy['MaximumRetryCount'])
                running.append((color,restart))
                restore.setdefault(color,restart)
        self.state['reconciliation_restore']=restore
        self.persist()
        try:
            for color,restart in running:
                # Legacy processes wait forever for quarantined records on TERM.
                # Admission is already persistently closed and all work is zero;
                # close only this idle process to obtain the exclusive spool lock.
                docker('update','--restart=no',self.name(color))
                docker('kill','--signal=KILL',self.name(color))
            for color in self.state['colors']:
                self.reconcile_spool(color,config)
        finally:
            failures=[]
            for color,restart in restore.items():
                try:
                    docker('update','--restart='+restart,self.name(color))
                    docker('start',self.name(color))
                except ReleaseError:failures.append(color)
            if failures:raise ReleaseError('reconciliation retained data; retry reconcile to restore containers: '+','.join(failures))
            self.state.pop('reconciliation_restore',None)
            self.persist()
        progress('✓','归档完成；节点保持排空，在主站重新启用调度前检查 status')

    def drain_node(self):
        call(read(self.root/'config'/'edge.json'),'self-mode',{'mode':'draining'})
        for color in self.state['colors']:
            if inspect(self.name(color))['State']['Running']:
                status=self.control(color,'draining')
                progress('排空',f'{color}: 连接={status["websockets"]} 任务={status["tasks"]} 已排空={status["drained"]}')
        progress('排空','已停止新会话调度；保留实例直至会话与未回传队列排空')


def main():
    parser=argparse.ArgumentParser(description='DreamTrans Edge 安装与生命周期')
    parser.add_argument('--dir',default='/opt/dreamtrans-edge')
    parser.add_argument('--image')
    parser.add_argument('--main')
    parser.add_argument('--proxy-image')
    parser.add_argument('--tunnel-image')
    parser.add_argument('--registration-file')
    parser.add_argument('--provider-key-file')
    parser.add_argument('--tunnel-token-file')
    parser.add_argument('--origins')
    parser.add_argument('--port',type=int,default=16003)
    parser.add_argument('--maximum',type=int,default=8)
    parser.add_argument('--training',action='store_true')
    parser.add_argument('--observe',type=int,default=60)
    parser.add_argument('--drain-timeout',type=int,default=60)
    parser.add_argument('--pause',action='store_true')
    parser.add_argument('action',choices=['install','status','logs','diagnose','upgrade','drain','rollback','resume','abort','uninstall','converge','pause-releases','resume-releases','reconcile'],nargs='?',default='install')
    args=parser.parse_args()
    if os.geteuid()!=0:raise ReleaseError('root is required for host lifecycle operations')
    if args.action=='install':
        Path(args.dir).mkdir(mode=0o700,parents=True,exist_ok=True)
    controller=EdgeController(args.dir)
    with (controller.path/'lock').open('a') as lock:
        try:fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:raise ReleaseError('another lifecycle command is running') from None
        if args.action=='install':
            controller.install(args)
            for name in ('edge-install.py','release.py'):
                source=Path(__file__).parent/name
                if source.resolve()!=(controller.root/name).resolve():shutil.copy2(source,controller.root/name)
            unit='dreamtrans-edge-'+controller.state['prefix']
            if ' ' in str(controller.root) or any(c in str(controller.root) for c in '\n\r%'):
                raise ReleaseError('unsupported systemd installation path')
            service='[Unit]\nDescription=DreamTrans Edge release reconciliation\nAfter=docker.service network-online.target\n[Service]\nType=oneshot\nExecStart=/usr/bin/python3 '+str(controller.root/'edge-install.py')+' --dir '+str(controller.root)+' converge\n'
            timer='[Unit]\nDescription=Poll authorized Edge release requests\n[Timer]\nOnBootSec=60\nOnUnitActiveSec=60\nUnit='+unit+'.service\n[Install]\nWantedBy=timers.target\n'
            atomic(Path('/etc/systemd/system')/(unit+'.service'),service,0o644)
            atomic(Path('/etc/systemd/system')/(unit+'.timer'),timer,0o644)
            command('systemctl','daemon-reload');command('systemctl','enable','--now',unit+'.timer')
        elif not controller.state:raise ReleaseError('no existing installation')
        elif args.action in ('status','diagnose'):print(json.dumps(controller.status(),ensure_ascii=False,indent=2))
        elif args.action=='logs':print(docker('logs','--tail','100',controller.name(controller.state['active'])))
        elif args.action=='upgrade':controller.deploy(args)
        elif args.action=='drain':controller.drain_node()
        elif args.action=='reconcile':controller.reconcile()
        elif args.action=='resume':controller.resume(args)
        elif args.action=='rollback':controller.rollback()
        elif args.action=='abort':controller.abort()
        elif args.action=='uninstall':controller.uninstall()
        elif args.action=='pause-releases':controller.state['release_paused']=True;controller.persist();progress('暂停','地区自动发布已暂停，现有服务继续运行')
        elif args.action=='resume-releases':controller.state['release_paused']=False;controller.persist()
        elif args.action=='converge':
            if controller.state.get('release_paused') or controller.state.get('reconciliation_restore'):
                return
            config=read(controller.root/'config'/'edge.json')
            desired=call(config,'deployment',{})
            if desired['mode'] not in ('enabled','draining') or not desired['image']:
                return
            active=controller.state['colors'][controller.state['active']]['image']
            target=image_id(desired['image'])
            if controller.state['phase'] not in ('ready','draining'):
                return  # manual resume decides interrupted or explicitly paused releases
            if active!=target:
                args.image=desired['image'];controller.deploy(args)
            elif controller.state['phase']=='draining':controller.drain(args.drain_timeout)


if __name__=='__main__':
    try:main()
    except (ReleaseError,OSError,ValueError,KeyError) as error:
        progress('失败',str(error));sys.exit(1)
