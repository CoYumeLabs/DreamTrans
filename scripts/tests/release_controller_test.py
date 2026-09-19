#!/usr/bin/env python3
"""Failure recovery tests use an isolated controller directory and mocked engine."""
import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
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
