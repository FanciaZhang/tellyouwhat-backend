import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from stage_test_upgrade import candidate_environment, check_empty, Stage, OperationError


class UpgradeTests(unittest.TestCase):
    def test_candidate_has_separate_endpoint_network_and_exact_revision(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'before.env'
            text = ('ALBUM_TEST_IMAGE_TAG=' + 'a' * 40 + '\nALBUM_TEST_IMAGE_PREFIX=tellyouwhat-albums-test\n'
                    'ALBUM_TEST_API_PORT=18090\nALBUM_TEST_ENV_FILE=/opt/tellyouwhat/albums-test/.env.albums-test\n'
                    'ALBUM_TEST_SECRET_DIRECTORY=/opt/tellyouwhat/albums-test/secrets\nALBUM_TEST_MYSQL_PASSWORD=private-fixture\n')
            path.write_text(text)
            updated = candidate_environment(path, 'b' * 40)
            self.assertIn('ALBUM_TEST_API_PORT=18091\n', updated)
            self.assertIn('ALBUM_TEST_NETWORK_CIDR=172.29.91.0/24\n', updated)
            self.assertIn('ALBUM_TEST_MYSQL_PASSWORD=private-fixture\n', updated)
            self.assertEqual(path.read_text(), text)
            for changed in (text.replace('18090', '8080'), text.replace('albums-test/secrets', 'health/secrets')):
                path.write_text(changed)
                with self.assertRaises(OperationError): candidate_environment(path, 'b' * 40)
            path.write_text(text)
            with self.assertRaises(OperationError): candidate_environment(path, 'latest')

    def test_existing_user_data_refuses_cloning(self):
        check_empty(dict(accounts=0, uploads=0))
        for value in (dict(accounts=1, uploads=0), dict(accounts=0, uploads=1)):
            with self.assertRaises(OperationError): check_empty(value)

    def test_failed_snapshot_resumes_untouched_active_stack(self):
        stage = Stage.__new__(Stage)
        for fail in ('state', 'dump', 'drained', 'drain'):
            with tempfile.TemporaryDirectory() as folder, self.subTest(fail=fail), patch.object(stage, 'compose', side_effect=lambda new, *a: list(a)), \
                    patch.object(stage, 'state', return_value=dict(accounts=0, uploads=0)) as state, \
                    patch.object(stage, 'dump', side_effect=OperationError('dump failure')), \
                    patch('stage_test_upgrade.ready') as health:
                if fail == 'state': state.side_effect = OperationError('state failed')
                stage.folder = Path(folder)
                calls = []
                def execute(label, command, **kwargs):
                    calls.append(label)
                    if label == 'drain' and fail == 'drain': raise OperationError('partial stop')
                    if label == 'drained':
                        return json.dumps(dict(Running=False, OOMKilled=False, ExitCode=137 if fail == 'drained' else 0)).encode()
                    return b''
                with patch.object(stage, 'execute', side_effect=execute):
                    with self.assertRaises(OperationError): stage.snapshot()
                self.assertEqual(calls[-1], 'resume-active')
                self.assertEqual(calls.count('resume-active'), 1)
                health.assert_called_once_with(18090)


if __name__ == '__main__': unittest.main()
