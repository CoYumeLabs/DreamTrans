#!/usr/bin/env python3
"""Failure recovery tests use an isolated controller directory and mocked engine."""
import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import tarfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import release


class ReleaseRecoveryTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.controller = release.Controller(self.directory.name)
        self.controller.state = {
            'role': 'edge', 'prefix': 'isolated-test', 'network': 'entry-test',
            'phase': 'draining', 'active': 'green', 'previous': 'blue',
            'colors': {'blue': {'image': 'old', 'empty_spool': False},
                       'green': {'image': 'new'}},
        }

    def test_configuration_snapshot_restores_companion_settings_and_linked_secrets(self):
        root = Path(self.directory.name)
        settings = {
            '.env': 'main secrets', 'compose.restore.yml': 'restore image',
            'compose.production.yml': 'external production volume',
            'compose.extra.yaml': 'yaml override', 'release.py': 'controller',
            'backup.sh': 'backup helper', 'yuaction/compose.ghcr.yml': 'companion image',
            'yuaction/compose.bluegreen.yml': 'stable internal network',
            'yuaction/.env.production': 'companion settings',
        }
        for name, data in settings.items():
            path = root/name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(data)
        secret = root/'operator-secret'
        secret.write_text('linked companion secret')
        (root/'yuaction/.env').symlink_to(secret)
        (root/'yuaction/node_modules').mkdir()
        (root/'yuaction/node_modules/ignored').write_text('cache')
        self.controller.persist()
        (self.controller.path/'lock').touch()
        (self.controller.path/'migration').mkdir()
        (self.controller.path/'migration/ignored.sql').write_text('derived schema')
        destination = root/'configuration.tar'
        release.archive_configuration(root, self.controller.path, destination)
        with tarfile.open(destination) as archive:
            for name, value in settings.items():
                self.assertEqual(archive.extractfile(name).read().decode(), value)
            self.assertTrue(archive.getmember('yuaction/.env').isfile())
            self.assertEqual(archive.extractfile('yuaction/.env').read(), b'linked companion secret')
            self.assertIn('.bluegreen/state.json', archive.getnames())
            for excluded in ('operator-secret', 'yuaction/node_modules/ignored', '.bluegreen/lock', '.bluegreen/migration/ignored.sql'):
                self.assertNotIn(excluded, archive.getnames())

    def test_incomplete_conversion_cannot_publish_a_partial_snapshot(self):
        c = self.controller
        c.state.update(active=None, colors={}, phase='importing')
        target = Path(self.directory.name)/'backup.tar'
        with patch.object(c, 'assert_database'), patch.object(release, 'docker') as engine:
            with self.assertRaisesRegex(release.ReleaseError, 'initial conversion is incomplete'):
                c.snapshot(target)
        engine.assert_not_called()
        self.assertFalse(target.exists())

    def test_protocol_rollback_cannot_strand_authorized_sessions(self):
        legacy = {'protocol': 1, 'state_epoch': 1, 'expand_migrations': [], 'edge_protocol_min': 1, 'edge_protocol_max': 1}
        current = dict(legacy, edge_protocol_max=2)
        release.check_contract(current, legacy)
        release.check_contract(current, current)
        with self.assertRaisesRegex(release.ReleaseError, 'already authorized'):
            release.check_contract(legacy, current)

    def test_abort_records_empty_spool_and_does_not_offer_failed_slot_for_rollback(self):
        c=self.controller
        c.state.update(phase='candidate',target='blue')
        with patch.object(release,'inspect',return_value={'State':{'Running':True}}), patch.object(c,'control',return_value={'drained':True}), patch.object(release,'docker') as engine:
            c.abort()
        self.assertTrue(c.state['colors']['blue']['empty_spool'])
        self.assertEqual(c.state['active'],'green')
        self.assertIsNone(c.state['previous'])
        self.assertEqual(c.state['phase'],'ready')
        engine.assert_called_once_with('stop','--timeout','-1','isolated-test-blue')

    def test_crashed_main_candidate_can_abort_without_calling_a_dead_control_socket(self):
        c=self.controller
        c.state.update(role='main',phase='candidate',target='blue')
        with patch.object(release,'inspect',return_value={'State':{'Running':False}}), patch.object(c,'control') as control, patch.object(release,'docker'):
            c.abort()
        control.assert_not_called()
        self.assertEqual(c.state['phase'],'ready')
        self.assertEqual(c.state['active'],'green')

    def test_abort_after_cutover_is_rejected(self):
        with self.assertRaises(release.ReleaseError):self.controller.abort()

    def test_timeout_keeps_running_websocket_and_unsent_results(self):
        c = self.controller
        status = {'drained': False, 'websockets': 1, 'requests': 0, 'tasks': 0, 'pending': 2}
        with patch.object(c, 'control', return_value=status), patch.object(release, 'inspect', return_value={'State': {'Running': True}}), patch.object(release, 'docker') as engine:
            c.drain(0)
        engine.assert_not_called()
        self.assertEqual(json.loads(c.state_file.read_text())['phase'], 'draining')
        self.assertFalse(c.state['colors']['blue']['empty_spool'])

    def test_stopped_unconfirmed_spool_is_never_declared_drained(self):
        c = self.controller
        with patch.object(release, 'inspect', return_value={'State': {'Running': False}}), patch.object(release, 'docker') as engine:
            with self.assertRaises(release.ReleaseError):
                c.drain(0)
        engine.assert_not_called()
        self.assertFalse(c.state['colors']['blue']['empty_spool'])

    def test_confirmed_drain_is_persisted_before_stop(self):
        c = self.controller
        status = {'drained': True, 'websockets': 0, 'requests': 0, 'tasks': 0, 'pending': 0}
        def stop(*args):
            self.assertEqual(args, ('stop', '--timeout', '-1', 'isolated-test-blue'))
            self.assertTrue(json.loads(c.state_file.read_text())['colors']['blue']['empty_spool'])
            raise release.ReleaseError('controller interrupted after recording drain')
        with patch.object(c, 'control', return_value=status), patch.object(release, 'inspect', return_value={'State': {'Running': True}}), patch.object(release, 'docker', side_effect=stop):
            with self.assertRaises(release.ReleaseError):
                c.drain(0)
        self.assertTrue(c.state['colors']['blue']['empty_spool'])

    def test_initial_recovery_reuses_exact_spool_without_removal(self):
        c = self.controller
        c.state['colors'] = {}
        current = {'Image': 'pinned', 'State': {'Running': True},
                   'Mounts': [{'Destination': '/spool', 'Source': str(c.path/'blue'/'spool')}],
                   'NetworkSettings': {'Networks': {'entry-test': {}}}}
        with patch.object(c, 'container_exists', return_value=True), patch.object(c, 'wait_ready') as ready, patch.object(release, 'inspect', return_value=current), patch.object(release, 'docker') as engine:
            c.ensure_initial_color('pinned', {'protocol': 1})
        ready.assert_called_once_with('blue')
        engine.assert_not_called()
        self.assertEqual(c.state['colors']['blue']['image'], 'pinned')

    def test_initial_recovery_rejects_an_unrelated_spool(self):
        c = self.controller
        current = {'Image': 'pinned', 'Mounts': [{'Destination': '/spool', 'Source': '/unrelated'}]}
        with patch.object(c, 'container_exists', return_value=True), patch.object(release, 'inspect', return_value=current), patch.object(release, 'docker') as engine:
            with self.assertRaises(release.ReleaseError):
                c.ensure_initial_color('pinned', {})
        engine.assert_not_called()

    def test_nonterminal_progress_is_readable_without_control_codes(self):
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            release.progress('2/8', '内存检查通过')
        self.assertIn('[####------------]', output.getvalue())
        self.assertNotIn('\033', output.getvalue())

    def test_small_edge_can_admit_candidate_but_low_memory_never_stops_active(self):
        root = Path(__file__).resolve().parents[2]
        edge = json.loads((root/'deploy/edge-release.json').read_text())
        main = json.loads((root/'deploy/release.json').read_text())
        with patch.object(Path, 'read_text', return_value='MemAvailable: 524288 kB\n'), patch.object(release, 'docker') as engine:
            release.check_memory(edge)
            with self.assertRaises(release.ReleaseError):
                release.check_memory(main)
        engine.assert_not_called()
        with patch.object(Path, 'read_text', return_value='MemAvailable: 262144 kB\n'), patch.object(release, 'docker') as engine:
            with self.assertRaises(release.ReleaseError):
                release.check_memory(edge)
        engine.assert_not_called()


if __name__ == '__main__':
    unittest.main()
