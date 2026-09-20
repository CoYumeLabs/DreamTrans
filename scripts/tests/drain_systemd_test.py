#!/usr/bin/env python3
"""Real systemd scheduling; a private fake Docker engine cannot touch services."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid


ENGINE = '''#!/usr/bin/python3
import json, sys
from pathlib import Path
root = Path(__file__).resolve().parent.parent
args = sys.argv[1:]
state = json.loads((root / '.bluegreen/state.json').read_text())
with (root / 'engine.log').open('a') as log:
    log.write(' '.join(args) + '\\n')
if args[:2] == ['volume', 'inspect']:
    print(json.dumps([{'Name': args[2], 'Driver': 'local', 'Options': {}}]))
elif args[:2] == ['container', 'inspect']:
    print(json.dumps([{'Id': args[2], 'State': {'Running': not (args[2].endswith('-green') and (root / 'stopped').exists())},
        'Mounts': [{'Type': 'volume', 'Name': 'production-pg', 'RW': True, 'Destination': '/var/lib/postgresql/data'}]}]))
elif args[0] == 'exec' and args[2] == 'wget':
    print('blue' if args[-1].endswith('/_release') else 'ready')
elif args[0] == 'exec' and args[2:4] == ['/app/server', 'deploy-control']:
    if args[-1] == 'handoff': (root / 'offered').touch()
    print(json.dumps({'protocol': 1, 'mode': 'draining', 'handoff_supported': True,
        'websockets': 0 if (root / 'complete').exists() else 1, 'tasks': 0,
        'drained': (root / 'complete').exists()}))
elif args[:3] == ['stop', '--timeout', '-1']:
    assert args[3] == state['prefix'] + '-green' and (root / 'complete').exists()
    (root / 'stopped').touch()
else:
    raise SystemExit('Unexpected engine operation')
'''


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def wait_for(check, description):
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.5)
    raise AssertionError(description)


def main():
    assert os.geteuid() == 0, 'Run this isolated systemd test as root'
    assert Path('/run/systemd/system').is_dir(), 'A running systemd host is required'
    binary = os.environ['DREAMTRANS_CTL']
    for role in ('main', 'edge'):
        with tempfile.TemporaryDirectory(prefix='dt-drain-systemd-') as directory:
            root = Path(directory)
            prefix = 'fixture-' + uuid.uuid4().hex[:12]
            unit = 'dreamtrans-drain-' + prefix
            units = Path('/etc/systemd/system')
            dropin = units / (unit + '.service.d')
            dropin.mkdir(mode=0o755)
            (dropin / 'fixture.conf').write_text('[Service]\nEnvironment="PATH=' + str(root / 'bin') + ':/usr/bin:/bin"\n')
            (root / 'bin').mkdir()
            engine = root / 'bin/docker'
            engine.write_text(ENGINE)
            engine.chmod(0o700)
            shutil.copyfile(binary, root / 'dreamtransctl')
            (root / 'dreamtransctl').chmod(0o700)
            (root / '.bluegreen').mkdir(mode=0o700)
            (root / 'config').mkdir()
            (root / 'config/edge.json').write_text('{}')
            (root / '.env').write_text('retained-configuration\n')
            state = {'format': 1, 'role': role, 'prefix': prefix, 'active': 'blue', 'previous': 'green',
                     'phase': 'observing', 'database_id': 'db-id', 'database_volume': 'production-pg',
                     'application_volume': 'production-app', 'colors': {'blue': {}, 'green': {}},
                     'drain_started_at': '2020-01-01T00:00:00Z'}
            state_file = root / '.bluegreen/state.json'
            state_file.write_text(json.dumps(state))
            command = [str(root / 'dreamtransctl'), *(['edge'] if role == 'edge' else []), '--dir', directory]
            try:
                run(*command, 'configure-drain', '--handoff-after', '600')
                run(*command, 'configure-drain', '--handoff-after', '0')
                assert run('systemctl', 'is-enabled', unit + '.timer') == 'enabled'
                assert json.loads(state_file.read_text()) == state
                state['phase'] = 'draining'
                state_file.write_text(json.dumps(state))
                wait_for(lambda: (root / 'offered').exists(), 'real timer did not request handoff')
                assert not (root / 'stopped').exists(), 'timer killed an active connection'
                # Recreate timer scheduling; no foreground process owns drain progress.
                run('systemctl', 'restart', unit + '.timer')
                (root / 'complete').touch()
                wait_for(lambda: json.loads(state_file.read_text())['phase'] == 'ready', 'timer did not retire drained version')
                assert (root / 'stopped').exists()
                assert (root / '.env').read_text() == 'retained-configuration\n'
                assert json.loads(state_file.read_text())['application_volume'] == 'production-app'
                print(f'{role}: real timer, repeated configuration, restart, protected live connection and automatic retirement passed', flush=True)
            finally:
                subprocess.run(['systemctl', 'disable', '--now', unit + '.timer'], capture_output=True)
                subprocess.run(['systemctl', 'stop', unit + '.service'], capture_output=True)
                for name in (unit + '.timer', unit + '.service'):
                    (units / name).unlink(missing_ok=True)
                (dropin / 'fixture.conf').unlink()
                dropin.rmdir()
                run('systemctl', 'daemon-reload')
                subprocess.run(['systemctl', 'reset-failed', unit + '.service'], capture_output=True)


if __name__ == '__main__':
    main()
