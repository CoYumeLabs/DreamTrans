#!/usr/bin/env python3
"""Run the actual bootstrap against disposable OS/engine/package-manager fixtures."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class EdgeBootstrapTest(unittest.TestCase):
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
