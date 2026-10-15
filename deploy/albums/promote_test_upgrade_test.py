import hashlib
import hmac
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from promote_test_upgrade import promote, replacement_route, verify_snapshot, Stage, OperationError


class PromotionTests(unittest.TestCase):
    def test_route_change_is_exact_and_preserves_other_directives(self):
        value = 'api.albums.tellyouwhat.cn {\n header X-Test yes\n reverse_proxy 127.0.0.1:18090\n}\n'
        self.assertEqual(replacement_route(value), value.replace('18090', '18091'))
        for changed in (value.replace('albums', 'health'), value.replace('18090', '18091'), value + value):
            with self.assertRaises(OperationError): replacement_route(changed)

    def test_tampered_ciphertext_or_plaintext_manifest_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            data = b'opaque encrypted fixture'
            key = b'a' * 32
            (folder / 'database.sql.enc').write_bytes(data)
            (folder / 'snapshot.key').write_text(key.hex())
            manifest = dict(ciphertext_sha256=hashlib.sha256(data).hexdigest(),
                            hmac_sha256=hmac.new(key, data, hashlib.sha256).hexdigest(),
                            plaintext_sha256=hashlib.sha256(b'actual SQL').hexdigest())
            (folder / 'snapshot.json').write_text(json.dumps(manifest))
            with patch('promote_test_upgrade.subprocess.run') as decrypt:
                decrypt.return_value.returncode = 0
                decrypt.return_value.stdout = b'actual SQL'
                self.assertEqual(verify_snapshot(folder), manifest)
                manifest['plaintext_sha256'] = hashlib.sha256(b'other SQL').hexdigest()
                (folder / 'snapshot.json').write_text(json.dumps(manifest))
                with self.assertRaisesRegex(OperationError, 'plaintext'): verify_snapshot(folder)
                (folder / 'database.sql.enc').write_bytes(b'changed')
                with self.assertRaisesRegex(OperationError, 'ciphertext'): verify_snapshot(folder)

    def test_failure_before_reload_recovers_old_service_but_unknown_reload_does_not(self):
        for failure in ('drain-public', 'stale-data', 'validate-caddy', 'reload-caddy'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                folder = Path(directory)
                stage = Stage.__new__(Stage)
                stage.folder, stage.selection = folder, folder
                stage.source = 'a' * 40
                stage.active_env, stage.new_env = folder / 'compose.env', folder / 'candidate.env'
                stage.active_env.write_text('old')
                stage.new_env.write_text('new')
                (folder / 'active-compose.env').write_text('old')
                (folder / '.env.albums-test').write_text('runtime')
                (folder / 'active-runtime.env').write_text('runtime')
                route = folder / 'route'
                route.write_text('api.albums.tellyouwhat.cn {\n reverse_proxy 127.0.0.1:18090\n}\n')
                before = dict(accounts=0, uploads=0, migrations=40)
                after = dict(accounts=0, uploads=0, migrations=62)
                (folder / 'evidence.json').write_text(json.dumps(dict(source=stage.source, public_route_changed=False,
                    before=before, after=after, recovery=dict(signing_sha256={}), candidate_services={})))
                calls = []
                def execute(label, *args, **kwargs):
                    calls.append(label)
                    if label == failure: raise OperationError('injected failure')
                    return json.dumps(dict(Running=False, OOMKilled=False, ExitCode=0)).encode()
                with patch('promote_test_upgrade.ROOT', folder), patch('promote_test_upgrade.ROUTE', route), \
                        patch('promote_test_upgrade.verify_snapshot', return_value=dict(plaintext_sha256=hashlib.sha256(b'snapshot').hexdigest())), \
                        patch('promote_test_upgrade.candidate_environment', return_value='new'), \
                        patch('promote_test_upgrade.ready'), patch.object(stage, 'execute', side_effect=execute), \
                        patch.object(stage, 'compose', side_effect=lambda new, *a: list(a)), \
                        patch.object(stage, 'state', side_effect=lambda new: after if new else before), \
                        patch.object(stage, 'dump', return_value=b'changed' if failure == 'stale-data' else b'snapshot'):
                    with self.assertRaises(OperationError): promote(stage)
                recorded = json.loads((folder / 'promotion-state.json').read_text())['state']
                if failure == 'reload-caddy':
                    self.assertNotIn('resume-public', calls)
                    self.assertNotIn('restore-route', calls)
                    self.assertEqual(recorded, 'inspect-forward-recovery')
                else:
                    self.assertIn('resume-public', calls)
                    self.assertEqual(recorded, 'old-service-restored')
                    self.assertEqual('restore-route' in calls, failure == 'validate-caddy')


if __name__ == '__main__': unittest.main()
