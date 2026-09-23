"""Release-boundary regression tests: no registry writes before identity/gate checks."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import images


class ImagesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.sha = 'a' * 40
        self.env = patch.dict(os.environ, {
            'GITHUB_SHA': self.sha, 'GITHUB_REPOSITORY_OWNER': 'CoYumeLabs',
            'GITHUB_REPOSITORY': 'CoYumeLabs/DreamTrans', 'GITHUB_REF': 'refs/heads/main',
            'GITHUB_STEP_SUMMARY': str(self.directory / 'summary.md'),
        })
        self.env.start()
        self.addCleanup(self.env.stop)

    def platform_records(self, product):
        records = []
        for item in images.matrix(product, publish=True)['include']:
            name, arch = item['component'], item['arch']
            record = {'component': name, 'architecture': arch, 'commit': self.sha,
                      'ref': images.repository(name) + '@sha256:' + ('b' if arch == 'amd64' else 'c') * 64}
            (self.directory / f'digest-{name}-{arch}.json').write_text(json.dumps(record))
            records.append(record)
        return records

    def test_native_platforms_have_unique_matching_artifacts(self):
        for product in images.PRODUCTS:
            builds = images.matrix(product)['include']
            published = images.matrix(product, publish=True)['include']
            self.assertEqual(len(builds), len({(v['component'], v['arch']) for v in builds}))
            for item in published:
                self.assertIn(item, builds)
                self.assertEqual(item['runner'], 'ubuntu-24.04-arm' if item['arch'] == 'arm64' else 'ubuntu-24.04')

    def test_product_paths_cover_shared_code_without_coupling_unrelated_changes(self):
        cases = {
            'yuaction/frontend/src/App.tsx': (False, True),
            'backend/internal/handlers/speechmatics_proxy.go': (True, False),
            'frontend/src/unified/WorkspaceShell.tsx': (True, False),
            'frontend/src/core/transcription/SpeechmaticsProxyClient.ts': (True, True),
            'backend/internal/ops/yuaction.go': (True, True),
            'backend/internal/edgehttp/client.go': (True, True),
            'backend/internal/deployment/deployment.go': (True, True),
            'backend/pkg/deployment/deployment.go': (True, True),
            'backend/go.sum': (True, True),
            '.github/workflows/docker-build.yml': (True, True),
            '.github/workflows/yuaction.yml': (False, True),
        }
        for path, expected in cases.items():
            with self.subTest(path=path):
                self.assertEqual(tuple(images.affects_product(p, path) for p in ('main', 'yuaction')), expected)

    def test_archive_rejects_wrong_commit_or_corruption_before_docker_load(self):
        archive = self.directory / 'image.tar'
        archive.write_bytes(b'tested image fixture')
        original = {'component': 'main', 'architecture': 'amd64', 'commit': self.sha,
                    'archive_sha256': images.checksum(archive), 'image_id': 'sha256:' + 'd' * 64}
        for key, wrong in [('commit', 'e' * 40), ('component', 'edge'), ('architecture', 'arm64'), ('archive_sha256', '0' * 64)]:
            with self.subTest(field=key), patch('images.subprocess.run') as run:
                (self.directory / 'image.json').write_text(json.dumps(dict(original, **{key: wrong})))
                with self.assertRaises(ValueError):
                    images.load_image('main', 'amd64', self.directory)
                run.assert_not_called()

    def test_loaded_image_must_match_tested_identity(self):
        archive = self.directory / 'image.tar'
        archive.write_bytes(b'fixture')
        image_id = 'sha256:' + 'd' * 64
        (self.directory / 'image.json').write_text(json.dumps({'component': 'main', 'architecture': 'amd64',
            'commit': self.sha, 'archive_sha256': images.checksum(archive), 'image_id': image_id}))
        good = {'Id': image_id, 'Architecture': 'amd64', 'Config': {'Labels': {
            'org.opencontainers.image.revision': self.sha,
            'org.opencontainers.image.source': 'https://github.com/CoYumeLabs/DreamTrans'}}}
        with patch('images.subprocess.run'), patch('images.inspect', return_value=good):
            self.assertEqual(images.load_image('main', 'amd64', self.directory), image_id)
        with patch('images.subprocess.run'), patch('images.inspect', return_value=dict(good, Id='another image')):
            with self.assertRaisesRegex(ValueError, 'differs from tested'):
                images.load_image('main', 'amd64', self.directory)

    def test_release_rejects_missing_wrong_commit_and_duplicate_architectures(self):
        self.platform_records('main')
        path = self.directory / 'digest-edge-arm64.json'
        original = path.read_text()
        with patch('images.subprocess.run') as run:
            path.unlink()
            with self.assertRaisesRegex(ValueError, 'incomplete'):
                images.release('main', self.directory)
            record = json.loads(original)
            for key, value in [('commit', 'e' * 40), ('architecture', 'amd64'), ('ref', 'evil.test/image@sha256:' + 'f' * 64)]:
                path.write_text(json.dumps(dict(record, **{key: value})))
                with self.assertRaises(ValueError):
                    images.release('main', self.directory)
            run.assert_not_called()

    def test_complete_product_is_assembled_before_any_mutable_tag(self):
        for product in ('main', 'yuaction'):
            with self.subTest(product=product):
                for p in self.directory.glob('digest-*.json'):
                    p.unlink()
                self.platform_records(product)
                with patch('images.command', return_value='sha256:' + 'd' * 64), patch('images.current_product', return_value=True), patch('images.subprocess.run') as run:
                    images.release(product, self.directory)
                commands = [call.args[0] for call in run.call_args_list]
                self.assertEqual(len(commands[:2]), 2)
                self.assertTrue(all(not any(':latest' in arg for arg in cmd) for cmd in commands[:2]))
                self.assertTrue(any(':latest' in arg for cmd in commands[2:] for arg in cmd))
                self.assertFalse(any('build' in cmd for cmd in commands))
                result = json.loads((self.directory / 'release-images.json').read_text())
                self.assertEqual(result['product'], product)

    def test_stale_product_keeps_only_immutable_images(self):
        self.platform_records('main')
        with patch('images.command', return_value='sha256:' + 'd' * 64), patch('images.current_product', return_value=False), patch('images.subprocess.run') as run:
            images.release('main', self.directory)
        tags = [arg for call in run.call_args_list for arg in call.args[0]]
        self.assertFalse(any(tag.endswith((':latest', ':main')) for tag in tags))
        self.assertIn('ghcr.io/coyumelabs/dreamtrans:sha-' + self.sha, tags)

    def test_freshness_allows_other_product_changes_but_rejects_own_changes(self):
        for changed, expected in [('', True), ('yuaction/README.md\0', True),
                                  ('yuaction/README.md\0frontend/src/index.ts\0', False)]:
            with patch('images.command', side_effect=['b' * 40 + '\trefs/heads/main', changed]), patch('images.subprocess.run'):
                self.assertEqual(images.current_product('main'), expected)

    def test_prerelease_does_not_move_stable_minor_tag(self):
        self.assertEqual(images.version_tags('refs/tags/v1.2.3'), ['1.2.3', '1.2'])
        self.assertEqual(images.version_tags('refs/tags/v1.2.3-rc.1'), ['1.2.3-rc.1'])
        self.assertEqual(images.version_tags('refs/tags/v1.2.3+build.1'), ['1.2.3_build.1', '1.2'])
        self.assertEqual(images.version_tags('refs/heads/main'), [])


if __name__ == '__main__':
    unittest.main()
