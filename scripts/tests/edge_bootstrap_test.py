#!/usr/bin/env python3
"""Run the actual bootstrap against disposable OS/engine/package-manager fixtures."""
import os
import importlib.util
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import sqlite3
import fcntl
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/'scripts'))
spec = importlib.util.spec_from_file_location('edge_install', ROOT/'scripts/edge-install.py')
edge_install = importlib.util.module_from_spec(spec)
spec.loader.exec_module(edge_install)


class EdgeBootstrapTest(unittest.TestCase):
    def test_reconciliation_releases_only_exact_main_archive_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            c=edge_install.EdgeController(directory)
            spool=c.path/'blue'/'spool';spool.mkdir(parents=True)
            payload='{"session_id":"session","generation":1,"sequence":2,"event_id":"event","kind":"end"}'
            with sqlite3.connect(spool/'outbox.db') as db:
                db.execute('PRAGMA journal_mode=WAL')
                db.execute('CREATE TABLE events(session_id TEXT,generation INTEGER,sequence INTEGER,payload TEXT,blocked INTEGER)')
                db.execute('CREATE TABLE counters(session_id TEXT,generation INTEGER)')
                db.execute('INSERT INTO events VALUES(?,?,?,?,1)',('session',1,2,payload))
                db.execute("INSERT INTO counters VALUES('session',1)")
            db.close()
            ack={'session_id':'session','generation':1,'sequence':2,'event_id':'event','archived':True,'disposition':'fenced','payload_hash':edge_install.hashlib.sha256(payload.encode()).hexdigest()}
            with (spool/'owner.lock').open('a') as owner:
                fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
                with self.assertRaisesRegex(edge_install.ReleaseError,'writer'):
                    c.reconcile_spool('blue',{})
            for change in ({'payload_hash':'bad'},{'generation':2},{'archived':False},{'event_id':'wrong'}):
                with patch.object(edge_install,'call',return_value=ack|change):
                    with self.assertRaisesRegex(edge_install.ReleaseError,'mismatch'):
                        c.reconcile_spool('blue',{})
                self.assertFalse((spool/'outbox.db-wal').exists(), 'writer survived release of owner.lock')
                with sqlite3.connect(spool/'outbox.db') as db:self.assertEqual(db.execute('SELECT count(*) FROM events').fetchone()[0],1)
                db.close()
            with patch.object(edge_install,'call',side_effect=edge_install.ReleaseError('offline')):
                with self.assertRaisesRegex(edge_install.ReleaseError,'offline'):c.reconcile_spool('blue',{})
            with patch.object(edge_install,'call',return_value=ack) as server:
                c.reconcile_spool('blue',{});c.reconcile_spool('blue',{})
                server.assert_called_once()
            with sqlite3.connect(spool/'outbox.db') as db:
                self.assertEqual(db.execute('SELECT count(*) FROM events').fetchone()[0],0)
                self.assertEqual(db.execute('SELECT count(*) FROM counters').fetchone()[0],0)
            backups=list((c.path/'reconciliation-audit').glob('*.db'))
            self.assertTrue(backups)
            self.assertTrue(all(p.stat().st_mode & 0o777==0o600 for p in backups))

    def test_reconcile_never_kills_busy_work_and_restarts_idle_writer_on_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            c=edge_install.EdgeController(directory)
            c.state={'phase':'ready','prefix':'node','colors':{'blue':{}},'active':'blue'}
            info={'State':{'Running':True},'HostConfig':{'RestartPolicy':{'Name':'unless-stopped'}}}
            with patch.object(edge_install,'read',return_value={}),patch.object(edge_install,'call'),patch.object(edge_install,'inspect',return_value=info),patch.object(edge_install,'docker') as engine:
                with patch.object(c,'control',return_value={'requests':0,'websockets':1,'tasks':0}):
                    with self.assertRaisesRegex(edge_install.ReleaseError,'live sessions'):c.reconcile()
                    engine.assert_not_called()
                c.state['phase']='draining'  # A release waiting on the old queue must be repairable.
                with patch.object(c,'control',return_value={'requests':0,'websockets':0,'tasks':0}),patch.object(c,'reconcile_spool',side_effect=edge_install.ReleaseError('main unavailable')):
                    with self.assertRaisesRegex(edge_install.ReleaseError,'main unavailable'):c.reconcile()
                    self.assertIn(unittest.mock.call('kill','--signal=KILL',c.name('blue')),engine.call_args_list)
                    self.assertEqual(engine.call_args_list[-1],unittest.mock.call('start',c.name('blue')))
                    self.assertIn(unittest.mock.call('update','--restart=unless-stopped',c.name('blue')),engine.call_args_list)
                # Simulate CLI SIGKILL after the container stopped: recover the
                # persisted restart intent even though Docker now reports stopped.
                engine.reset_mock()
                c.state['reconciliation_restore']={'blue':'unless-stopped'}
                info['State']['Running']=False
                with patch.object(c,'reconcile_spool'):
                    c.reconcile()
                self.assertEqual(engine.call_args_list[-1],unittest.mock.call('start',c.name('blue')))
                self.assertNotIn('reconciliation_restore',c.state)

    def test_stopped_candidate_requires_an_empty_readable_exclusive_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            c=edge_install.EdgeController(directory)
            spool=c.path/'blue'/'spool';spool.mkdir(parents=True)
            c.verify_stopped_candidate('blue')
            with sqlite3.connect(spool/'outbox.db') as db:
                db.execute('CREATE TABLE events(payload TEXT)')
            c.verify_stopped_candidate('blue')
            with (spool/'owner.lock').open('a') as owner:
                fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
                with self.assertRaisesRegex(edge_install.ReleaseError,'owner'):
                    c.verify_stopped_candidate('blue')
            with sqlite3.connect(spool/'outbox.db') as db:
                db.execute("INSERT INTO events VALUES('unsent result')")
            with self.assertRaisesRegex(edge_install.ReleaseError,'unacknowledged'):
                c.verify_stopped_candidate('blue')
            with sqlite3.connect(spool/'outbox.db') as db:
                self.assertEqual(db.execute('SELECT count(*) FROM events').fetchone()[0],1)
            (spool/'outbox.db').write_bytes(b'corrupt journal')
            with self.assertRaisesRegex(edge_install.ReleaseError,'cannot be verified'):
                c.verify_stopped_candidate('blue')

    def test_reinstall_reuses_only_the_nodes_own_retained_network(self):
        with tempfile.TemporaryDirectory() as directory:
            c=edge_install.EdgeController(directory)
            c.state={'network':'node-entry','prefix':'node'}
            with patch.object(edge_install,'docker',return_value='node-entry') as engine, patch.object(edge_install,'inspect',return_value={'Labels':{'dreamtrans.release':'node'}}):
                c.ensure_entry_network()
                engine.assert_called_once_with('network','ls','--format','{{.Name}}')
            with patch.object(edge_install,'docker',return_value='node-entry'), patch.object(edge_install,'inspect',return_value={'Labels':{}}):
                with self.assertRaisesRegex(edge_install.ReleaseError,'not owned'):
                    c.ensure_entry_network()
            c.state['phase']='uninstalled'
            with self.assertRaisesRegex(edge_install.ReleaseError,'new --dir'):
                c.install(None)

    def test_fresh_install_creates_private_root_and_preserves_existing_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)/'new-node'
            argv = ['edge-install.py', '--dir', str(root), 'install']
            def enter_install():
                with patch.object(sys, 'argv', argv), patch.object(os, 'geteuid', return_value=0), patch.object(edge_install.EdgeController, 'install', side_effect=edge_install.ReleaseError('fixture reached install')):
                    with self.assertRaisesRegex(edge_install.ReleaseError, 'fixture reached install'):
                        edge_install.main()
            enter_install()
            self.assertEqual(root.stat().st_mode & 0o777, 0o700)
            root.chmod(0o750)
            (root/'existing-config').write_text('preserve')
            enter_install()
            self.assertEqual(root.stat().st_mode & 0o777, 0o750)
            self.assertEqual((root/'existing-config').read_text(), 'preserve')

    def test_registration_crosses_client_filter_with_identity_and_body_intact(self):
        requests = []
        class Gateway(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append((self.path, self.headers.get('Authorization'), body))
                if self.headers.get('User-Agent', '').startswith('Python-urllib/'):
                    self.send_response(403)
                    self.end_headers()
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"node_id":"registered-node"}')
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(('127.0.0.1', 0), Gateway)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            config = {'main_url': f'http://127.0.0.1:{server.server_port}', 'identity': 'fixture-identity'}
            result = edge_install.call(config, 'register', {'token': 'fixture-registration'})
            self.assertEqual(result, {'node_id': 'registered-node'})
            self.assertEqual(requests, [('/api/edge-control/register', 'Edge fixture-identity', {'token': 'fixture-registration'})])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def run_bootstrap(self, version, image=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / 'bin'
            binary.mkdir()
            release = root / 'os-release'
            release.write_text(f'ID=ubuntu\nVERSION_ID={version}\n')
            source = (ROOT/'scripts/edge-install.sh').read_text()
            script = root/'installer.sh'
            script.write_text(source.replace('/etc/os-release', str(release)))
            log = root/'operations'
            stubs = {
                'id': '#!/bin/sh\necho 0\n',
                'apt-get': '#!/bin/sh\necho "apt-get $*" >> "$BOOTSTRAP_LOG"\n',
                'systemctl': '#!/bin/sh\necho "systemctl $*" >> "$BOOTSTRAP_LOG"\n',
                'docker': '''#!/bin/sh
printf 'docker %s\\n' "$*" >> "$BOOTSTRAP_LOG"
case "$1" in
compose) exit 1;;
create) echo isolated-extraction-container;;
cp) printf 'pass\\n' > "$3";;
esac
''',
            }
            for name, contents in stubs.items():
                path = binary/name
                path.write_text(contents)
                path.chmod(0o755)
            result = subprocess.run(['bash', str(script), image or 'example/edge@sha256:'+'a'*64],
                                    env={**os.environ, 'PATH':str(binary)+':'+os.environ['PATH'], 'BOOTSTRAP_LOG':str(log)},
                                    capture_output=True, text=True, check=False)
            return result.returncode, log.read_text() if log.exists() else '', result.stderr

    def test_supported_lightsail_and_ec2_images_install_missing_compose(self):
        for version in ('24.04','26.04'):
            with self.subTest(version=version):
                code, operations, error = self.run_bootstrap(version)
                self.assertEqual(code, 0, error)
                self.assertIn('apt-get install -y docker.io docker-compose-v2 python3 ca-certificates', operations)
                self.assertIn('docker rm -v isolated-extraction-container', operations)
                self.assertNotIn('docker system prune', operations)

    def test_unsupported_os_stops_before_installing_packages(self):
        code, operations, _ = self.run_bootstrap('22.04')
        self.assertNotEqual(code, 0)
        self.assertNotIn('apt-get', operations)
        self.assertNotIn('docker pull', operations)

    def test_mutable_image_stops_before_engine_or_packages(self):
        code, operations, _ = self.run_bootstrap('24.04', 'example/edge:latest')
        self.assertNotEqual(code, 0)
        self.assertEqual(operations, '')


if __name__ == '__main__':
    unittest.main()
