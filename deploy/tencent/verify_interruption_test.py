import unittest
from unittest.mock import patch
from pathlib import Path

import blue_green as bg
from ops_common import OperationError
from verify_interruption import validate


class ManualEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.sha = 'b' * 40
        self.repo = 'owner/backend'
        self.run = dict(path='.github/workflows/backend.yml', head_sha=self.sha, head_branch='main',
                        event='push', status='completed', conclusion='failure',
                        repository={'full_name': self.repo}, head_repository={'full_name': self.repo})
        self.jobs = [dict(name=name, conclusion='success', head_sha=self.sha) for name in
                     ['verify', *['container (' + s + ')' for s in ['gateway','worker','admin','adminctl','migrate','maintenance']]]]
        self.jobs.append(dict(name='deploy / deploy', conclusion='failure', head_sha=self.sha,
                             steps=[dict(name='Deploy and verify internal services', conclusion='failure')]))

    def check(self):
        validate(self.run, self.jobs, self.repo, self.sha, 'interrupt:' + self.sha)

    def test_exact_failed_production_run_is_eligible_for_host_refusal_check(self):
        self.check()

    def test_fork_pr_unverified_old_sha_wrong_workflow_and_missing_images_are_rejected(self):
        for field, value in [('event','pull_request'), ('head_branch','feature/x'), ('head_sha','c'*40),
                             ('path','.github/workflows/other.yml'), ('conclusion','success'),
                             ('head_repository',{'full_name':'fork/backend'})]:
            with self.subTest(field=field):
                original = self.run[field]
                self.run[field] = value
                with self.assertRaises(ValueError): self.check()
                self.run[field] = original
        for index in range(7):
            self.jobs[index]['conclusion'] = 'skipped'
            with self.assertRaises(ValueError): self.check()
            self.jobs[index]['conclusion'] = 'success'
        self.jobs[-1]['steps'][0]['name'] = 'Back up database before migration'
        with self.assertRaises(ValueError): self.check()

    def test_confirmation_cannot_be_generic_yes_or_different_sha(self):
        for confirmation in ['yes', '', 'interrupt:' + 'c'*40]:
            with self.assertRaises(ValueError):
                validate(self.run, self.jobs, self.repo, self.sha, confirmation)


class CapacityTests(unittest.TestCase):
    def test_memory_headroom_is_required_without_counting_swap(self):
        with patch.object(Path, 'read_text', return_value='MemTotal: 3812352 kB\nMemAvailable: 500000 kB\nSwapFree: 9000000 kB\n'):
            with self.assertRaises(bg.CapacityError): bg.require_memory_capacity()
        with patch.object(Path, 'read_text', return_value='MemTotal: 3812352 kB\nMemAvailable: 2900000 kB\n'):
            bg.require_memory_capacity()

    def test_cpu_pressure_offers_interruption_but_sampling_errors_do_not(self):
        with patch.object(bg, 'require_memory_capacity'), patch.object(bg, 'cpu_busy_percent', return_value=90):
            with self.assertRaises(bg.CapacityError): bg.resource_check(None, 'green')
        with patch.object(bg, 'require_memory_capacity'), patch.object(bg, 'cpu_busy_percent', side_effect=OperationError('missing sample')):
            with self.assertRaises(OperationError) as caught: bg.resource_check(None, 'green')
            self.assertNotIsInstance(caught.exception, bg.CapacityError)

    def test_cpu_sample_excludes_guest_duplicate_ticks_and_counts_iowait_as_idle(self):
        samples = ['cpu 100 0 100 800 0 0 0 0 100 0\n', 'cpu 200 0 200 800 0 0 0 0 200 0\n']
        with patch.object(Path, 'read_text', side_effect=samples), patch.object(bg.time, 'sleep'):
            self.assertEqual(bg.cpu_busy_percent(), 100)


if __name__ == '__main__':
    unittest.main()
