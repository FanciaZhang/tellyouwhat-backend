import io
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch
import zipfile

from ci_evidence import fingerprints, receipt, select_evidence, SERVICES, main


class EvidenceTests(unittest.TestCase):
    def test_dirty_verified_workspace_cannot_issue_a_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'receipt.json'
            with patch.object(sys, 'argv', ['ci_evidence.py', 'record', str(output)]), patch(
                    'ci_evidence.git', side_effect=[b'a' * 40, b' M changed.go']), self.assertRaises(SystemExit):
                main()
            self.assertFalse(output.exists())

    def test_changed_tree_component_reuse_requires_a_main_source(self):
        current = dict(full='new', albums='same', swift='same')
        previous = dict(full='old', albums='same', swift='same')
        run = dict(id=10, head_sha='b' * 40, status='completed', event='pull_request', head_branch='feature/x', head_repository={'full_name': 'owner/repo'})
        record = dict(version=1, commit=run['head_sha'], image='runner', keys=previous)
        jobs = json.dumps({'jobs': [{'name': 'verify', 'conclusion': 'success'}]}).encode()
        for event, branch, reused in [('pull_request', 'feature/x', ''), ('push', 'main', '10')]:
            with self.subTest(event=event), patch.dict(run, event=event, head_branch=branch), patch(
                    'ci_evidence.fingerprints', side_effect=[current, previous]), patch('ci_evidence.subprocess.run'), patch(
                    'ci_evidence.api', return_value=jobs), patch('ci_evidence.receipt', return_value=record):
                self.assertEqual(select_evidence('owner/repo', 'a' * 40, 'runner', [run], 20)['albums'], reused)

    def test_operations_only_changes_reuse_albums_but_code_and_toolchain_changes_do_not(self):
        def inventory(ops=b'a', code=b'a'):
            return b'100644 blob ' + code + b'\tinternal/albums/store.go\0' + b'100644 blob ' + ops + b'\tdeploy/tencent/ops_health.py\0'
        with patch('ci_evidence.git', return_value=inventory()):
            original = fingerprints('source', 'runner-1')
        with patch('ci_evidence.git', return_value=inventory(ops=b'b')):
            ops = fingerprints('source', 'runner-1')
        self.assertEqual(original['albums'], ops['albums'])
        self.assertNotEqual(original['full'], ops['full'])
        for records, image in [(inventory(code=b'b'), 'runner-1'), (inventory(), 'runner-2')]:
            with patch('ci_evidence.git', return_value=records):
                self.assertNotEqual(original['albums'], fingerprints('source', image)['albums'])
        with patch('ci_evidence.git', return_value=inventory().replace(b'ops_health.py', b'ops_backup.py')):
            self.assertNotEqual(original['albums'], fingerprints('source', 'runner-1')['albums'])

    def test_successful_ancestor_receipt_is_required_and_failed_deployment_does_not_repeat_tests(self):
        commit = 'a' * 40
        keys = dict(full='full', albums='albums', swift='swift')
        run = dict(id=10, head_sha=commit, status='completed', conclusion='failure', event='push', head_branch='main', head_repository={'full_name': 'owner/repo'})
        record = dict(version=1, commit=commit, image='runner', keys=keys)
        jobs = [{'name': 'verify', 'conclusion': 'success'}] + [{'name': f'container ({name})', 'conclusion': 'success'} for name in SERVICES]
        with patch('ci_evidence.fingerprints', return_value=keys), patch('ci_evidence.subprocess.run'), patch(
                'ci_evidence.receipt', return_value=record), patch('ci_evidence.api', return_value=json.dumps({'jobs': jobs}).encode()):
            self.assertEqual(select_evidence('owner/repo', commit, 'runner', [run], 20), dict(full='10', albums='10', swift='10', images='10'))
            for field, value in [('commit', 'b' * 40), ('image', 'other'), ('keys', {})]:
                with self.subTest(field=field), patch('ci_evidence.receipt', return_value={**record, field: value}):
                    self.assertFalse(any(select_evidence('owner/repo', commit, 'runner', [run], 20).values()))
            with patch('ci_evidence.api', return_value=json.dumps({'jobs': [{'name': 'verify', 'conclusion': 'failure'}]}).encode()):
                self.assertFalse(any(select_evidence('owner/repo', commit, 'runner', [run], 20).values()))
            with patch('ci_evidence.subprocess.run', side_effect=subprocess.CalledProcessError(1, 'git')):
                self.assertFalse(any(select_evidence('owner/repo', commit, 'runner', [run], 20).values()))
            with patch.dict(run['head_repository'], full_name='untrusted/fork'):
                self.assertFalse(any(select_evidence('owner/repo', commit, 'runner', [run], 20).values()))
            with patch.dict(run, event='pull_request', head_branch='feature/example'):
                with patch.dict(os.environ, GITHUB_REF='refs/heads/main'):
                    self.assertEqual(select_evidence('owner/repo', commit, 'runner', [run], 20)['images'], '')
                with patch.dict(os.environ, GITHUB_REF='refs/heads/feature/example'):
                    self.assertEqual(select_evidence('owner/repo', commit, 'runner', [run], 20)['images'], '10')
            with patch.dict(run, event='workflow_dispatch', head_branch='main'), patch.dict(os.environ, GITHUB_REF='refs/heads/main'):
                self.assertEqual(select_evidence('owner/repo', commit, 'runner', [run], 20)['images'], '')

    def test_expired_or_missing_receipt_cannot_authorize_reuse(self):
        with patch('ci_evidence.api', return_value=b'{"artifacts": []}'):
            self.assertIsNone(receipt('owner/repo', {'id': 1}))
        data = io.BytesIO()
        with zipfile.ZipFile(data, 'w') as archive:
            archive.writestr('verification.json', json.dumps({'version': 1}))
        artifacts = {'artifacts': [{'id': 2, 'name': 'backend-verification-1', 'expired': False}]}
        with patch('ci_evidence.api', side_effect=[json.dumps(artifacts).encode(), data.getvalue()]):
            self.assertEqual(receipt('owner/repo', {'id': 1}), {'version': 1})

    def test_newer_failed_test_cannot_be_hidden_by_older_success(self):
        commit = 'a' * 40
        keys = dict(full='full', albums='albums', swift='swift')
        run = dict(head_sha=commit, status='completed', event='pull_request', head_repository={'full_name': 'owner/repo'})
        failed = {'jobs': [{'name': 'verify', 'conclusion': 'failure', 'steps': [{'name': 'Test Albums with real storage', 'conclusion': 'failure'}]}]}
        passed = {'jobs': [{'name': 'verify', 'conclusion': 'success'}]}
        record = dict(version=1, commit=commit, image='runner', keys=keys)
        with patch('ci_evidence.fingerprints', return_value=keys), patch('ci_evidence.subprocess.run'), patch(
                'ci_evidence.receipt', return_value=record), patch('ci_evidence.api', side_effect=[json.dumps(failed).encode(), json.dumps(passed).encode()]):
            result = select_evidence('owner/repo', commit, 'runner', [{**run, 'id': 11}, {**run, 'id': 10}], 20)
        self.assertEqual(result, dict(full='', albums='', swift='10', images=''))

    def test_failed_matching_pr_tests_also_invalidate_older_main_component_evidence(self):
        current = dict(full='current', albums='same', swift='same')
        pr_keys = {**current, 'full': 'pr'}
        main_keys = {**current, 'full': 'main'}
        common = dict(status='completed', head_repository={'full_name': 'owner/repo'})
        runs = [{**common, 'id': 11, 'head_sha': 'b' * 40, 'event': 'pull_request', 'conclusion': 'failure'},
                {**common, 'id': 10, 'head_sha': 'c' * 40, 'event': 'push', 'head_branch': 'main'}]
        failed = {'jobs': [{'name': 'verify', 'conclusion': 'failure', 'steps': [{'name': 'Test Albums with real storage', 'conclusion': 'failure'}]}]}
        passed = {'jobs': [{'name': 'verify', 'conclusion': 'success'}]}
        record = dict(version=1, commit='c' * 40, image='runner', keys=main_keys)
        with patch('ci_evidence.fingerprints', side_effect=[current, pr_keys, main_keys]), patch('ci_evidence.subprocess.run'), patch(
                'ci_evidence.receipt', return_value=record), patch('ci_evidence.api', side_effect=[json.dumps(failed).encode(), json.dumps(passed).encode()]):
            result = select_evidence('owner/repo', 'a' * 40, 'runner', runs, 20)
        self.assertEqual(result['albums'], '')
        self.assertEqual(result['swift'], '10')


if __name__ == '__main__':
    unittest.main()
