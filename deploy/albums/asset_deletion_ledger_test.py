import copy
import datetime
import json
import unittest
import time
from unittest.mock import patch

import asset_deletion_ledger as assets
import deletion_ledger
import deletion_ledger_test as ledger_tests
from ops_common import OperationError


class AssetDeletionCheckpointTests(unittest.TestCase):
    write = ledger_tests.DeletionLedgerTests.write
    expired_checkpoint = ledger_tests.DeletionLedgerTests.expired_checkpoint
    def setUp(self):
        ledger_tests.DeletionLedgerTests.setUp(self)
        self.asset = [self.entries[0][0], "asset-1", 3, "retention", self.entries[0][3], ["30000000-0000-4000-8000-000000000001"]]

    def test_asset_receipt_is_authenticated_and_required_even_if_accounts_are_empty(self):
        self.payload["entries"] = []; self.payload["assets"] = [self.asset]
        digest = self.write()
        self.assertEqual(deletion_ledger.verify_checkpoint(self.runtime,self.path,digest,0)["assets"],[self.asset])
        envelope=json.loads(self.path.read_text());envelope["payload"]["assets"]=[];self.path.write_text(json.dumps(envelope))
        with self.assertRaises(OperationError):deletion_ledger.verify_checkpoint(self.runtime,self.path,digest,0)
        self.payload.pop("assets")
        with self.assertRaises(OperationError):deletion_ledger.verify_checkpoint(self.runtime,self.path,self.write(),0)

    def test_wrong_asset_fields_and_ambiguous_versions_never_reach_sql(self):
        for index, value in [(0,"foreign'"),(1,"a' OR 1=1"),(2,True),(2,0),(2,2**63),(3,"expiry"),(4,"2026-02-30T00:00:00.000000Z"),(5,[]),(5,["bad"]),(5,[self.asset[5][0]]*2)]:
            row=copy.deepcopy(self.asset);row[index]=value
            with self.subTest(index=index,value=value):
                with self.assertRaises(OperationError):assets.validate([row])
        other=copy.deepcopy(self.asset);other[1]="another"
        with self.assertRaises(OperationError):assets.validate([self.asset,other])
        with self.assertRaises(OperationError):assets.validate([self.asset,self.asset])
        with patch.object(self.runtime,"execute") as execute:
            with self.assertRaises(OperationError):assets.restore_fences(self.runtime,["mysql"],[other,self.asset])
            execute.assert_not_called()

    def test_export_and_restore_reject_malformed_rows_or_live_target(self):
        row=self.asset;data="\t".join([row[0],row[1],str(row[2]),row[3],row[4],json.dumps(row[5])])+"\n"
        with patch.object(self.runtime,"execute",side_effect=[b"1\t1\n",data.encode()]):self.assertEqual(assets.export(self.runtime,["mysql"],{}),[row])
        for value in [b"broken\trow\n",b"invalid\tasset\t0\ttrash\t\t[]\n",b"\xff"]:
            with patch.object(self.runtime,"execute",side_effect=[b"1\t1\n",value]):
                with self.assertRaises(OperationError):assets.export(self.runtime,["mysql"],{})
        with patch.object(self.runtime,"execute",return_value=b"0\n") as execute:
            with self.assertRaisesRegex(OperationError,"quarantined"):assets.restore_fences(self.runtime,["mysql"],[row])
            self.assertEqual(execute.call_count,1)

    def test_young_asset_keeps_an_otherwise_expired_checkpoint(self):
        self.expired_checkpoint();self.payload["assets"]=[self.asset];self.write()
        self.assertEqual(deletion_ledger.prune_expired(self.runtime,int(time.time())-14*86400),0)
        self.assertTrue(self.path.exists())
        self.asset[4]=self.entries[0][3];self.write()
        self.assertEqual(deletion_ledger.prune_expired(self.runtime,int(time.time())-14*86400),1)

    def test_legacy_completed_purges_require_real_version_evidence(self):
        row = self.asset
        raw = "\t".join([row[0], row[1], str(row[2]), row[3], row[4], json.dumps(row[5])]).encode()
        for body, expected in [(raw, [row]), (b"", [])]:
            with patch.object(self.runtime, "execute", side_effect=[b"0\t0\n", body]) as execute:
                self.assertEqual(assets.export(self.runtime, ["mysql"], {}), expected)
                self.assertIn("LEFT JOIN album_library_changes", execute.call_args.args[1][-1])
        for body in (raw.rsplit(b"\t", 1)[0] + b"\t[null]", raw.replace(b"retention", b"invalid")):
            with patch.object(self.runtime, "execute", side_effect=[b"0\t0\n", body]):
                with self.assertRaises(OperationError):
                    assets.export(self.runtime, ["mysql"], {})
        for schema in (b"0\t1\n", b"", b"2\t0\n", b"\xff"):
            with patch.object(self.runtime, "execute", return_value=schema) as execute:
                with self.assertRaisesRegex(OperationError, "schema"):
                    assets.export(self.runtime, ["mysql"], {})
                self.assertEqual(execute.call_count, 1)

    def test_old_checkpoint_without_asset_coverage_is_preserved_not_used_for_restore(self):
        self.payload['scope']='albums-deletion-ledger-v1';self.payload.pop('assets'); self.payload.pop('collections')
        with self.assertRaises(OperationError):deletion_ledger.verify_checkpoint(self.runtime,self.path,self.write(),0)
        self.assertTrue(self.path.exists())

    def test_combined_account_and_asset_limit_applies_to_publish_and_read(self):
        row=self.asset;data="\t".join([row[0],row[1],str(row[2]),row[3],row[4],json.dumps(row[5])])+"\n"
        with patch.object(deletion_ledger,'MAX_RECORDS',1):
            raw = ("__albums_source__\nalbums_fixture\t1\n__albums_entries__\n" + "\t".join(self.entries[0]) + "\n__albums_assets__\n" + data + "__albums_collections__\n").encode()
            with patch.object(self.runtime,'execute',side_effect=[b"1\t1\n",b"1\t1\n",raw]):
                with self.assertRaisesRegex(OperationError,'combined'):deletion_ledger.publish(self.runtime,['mysql'],{})
            self.payload['assets']=[row]
            with self.assertRaisesRegex(OperationError,'combined'):deletion_ledger.verify_checkpoint(self.runtime,self.path,self.write(),0)
