"""Guard the complete CI gate, independent triggers, and build-once boundary."""
from pathlib import Path
import unittest

import yaml
import images

ROOT = Path(__file__).resolve().parents[2]


def workflow(name):
    return yaml.load((ROOT / '.github/workflows' / name).read_text(), Loader=yaml.BaseLoader)


def ancestors(jobs, name):
    needs = jobs[name].get('needs', [])
    if isinstance(needs, str):
        needs = [needs]
    return set(needs).union(*(ancestors(jobs, parent) for parent in needs))


class WorkflowTest(unittest.TestCase):
    def test_publication_requires_all_checks_for_its_own_product(self):
        main = workflow('ci.yml')['jobs']
        yua = workflow('yuaction.yml')['jobs']
        self.assertEqual(ancestors(main, 'publish-image'), {'metadata', 'frontend', 'backend', 'security-scan', 'workflow-checks', 'build-verification'})
        self.assertEqual(ancestors(yua, 'publish-image'), {'metadata', 'frontend', 'backend', 'security-scan', 'workflow-checks', 'images', 'runtime'})
        for jobs in (main, yua):
            self.assertEqual(jobs['publish-image']['if'], "github.event_name != 'pull_request'")
            for name, job in jobs.items():
                if name != 'publish-image':
                    self.assertNotEqual(job.get('permissions', {}).get('packages'), 'write')
                    for step in job.get('steps', []):
                        self.assertNotEqual(step.get('with', {}).get('push'), 'true')

    def test_workflow_triggers_and_promotion_freshness_share_product_paths(self):
        for product, filename, key in [('main', 'ci.yml', 'paths-ignore'), ('yuaction', 'yuaction.yml', 'paths')]:
            expected = images.PRODUCTS[product]['exclude' if product == 'main' else 'include']
            triggers = workflow(filename)['on']
            self.assertEqual(triggers['push'][key], expected)
            self.assertEqual(triggers['pull_request'][key], expected)
            self.assertIn('workflow_dispatch', triggers)

    def test_publication_only_transfers_images_and_joins_all_platforms(self):
        publish = workflow('docker-build.yml')
        self.assertEqual(set(publish['on']), {'workflow_call'})
        self.assertEqual(publish['jobs']['prepare']['if'], "github.event_name != 'pull_request'")
        self.assertIn('push-platforms', ancestors(publish['jobs'], 'promote'))
        for job in publish['jobs'].values():
            for step in job.get('steps', []):
                self.assertFalse(step.get('uses', '').startswith('docker/build-push-action'))
                self.assertNotIn('docker build ', step.get('run', ''))
                self.assertNotIn('docker buildx build ', step.get('run', ''))

    def test_full_browser_and_database_race_checks_are_preserved(self):
        main = workflow('ci.yml')['jobs']
        yua = workflow('yuaction.yml')['jobs']
        frontend = '\n'.join(step.get('run', '') for step in main['frontend']['steps'])
        backend = '\n'.join(step.get('run', '') for step in main['backend']['steps'])
        self.assertIn('npm run verify:ci', frontend)
        self.assertIn('go test -v -race -coverprofile=coverage.txt ./...', backend)
        self.assertIn('go test -v -race -tags=event_worker', backend)
        self.assertEqual(main['backend']['services']['postgres']['env']['POSTGRES_DB'], 'dreamtrans_test')
        yuafront = '\n'.join(step.get('run', '') for step in yua['frontend']['steps'])
        self.assertIn('npm run test:e2e', yuafront)
        yuaback = '\n'.join(step.get('run', '') for step in yua['backend']['steps'])
        self.assertIn('go test -race ./...', yuaback)
        self.assertIn('unittest discover', yuaback)

    def test_runtime_checks_run_before_export_on_the_exact_loaded_image(self):
        steps = workflow('ci.yml')['jobs']['build-verification']['steps']
        names = [step.get('name', '') for step in steps]
        expected = [
            'Verify Edge runtime identity and credential boundary',
            'Verify Edge audio and durable return under a small memory limit',
            'Verify Edge admission state survives process restart',
            'Verify final image contents and runtime identity',
            'Verify file extraction in the production runtime',
            'Verify legacy appdata permission migration',
            'Verify restored deployment updates and recovery',
            'Verify Go lifecycle on existing production-shaped volumes',
            'Verify full backup restoration with production Docker tooling',
            'Verify stable companion routing after recreation',
            'Verify Compose migrations and readiness',
            'Verify standalone frontend runtime',
        ]
        for name in expected:
            self.assertLess(names.index(name), names.index('Export the tested image'))
        runtime = workflow('yuaction.yml')['jobs']['runtime']['steps']
        runtime_names = {step.get('name') for step in runtime}
        self.assertIn('Verify independent port, persistence and container recreation', runtime_names)
        self.assertIn('Verify YuAction live blue/green upgrade and rollback', runtime_names)


if __name__ == '__main__':
    unittest.main()
