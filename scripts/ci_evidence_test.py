import io
import json
import subprocess
import unittest
from unittest.mock import patch
import zipfile

from ci_evidence import fingerprints, receipt, select_evidence, SERVICES


class EvidenceTests(unittest.TestCase):
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

    def test_expired_or_missing_receipt_cannot_authorize_reuse(self):
        with patch('ci_evidence.api', return_value=b'{"artifacts": []}'):
            self.assertIsNone(receipt('owner/repo', {'id': 1}))
        data = io.BytesIO()
        with zipfile.ZipFile(data, 'w') as archive:
            archive.writestr('verification.json', json.dumps({'version': 1}))
        artifacts = {'artifacts': [{'id': 2, 'name': 'backend-verification-1', 'expired': False}]}
        with patch('ci_evidence.api', side_effect=[json.dumps(artifacts).encode(), data.getvalue()]):
            self.assertEqual(receipt('owner/repo', {'id': 1}), {'version': 1})


if __name__ == '__main__':
    unittest.main()
