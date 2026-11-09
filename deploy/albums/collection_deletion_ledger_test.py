import json
import time
import unittest
from unittest.mock import patch

import collection_deletion_ledger as collections
import deletion_ledger
import deletion_ledger_test as ledger_tests
from ops_common import OperationError


class CollectionDeletionCheckpointTests(unittest.TestCase):
    write = ledger_tests.DeletionLedgerTests.write
    expired_checkpoint = ledger_tests.DeletionLedgerTests.expired_checkpoint

    def setUp(self):
        ledger_tests.DeletionLedgerTests.setUp(self)
        self.row = [self.entries[0][0], '30000000-0000-4000-8000-000000000001', 3,
                    '40000000-0000-4000-8000-000000000001', self.entries[0][3]]

    def test_collection_fences_are_required_and_authenticated(self):
        self.payload['collections'] = [self.row]
        digest = self.write()
        self.assertEqual(deletion_ledger.verify_checkpoint(self.runtime, self.path, digest, 0)['collections'], [self.row])
        body = json.loads(self.path.read_text()); body['payload']['collections'] = []
        self.path.write_text(json.dumps(body))
        with self.assertRaises(OperationError):
            deletion_ledger.verify_checkpoint(self.runtime, self.path, digest, 0)
        self.payload.pop('collections')
        with self.assertRaises(OperationError):
            deletion_ledger.verify_checkpoint(self.runtime, self.path, self.write(), 0)

    def test_invalid_or_duplicate_rows_cannot_reach_sql(self):
        for index, value in [(0, "owner'"), (1, '00000000-0000-0000-0000-000000000000'),
                             (2, True), (2, 0), (2, 2**63), (3, "request'"), (4, None),
                             (4, '2026-02-30T00:00:00.000000Z')]:
            row = self.row.copy(); row[index] = value
            with self.subTest(index=index, value=value), patch.object(self.runtime, 'execute') as execute:
                with self.assertRaises(OperationError):
                    collections.restore_fences(self.runtime, ['mysql'], [row])
                execute.assert_not_called()
        for rows in [[self.row, self.row], [self.row + ['extra']], [self.row] * 100001]:
            with self.assertRaises(OperationError): collections.validate(rows)

    def test_export_requires_schema_evidence_and_valid_rows(self):
        raw = ('\t'.join(map(str, self.row)) + '\n').encode()
        for schema in (b'1\t0\n', b'1\t1\n'):
            with patch.object(self.runtime, 'execute', side_effect=[schema, raw]):
                self.assertEqual(collections.export(self.runtime, ['mysql'], {}), [self.row])
        with patch.object(self.runtime, 'execute', return_value=b'0\t0\n') as execute:
            self.assertEqual(collections.export(self.runtime, ['mysql'], {}), [])
            execute.assert_called_once()
        for schema in (b'0\t1\n', b'', b'invalid'):
            with patch.object(self.runtime, 'execute', return_value=schema):
                with self.assertRaises(OperationError): collections.export(self.runtime, ['mysql'], {})
        for body in (b'broken\n', b'\xff', raw + raw):
            with patch.object(self.runtime, 'execute', side_effect=[b'1\t1\n', body]):
                with self.assertRaises(OperationError): collections.export(self.runtime, ['mysql'], {})

    def test_live_database_is_refused_before_any_schema_or_write(self):
        with patch.object(self.runtime, 'execute', return_value=b'0\n') as execute:
            with self.assertRaisesRegex(OperationError, 'quarantined'):
                collections.restore_fences(self.runtime, ['mysql'], [self.row])
            execute.assert_called_once()

    def test_young_collection_tombstone_retains_an_old_checkpoint(self):
        self.expired_checkpoint(); self.payload['collections'] = [self.row]; self.write()
        cutoff = int(time.time()) - 14 * 86400
        self.assertEqual(deletion_ledger.prune_expired(self.runtime, cutoff), 0)
        self.assertTrue(self.path.exists())
        self.row[4] = self.entries[0][3]; self.write()
        self.assertEqual(deletion_ledger.prune_expired(self.runtime, cutoff), 1)

    def test_asset_only_legacy_keeps_original_expiry_but_cannot_authorize_restore(self):
        self.expired_checkpoint(); self.payload['scope'] = 'albums-deletion-ledger-v2'
        self.payload.pop('collections'); expired = self.payload['exported_at']
        self.payload['exported_at'] = int(time.time()); digest = self.write()
        self.assertEqual(deletion_ledger.prune_expired(self.runtime, int(time.time()) - 14*86400), 0)
        with self.assertRaises(OperationError):
            deletion_ledger.verify_checkpoint(self.runtime, self.path, digest, 0)
        self.payload['exported_at'] = expired; digest = self.write()
        self.assertEqual(deletion_ledger.verify_cleanup_checkpoint(self.runtime, self.path, digest, 0)['scope'], 'albums-deletion-ledger-v2')
        with self.assertRaises(OperationError):
            deletion_ledger.verify_checkpoint(self.runtime, self.path, digest, 0)
        self.assertEqual(deletion_ledger.prune_expired(self.runtime, int(time.time()) - 14*86400), 1)


    def test_combined_checkpoint_budget_includes_collection_rows(self):
        self.payload['collections'] = [self.row]
        with patch.object(deletion_ledger, 'MAX_RECORDS', 1):
            with self.assertRaisesRegex(OperationError, 'combined'):
                deletion_ledger.verify_checkpoint(self.runtime, self.path, self.write(), 0)
            raw = ('__albums_source__\nalbums_fixture\t1\n__albums_entries__\n' + '\t'.join(self.entries[0]) + '\n__albums_assets__\n__albums_collections__\n' + '\t'.join(map(str, self.row)) + '\n').encode()
            with patch.object(self.runtime, 'execute', side_effect=[b'1\t1\n', b'1\t1\n', raw]):
                with self.assertRaisesRegex(OperationError, 'combined'):
                    deletion_ledger.publish(self.runtime, ['mysql'], {})


if __name__ == '__main__':
    unittest.main()
