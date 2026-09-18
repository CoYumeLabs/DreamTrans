"""Installer behavior tests; no daemon, package manager, network, or real data touched."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/install.sh"
REVISION = "a" * 40

MOCK = r'''#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ['MOCK_LOG'], 'a') as f:
    f.write(json.dumps({'tool':tool, 'args':args, 'ambient_password': 'POSTGRES_PASSWORD' in os.environ})+'\n')
revision=os.environ.get('MOCK_REVISION','a'*40)
if tool=='curl':
    url=next(a for a in args if a.startswith(('https://','http://')))
    if url.endswith('/api/health'):
        print('{"status":"ok"}'); sys.exit(0)
    dest=args[args.index('-o')+1]
    root=pathlib.Path(os.environ['MOCK_REPO'])
    if url.endswith(('/compose.ghcr.yml','/compose.dreamtrans.yml')):
        if os.environ.get('MOCK_BAD_DOWNLOAD'): pathlib.Path(dest).write_text('malformed')
        else: shutil.copyfile(root/url.rsplit('/',1)[1],dest)
    elif url.endswith('/scripts/install.sh'): shutil.copyfile(root/'scripts/install.sh',dest)
    else: sys.exit('unexpected URL')
elif tool=='docker':
    if args==['info']: sys.exit(0)
    if args[:2]==['compose','version']: print('2.35.1'); sys.exit(0)
    if args[0]=='ps':
        if args[-1]=='label=com.docker.compose.project.working_dir':
            if os.environ.get('MOCK_DREAMTRANS'): print('parent-db')
            sys.exit(0)
        if os.environ.get('MOCK_OTHER_OWNER'): print('other-container')
        sys.exit(0)
    if args[0]=='inspect':
        if args[-1]=='parent-db':
            template=args[args.index('--format')+1]
            if 'working_dir' in template: print(os.environ['MOCK_DREAMTRANS'])
            elif 'com.docker.compose.service' in template: print(os.environ.get('MOCK_DB_SERVICE','db'))
            elif '.NetworkSettings.Networks' in template: print('dreamtrans_default')
            elif '.Name' in template: print('/dreamtrans-db-1')
            else: sys.exit('unexpected inspect')
        else: print(os.environ['MOCK_OTHER_OWNER'])
        sys.exit(0)
    if args[:2]==['volume','inspect']: sys.exit(0 if os.environ.get('MOCK_VOLUME') else 1)
    if args[0]=='pull': sys.exit(1 if os.environ.get('MOCK_FAIL_PULL') else 0)
    if args[:2]==['image','inspect']:
        if os.environ.get('MOCK_MISMATCH') and '-frontend:' in args[-1]: print('b'*40)
        else: print(revision)
        sys.exit(0)
    if args[0]=='compose':
        if 'config' in args: sys.exit(1 if os.environ.get('MOCK_BAD_DOWNLOAD') else 0)
        if 'pull' in args: sys.exit(1 if os.environ.get('MOCK_FAIL_PAIR_PULL') else 0)
        if 'psql' in args:
            if os.environ.get('MOCK_DB_FAIL'): sys.exit(1)
            if 'SELECT EXISTS' in args[-1]: print('t' if os.environ.get('MOCK_SCHEMA_EXISTS') else 'f')
            sys.exit(0)
        if 'pg_dump' in args:
            if os.environ.get('MOCK_FAIL_BACKUP'): sys.exit(1)
            sys.stdout.buffer.write(b'PGDMP-fake-test-backup'); sys.exit(0)
        if 'up' in args or 'ps' in args or 'logs' in args: sys.exit(0)
    sys.exit('unexpected Docker invocation: '+repr(args))
else: sys.exit('unexpected mock command')
'''


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="yuaction-installer-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.install = self.root / "install with spaces"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for tool in ("docker", "curl"):
            path = self.bin / tool
            path.write_text(MOCK)
            path.chmod(0o755)
        self.log = self.root / "commands.jsonl"
        self.env = dict(os.environ, PATH=str(self.bin) + ":" + os.environ["PATH"],
                        MOCK_REPO=str(ROOT), MOCK_LOG=str(self.log))

    def run_installer(self, *args, ok=True, **env):
        result = subprocess.run(["bash", str(SCRIPT), "--dir", str(self.install),
                                 "--no-docker-install", *args],
                                env=dict(self.env, **env), text=True, capture_output=True, timeout=15)
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, "unexpected success")
        return result

    def config(self):
        return dict(line.split("=", 1) for line in (self.install / ".env").read_text().splitlines() if "=" in line)

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def test_fresh_install_generates_private_credentials_and_pins_pair(self):
        result = self.run_installer("--port", "12345", "--bind", "0.0.0.0", POSTGRES_PASSWORD="ambient-must-not-win")
        cfg = self.config()
        self.assertRegex(cfg["POSTGRES_PASSWORD"], r"^[a-f0-9]{64}$")
        self.assertRegex(cfg["YUACTION_CREATOR_KEY"], r"^[a-f0-9]{64}$")
        self.assertNotEqual(cfg["POSTGRES_PASSWORD"], cfg["YUACTION_CREATOR_KEY"])
        self.assertEqual(cfg["IMAGE_TAG"], "sha-" + REVISION)
        self.assertEqual(cfg["APP_PORT"], "12345")
        self.assertEqual(cfg["YUACTION_DEMO"], "false")
        self.assertEqual((self.install / ".env").stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.install / "install.sh").stat().st_mode & 0o777, 0o700)
        self.assertNotIn(cfg["YUACTION_CREATOR_KEY"], result.stdout + result.stderr)
        for call in self.calls():
            if call["tool"] == "docker" and call["args"][0] == "compose" and "--env-file" in call["args"]:
                self.assertFalse(call["ambient_password"])
        urls = [arg for c in self.calls() for arg in c["args"] if arg.startswith("https:")]
        self.assertIn(f"https://raw.githubusercontent.com/CoYumeLabs/YuAction/{REVISION}/compose.ghcr.yml", urls)

    def test_update_preserves_secrets_settings_and_backs_up_before_recreate(self):
        self.run_installer("--project", "custom-class", "--port", "12346")
        original = self.config()
        original_text = (self.install / ".env").read_text()
        self.log.write_text("")
        self.run_installer("--update", MOCK_REVISION="b" * 40)
        current = self.config()
        for key in ("POSTGRES_PASSWORD", "YUACTION_CREATOR_KEY", "APP_PORT", "APP_BIND"):
            self.assertEqual(original[key], current[key])
        self.assertEqual(current["IMAGE_TAG"], "sha-" + "b" * 40)
        backups = list((self.install / "backups").iterdir())
        self.assertEqual(len(backups), 1)
        self.assertEqual((backups[0] / ".env").read_text(), original_text)
        self.assertTrue((backups[0] / "database.dump").read_bytes().startswith(b"PGDMP"))
        calls = self.calls()
        backup = next(i for i, c in enumerate(calls) if "pg_dump" in c["args"])
        recreate = next(i for i, c in enumerate(calls) if "--no-build" in c["args"])
        self.assertLess(backup, recreate)
        for c in calls:
            self.assertNotIn("down", c["args"])
            if "--project-name" in c["args"]:
                self.assertIn("custom-class", c["args"])

    def test_download_pull_and_version_errors_leave_active_config_unchanged(self):
        self.run_installer()
        original = (self.install / ".env").read_bytes()
        for env in ({"MOCK_FAIL_PULL": "1"}, {"MOCK_FAIL_PAIR_PULL": "1"},
                    {"MOCK_MISMATCH": "1"}, {"MOCK_BAD_DOWNLOAD": "1"}):
            with self.subTest(env=env):
                self.log.write_text("")
                self.run_installer("--update", ok=False, **env)
                self.assertEqual((self.install / ".env").read_bytes(), original)
                self.assertFalse(any("up" in c["args"] for c in self.calls()))

    def test_failed_backup_stops_before_replacing_config(self):
        self.run_installer()
        original = (self.install / ".env").read_bytes()
        self.log.write_text("")
        self.run_installer("--update", ok=False, MOCK_FAIL_BACKUP="1", MOCK_REVISION="b" * 40)
        self.assertEqual((self.install / ".env").read_bytes(), original)
        self.assertFalse(any("--no-build" in c["args"] for c in self.calls()))

    def test_existing_project_or_orphaned_volume_is_not_adopted(self):
        self.run_installer(ok=False, MOCK_OTHER_OWNER="/some/other/install")
        self.assertFalse((self.install / ".env").exists())
        self.run_installer(ok=False, MOCK_VOLUME="1")
        self.assertFalse((self.install / ".env").exists())

    def test_env_is_not_executed_and_key_requires_explicit_action(self):
        self.run_installer()
        marker = self.root / "must-not-exist"
        with (self.install / ".env").open("a") as f:
            f.write(f"UNKNOWN=$(touch '{marker}')\n")
        result = self.run_installer("--update")
        self.assertFalse(marker.exists())
        self.assertNotIn(self.config()["YUACTION_CREATOR_KEY"], result.stdout)
        shown = self.run_installer("--show-key")
        self.assertEqual(shown.stdout.strip(), self.config()["YUACTION_CREATOR_KEY"])

    def test_rejects_unknown_arguments_invalid_ports_and_unrelated_directory(self):
        self.run_installer("--port", "99999", ok=False)
        self.assertFalse((self.install / ".env").exists())
        self.run_installer("--dir", ok=False)
        (self.install / "unrelated.txt").write_text("do not touch")
        self.run_installer(ok=False)
        self.assertEqual((self.install / "unrelated.txt").read_text(), "do not touch")

    def parent_install(self):
        parent = self.root / 'dreamtrans'
        parent.mkdir()
        (parent / '.env').write_text('POSTGRES_PASSWORD=parent-secret\nIMAGE_TAG=parent-release\nJWT_SECRET=parent-jwt\n')
        (parent / 'docker-compose.yml').write_text('services: {}\n')
        return parent

    def test_dreamtrans_reads_parent_without_copying_secrets_and_preserves_it(self):
        parent = self.parent_install()
        original = (parent / '.env').read_bytes()
        self.run_installer('--dreamtrans-dir', str(parent), MOCK_DREAMTRANS=str(parent))
        cfg = self.config()
        self.assertNotIn('POSTGRES_PASSWORD', cfg)
        self.assertNotIn('JWT_SECRET', cfg)
        self.assertEqual(cfg['IMAGE_TAG'], 'sha-' + REVISION)
        self.assertEqual(cfg['DREAMTRANS_DB_HOST'], 'dreamtrans-db-1')
        self.assertEqual(cfg['DREAMTRANS_NETWORK'], 'dreamtrans_default')
        self.assertEqual((self.install / '.dreamtrans-dir').read_text().strip(), str(parent))
        self.log.write_text('')
        self.run_installer('--update', MOCK_DREAMTRANS=str(parent), MOCK_REVISION='b'*40)
        self.assertEqual((parent / '.env').read_bytes(), original)
        self.assertEqual(self.config()['YUACTION_CREATOR_KEY'], cfg['YUACTION_CREATOR_KEY'])
        calls = self.calls()
        dump = next(c for c in calls if 'pg_dump' in c['args'])
        self.assertIn('--schema=yuaction', dump['args'])
        for call in calls:
            args = call['args']
            if '--env-file' in args:
                self.assertLess(args.index(str(parent / '.env')), args.index(str(self.install / '.env')) if str(self.install / '.env') in args else len(args))
            self.assertFalse('up' in args and args[-1]=='db')
            self.assertNotIn('parent-secret', args)

    def test_dreamtrans_requires_running_parent_and_refuses_orphaned_schema(self):
        parent = self.parent_install()
        self.run_installer('--dreamtrans-dir', str(parent), ok=False)
        self.assertFalse((self.install / '.env').exists())
        self.run_installer('--dreamtrans-dir', str(parent), ok=False,
                           MOCK_DREAMTRANS=str(parent), MOCK_SCHEMA_EXISTS='1')
        self.assertFalse((self.install / '.env').exists())
        self.assertFalse(any('CREATE SCHEMA yuaction' in c['args'] for c in self.calls()))

    def test_dreamtrans_accepts_repository_postgres_service_and_rejects_config_override(self):
        parent = self.parent_install()
        self.run_installer('--dreamtrans-dir', str(parent), MOCK_DREAMTRANS=str(parent), MOCK_DB_SERVICE='postgres')
        with (self.install / '.env').open('a') as stream:
            stream.write('POSTGRES_PASSWORD=wrong\n')
        self.log.write_text('')
        self.run_installer('--update', ok=False, MOCK_DREAMTRANS=str(parent))
        self.assertFalse(any('pg_dump' in c['args'] or 'up' in c['args'] for c in self.calls()))

    def test_standalone_install_cannot_silently_switch_to_shared_database(self):
        self.run_installer()
        original = (self.install / '.env').read_bytes()
        parent = self.parent_install()
        self.run_installer('--dreamtrans-dir', str(parent), ok=False, MOCK_DREAMTRANS=str(parent))
        self.assertEqual((self.install / '.env').read_bytes(), original)

    def test_shared_backup_failure_keeps_active_config_and_containers(self):
        parent = self.parent_install()
        self.run_installer('--dreamtrans-dir', str(parent), MOCK_DREAMTRANS=str(parent))
        original = (self.install / '.env').read_bytes()
        self.log.write_text('')
        self.run_installer('--update', ok=False, MOCK_DREAMTRANS=str(parent), MOCK_FAIL_BACKUP='1', MOCK_REVISION='b'*40)
        self.assertEqual((self.install / '.env').read_bytes(), original)
        self.assertFalse(any('up' in c['args'] for c in self.calls()))

    def test_dreamtrans_defaults_to_child_directory_and_rejects_parent_overwrite(self):
        parent = self.parent_install()
        result = subprocess.run(['bash', str(SCRIPT), '--dreamtrans-dir', str(parent), '--no-docker-install'],
                                env=dict(self.env, MOCK_DREAMTRANS=str(parent)), capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((parent / 'yuaction' / '.env').exists())
        self.run_installer('--dreamtrans-dir', str(parent), '--dir', str(parent), ok=False)
        self.assertFalse((parent / '.install.lock').exists())


if __name__ == "__main__":
    unittest.main()
