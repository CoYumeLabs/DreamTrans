#!/usr/bin/env python3
"""Exercise SIGTERM/restart against the real Edge entrypoint, without providers."""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid


def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()


def control(name):
    for _ in range(60):
        result = subprocess.run(
            ['docker', 'exec', name, '/app/server', 'deploy-control', 'status'],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            return json.loads(result.stdout)
        time.sleep(0.25)
    raise RuntimeError('Edge control socket did not become ready')


def main():
    if len(sys.argv) != 2:
        raise SystemExit('Usage: edge_restart_test.py EDGE_IMAGE')
    name = 'dreamtrans-edge-restart-' + uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix='dreamtrans-edge-restart-') as directory:
        root = Path(directory)
        # Permit the non-root image user to traverse the bind mount parent.
        root.chmod(0o755)
        for child in ['deployment', 'spool']:
            path = root / child
            path.mkdir(mode=0o755)
            # CI may run as an unprivileged Docker client. Initialize only these
            # newly-created fixture directories through the test image.
        config = {
            'node_id': str(uuid.uuid4()), 'identity': 'test-only-node-identity',
            'public_key': base64.b64encode(bytes(32)).decode().rstrip('='),
            'main_url': 'https://127.0.0.1:9',
            'origins': ['https://main.example.test'],
            'provider_key': 'test-only-unused-provider-key', 'maximum': 1,
        }
        (root / 'edge.json').write_text(json.dumps(config))
        (root / 'edge.json').chmod(0o644)
        docker('run', '--rm', '--user', '0', '--entrypoint', '/bin/sh',
               '--mount', f'type=bind,src={root},dst=/fixture', sys.argv[1],
               '-c', 'chown 10001:10001 /fixture/deployment /fixture/spool')
        try:
            docker('run', '-d', '--name', name, '--memory=256m', '--memory-swap=256m',
                   '--cpus=1', '--pids-limit=128', '--no-healthcheck',
                   '-e', 'DREAMTRANS_DEPLOYMENT_MODE=active',
                   '-e', 'DREAMTRANS_DEPLOYMENT_STATE=/deployment/mode',
                   '-e', 'EDGE_CONFIG=/config/edge.json',
                   '--mount', f'type=bind,src={root}/edge.json,dst=/config/edge.json,readonly',
                   '--mount', f'type=bind,src={root}/deployment,dst=/deployment',
                   '--mount', f'type=bind,src={root}/spool,dst=/spool', sys.argv[1])
            assert control(name)['mode'] == 'active'
            for mode in ['active', 'standby', 'draining']:
                docker('exec', name, '/app/server', 'deploy-control', mode)
                docker('stop', '--timeout', '20', name)
                state = json.loads(docker('inspect', name))[0]['State']
                assert state['ExitCode'] == 0 and not state['OOMKilled'], state
                # Read with the image's uid because the persisted file is 0600.
                persisted = docker('run', '--rm', '--entrypoint', '/bin/cat',
                                   '--mount', f'type=bind,src={root}/deployment,dst=/deployment,readonly',
                                   sys.argv[1], '/deployment/mode')
                assert persisted == mode, (persisted, mode)
                docker('start', name)
                assert control(name)['mode'] == mode
                print(f'PASS: Edge SIGTERM/restart preserves {mode}', flush=True)
        finally:
            subprocess.run(['docker', 'rm', '-f', '-v', name], capture_output=True, check=False)
            # Remove fixture files using their container uid; preserve the user's
            # temporary root directory for TemporaryDirectory to clean up.
            docker('run', '--rm', '--user', '0', '--entrypoint', '/bin/sh',
                   '--mount', f'type=bind,src={root},dst=/fixture', sys.argv[1],
                   '-c', 'chown -R ' + str(os.getuid()) + ':' + str(os.getgid()) + ' /fixture/deployment /fixture/spool')


if __name__ == '__main__':
    main()
