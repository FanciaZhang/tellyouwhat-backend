import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from backup import (TABLES, SCOPE, RETENTION, configure, create, drill, prune, verify,
                    Runtime, OperationError, manifest_signature, backup_secret)
from ops_backup import verify_backup as verify_control_backup


class AlbumsBackupSafetyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="albums-backup-unit-")
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        env = root / "snapshot.env"
        env.write_text("MYSQL_DATABASE=albums_test\nALBUM_BACKUP_ENCRYPTION_KEY=" + "a" * 64 + "\n")
        env.chmod(0o600)
        self.runtime = Runtime(root, root / "vault", env)
        configure(self.runtime)
        self.path = self.runtime.backups / ("albums-" + "1" * 32 + ".sql.gz.enc")
        self.path.write_bytes(b"authenticated opaque fixture")
        self.path.chmod(0o600)
        self.manifest = {"scope": SCOPE, "created_at": int(time.time()), "database": "albums_test", "filename": self.path.name,
                         "table_rows": {t: 0 for t in TABLES}, "table_sha256": {t: hashlib.sha256(b"").hexdigest() for t in TABLES},
                         "sha256": hashlib.sha256(self.path.read_bytes()).hexdigest()}
        self.sign()

    def sign(self):
        path = Path(str(self.path) + ".json")
        path.write_text(json.dumps({"manifest": self.manifest, "hmac_sha256": manifest_signature(backup_secret(self.runtime), self.manifest)}))
        path.chmod(0o600)

    def test_backup_cli_keeps_albums_offsite_module_in_its_own_scope(self):
        script = "import backup,importlib.util;from pathlib import Path;[print(Path(importlib.util.find_spec(n).origin).resolve()) for n in ('offsite','offsite_test')]"
        result = subprocess.run([sys.executable, '-c', script], cwd=Path(__file__).parent,
                                capture_output=True, text=True, timeout=10, check=True)
        self.assertEqual(result.stdout.splitlines(), [str(Path(__file__).with_name(n).resolve()) for n in ('offsite.py','offsite_test.py')])

    def test_export_child_keeps_its_active_workspace_lease(self):
        root = Path(self.temp.name)
        lease = root / 'owned-export.lease'; lease.write_bytes(b'controlled lease')
        self.runtime.config.update(MYSQL_HOST='fixture', MYSQL_USER='fixture')
        with lease.open('rb') as handle:
            self.runtime._child_pass_fds = (handle.fileno(),)
            executable = root / 'mysqldump'
            executable.write_text('#!' + sys.executable + '\nimport os\n' +
                'assert os.fstat(' + str(handle.fileno()) + ').st_ino == os.stat(' + repr(str(lease)) + ').st_ino\n' +
                '\n'.join('print(' + repr('CREATE TABLE `' + t + '` (id INT);') + ')' for t in TABLES) + '\n' +
                'print("INSERT INTO `album_recovery_state` (`singleton_id`, `quarantined`, `quarantined_at`) VALUES (1,0,NULL);")\n')
            executable.chmod(0o700)
            inventory = ('albums_test\t1\n' + '\n'.join(t + '\tInnoDB' for t in TABLES) + '\n').encode()
            def encrypt(runtime, source, target, **kwargs):
                target.touch(mode=0o600); target.write_bytes(b'controlled encryption boundary')
            with patch.object(self.runtime, 'execute', return_value=inventory), patch('backup.crypt', side_effect=encrypt):
                result = create(self.runtime, root)
            self.assertEqual(len(verify(self.runtime, self.runtime.backups / result['filename'])['table_rows']), len(TABLES))

    def test_explicit_albums_environment_is_not_replaced_by_active_deployment(self):
        active = self.runtime.root / ".slots" / "health"
        active.mkdir(parents=True)
        (active / ".env.production").write_text("MYSQL_DATABASE=health_wrong_source\n")
        (self.runtime.state / "blue-green.json").write_text(json.dumps({"current": {"path": ".slots/health"}}))
        self.runtime.refresh()
        self.assertEqual(configure(self.runtime), "albums_test")

    def test_example_key_is_rejected(self):
        self.runtime.config["ALBUM_BACKUP_ENCRYPTION_KEY"] = "replace-with-at-least-256-bits-of-random-key-material"
        with self.assertRaisesRegex(OperationError, "placeholder"):
            configure(self.runtime)

    def test_schema_coverage_is_complete_and_explicit(self):
        migrations = Path(__file__).resolve().parents[2] / "migrations"
        names = set()
        for file in migrations.glob("*album*.sql"):
            names.update(re.findall(r"CREATE TABLE (?:IF NOT EXISTS )?(album_\w+)", file.read_text()))
        self.assertEqual(set(TABLES), names)
        self.assertEqual(len(TABLES), len(set(TABLES)))
        self.assertTrue(all(t.startswith("album_") for t in TABLES))

    def test_partial_collection_snapshot_coverage_is_rejected(self):
        for table in ['album_collections', 'album_collection_members']:
            manifest = json.loads(json.dumps(self.manifest))
            self.manifest['table_rows'].pop(table)
            self.manifest['table_sha256'].pop(table)
            self.sign()
            with self.assertRaises(OperationError):
                verify(self.runtime, self.path)
            self.manifest = manifest

    def test_unreviewed_live_inventory_is_rejected_before_export(self):
        from backup import create
        self.runtime.config.update(MYSQL_HOST="fixture", MYSQL_USER="fixture")
        inventory = [table + "\tInnoDB" for table in TABLES]
        for changed in (inventory[:-1], sorted(inventory + ["album_unknown\tInnoDB"]),
                        [row for row in inventory if not row.startswith("album_collection_members\t")],
                        [row.replace("\tInnoDB", "\tMyISAM") for row in inventory]):
            source_and_tables = ("albums_test\t1\n" + "\n".join(changed) + "\n").encode()
            with self.subTest(inventory=changed), patch.object(self.runtime, "execute", return_value=source_and_tables), \
                    patch("backup.subprocess.run") as export:
                with self.assertRaisesRegex(OperationError, "schema inventory"):
                    create(self.runtime, Path("/unused"))
                export.assert_not_called()

    def test_historical_snapshots_remain_verifiable_but_unknown_or_partial_coverage_is_rejected(self):
        self.manifest['table_rows'].pop('album_asset_deletion_ledger')
        self.manifest['table_sha256'].pop('album_asset_deletion_ledger')
        self.sign()
        self.assertEqual(verify(self.runtime, self.path), self.manifest)
        for table in ['album_collections', 'album_collection_members']:
            self.manifest['table_rows'].pop(table)
            self.manifest['table_sha256'].pop(table)
        self.sign()
        self.assertEqual(verify(self.runtime, self.path), self.manifest)
        for table in ['album_asset_favorites', 'album_content_corrections', 'album_system_copy_locators']:
            self.manifest['table_rows'].pop(table)
            self.manifest['table_sha256'].pop(table)
            self.sign()
            self.assertEqual(verify(self.runtime, self.path), self.manifest)
        for name in ['album_uploads', 'album_future_unknown']:
            original = json.loads(json.dumps(self.manifest))
            if name in self.manifest['table_rows']:
                self.manifest['table_rows'].pop(name)
                self.manifest['table_sha256'].pop(name)
            else:
                self.manifest['table_rows'][name] = 0
                self.manifest['table_sha256'][name] = hashlib.sha256(b'').hexdigest()
            self.sign()
            with self.assertRaises(OperationError):
                verify(self.runtime, self.path)
            self.manifest = original

    def test_valid_scope_cannot_enter_control_plane_restore(self):
        self.assertEqual(verify(self.runtime, self.path), self.manifest)
        with self.assertRaisesRegex(OperationError, "scope"):
            verify_control_backup(self.runtime, self.path)

    def test_ciphertext_and_manifest_tampering_rejected(self):
        self.path.write_bytes(b"changed")
        with self.assertRaises(OperationError): verify(self.runtime, self.path)
        self.path.write_bytes(b"authenticated opaque fixture")
        self.manifest["created_at"] -= 86400
        envelope = json.loads(Path(str(self.path) + ".json").read_text())
        envelope["manifest"] = self.manifest
        Path(str(self.path) + ".json").write_text(json.dumps(envelope))
        with self.assertRaisesRegex(OperationError, "authentication"): verify(self.runtime, self.path)

    def test_wrong_key_database_scope_and_partial_tables_rejected(self):
        self.runtime.config["ALBUM_BACKUP_ENCRYPTION_KEY"] = "b" * 64
        with self.assertRaises(OperationError): verify(self.runtime, self.path)
        self.runtime.config["ALBUM_BACKUP_ENCRYPTION_KEY"] = "a" * 64
        configure(self.runtime)
        original = json.loads(json.dumps(self.manifest))
        for mutation in (lambda m: m.update(database="other"), lambda m: m.update(scope="control-plane"),
                         lambda m: m["table_rows"].pop("album_uploads"), lambda m: m.update(created_at=int(time.time()) + 3600)):
            self.manifest = json.loads(json.dumps(original))
            mutation(self.manifest)
            self.sign()
            with self.assertRaises(OperationError): verify(self.runtime, self.path)

    def test_expiry_checked_before_decrypt_or_starting_mysql(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        with patch("backup.crypt") as crypto, patch("backup.subprocess.Popen") as server:
            with self.assertRaisesRegex(OperationError, "retention"): drill(self.runtime, Path("/unused"), self.path.name)
        crypto.assert_not_called()
        server.assert_not_called()

    def test_pruning_uses_authenticated_age_and_keeps_unknown_files(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        os.utime(self.path, None)
        other = self.runtime.backups / "user-deliverable.enc"
        other.write_bytes(b"keep")
        self.assertEqual(prune(self.runtime), 1)
        self.assertTrue(other.exists())
        self.assertFalse(self.path.exists())

    def test_corruption_is_preserved_for_review_not_pruned(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        self.path.write_bytes(b"corrupt")
        with self.assertRaises(OperationError): prune(self.runtime)
        self.assertTrue(self.path.exists())

    def test_pruning_resumes_after_ciphertext_was_removed(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        manifest = Path(str(self.path) + ".json")
        before = manifest.read_bytes()
        unlink = Path.unlink

        def interrupted(path, *args, **kwargs):
            if path == manifest:
                raise OSError("synthetic interruption after ciphertext removal")
            return unlink(path, *args, **kwargs)

        with patch.object(Path, "unlink", interrupted):
            with self.assertRaises(OSError):
                prune(self.runtime)
        self.assertFalse(self.path.exists())
        self.assertEqual(manifest.read_bytes(), before)
        self.assertEqual(prune(self.runtime), 1)
        self.assertFalse(manifest.exists())
        self.assertEqual(prune(self.runtime), 0)

    def test_pruning_resumes_after_process_exit_and_releases_operations_lock(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        manifest = Path(str(self.path) + ".json")
        before = manifest.read_bytes()
        child = """
import os, sys
from pathlib import Path
from backup import Runtime, prune
unlink = Path.unlink
def interrupted(path, *args, **kwargs):
    if path.name == sys.argv[4]:
        os._exit(73)
    return unlink(path, *args, **kwargs)
Path.unlink = interrupted
runtime = Runtime(sys.argv[1], sys.argv[2], sys.argv[3])
with runtime.lock():
    prune(runtime)
"""
        result = subprocess.run([sys.executable, "-c", child, str(self.runtime.root), str(self.runtime.backups),
                                 str(self.runtime.base_environment_file), manifest.name],
                                env=dict(os.environ, PYTHONPATH=str(Path(__file__).resolve().parent)),
                                capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 73, result.stderr.decode())
        self.assertFalse(self.path.exists())
        self.assertEqual(manifest.read_bytes(), before)
        with self.runtime.lock(wait_seconds=1):
            self.assertEqual(prune(self.runtime), 1)
        self.assertFalse(manifest.exists())

    def test_pruning_interrupted_before_ciphertext_removal_preserves_the_pair(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        manifest = Path(str(self.path) + ".json")
        before = [self.path.read_bytes(), manifest.read_bytes()]
        unlink = Path.unlink

        def interrupted(path, *args, **kwargs):
            if path == self.path:
                raise KeyboardInterrupt("synthetic cancellation before ciphertext removal")
            return unlink(path, *args, **kwargs)

        with patch.object(Path, "unlink", interrupted):
            with self.assertRaises(KeyboardInterrupt):
                prune(self.runtime)
        self.assertEqual([self.path.read_bytes(), manifest.read_bytes()], before)
        self.assertEqual(prune(self.runtime), 1)

    def test_missing_ciphertext_is_never_a_verified_or_restorable_backup(self):
        self.path.unlink()
        manifest = Path(str(self.path) + ".json")
        with self.assertRaises(OperationError):
            verify(self.runtime, self.path)
        with patch("backup.crypt") as crypto, patch("backup.subprocess.Popen") as server:
            with self.assertRaises(OperationError):
                drill(self.runtime, Path("/unused"), self.path.name)
        crypto.assert_not_called()
        server.assert_not_called()
        with self.assertRaisesRegex(OperationError, "unexpired"):
            prune(self.runtime)
        self.assertTrue(manifest.exists())

    def test_orphan_manifest_authentication_scope_age_and_digest_remain_required(self):
        self.path.unlink()
        manifest = Path(str(self.path) + ".json")
        original = json.loads(json.dumps(self.manifest))
        for changes in [{"database": "other_app"}, {"scope": "control-plane"}, {"sha256": "invalid"},
                        {"created_at": 0}, {"created_at": int(time.time()) + 3600}, {"created_at": True}]:
            with self.subTest(changes=changes):
                self.manifest = dict(original, created_at=int(time.time()) - RETENTION)
                self.manifest.update(changes)
                self.sign()
                before = manifest.read_bytes()
                with self.assertRaises(OperationError):
                    prune(self.runtime)
                self.assertEqual(manifest.read_bytes(), before)
        self.manifest = dict(original, created_at=int(time.time()) - RETENTION)
        self.sign()
        envelope = json.loads(manifest.read_text()); envelope["hmac_sha256"] = "0" * 64
        manifest.write_text(json.dumps(envelope))
        before = manifest.read_bytes()
        with self.assertRaises(OperationError):
            prune(self.runtime)
        self.assertEqual(manifest.read_bytes(), before)

    def test_cleanup_preserves_reappeared_or_linked_or_nonprivate_files(self):
        self.manifest["created_at"] = int(time.time()) - RETENTION
        self.sign()
        manifest = Path(str(self.path) + ".json")
        for path in [self.path, manifest]:
            with self.subTest(member=path.name, condition="nonprivate"):
                path.chmod(0o644)
                with self.assertRaises(OperationError):
                    prune(self.runtime)
                self.assertTrue(self.path.exists()); self.assertTrue(manifest.exists())
                path.chmod(0o600)
            with self.subTest(member=path.name, condition="hardlink"):
                linked = self.runtime.root / "keep-linked-file"
                os.link(path, linked)
                before = linked.read_bytes()
                with self.assertRaises(OperationError):
                    prune(self.runtime)
                self.assertEqual(linked.read_bytes(), before)
                self.assertTrue(self.path.exists()); self.assertTrue(manifest.exists())
                linked.unlink()
        self.path.unlink()
        self.path.write_bytes(b"reappeared different ciphertext")
        self.path.chmod(0o600)
        with self.assertRaises(OperationError):
            prune(self.runtime)
        self.assertEqual(self.path.read_bytes(), b"reappeared different ciphertext")
        self.assertTrue(manifest.exists())

    def test_symlink_and_traversal_and_nonprivate_directory_rejected(self):
        original = self.path.with_name("original.enc")
        self.path.rename(original)
        self.path.symlink_to(original)
        with self.assertRaises(OperationError): verify(self.runtime, self.path)
        with self.assertRaises(OperationError): verify(self.runtime, self.runtime.backups / "../outside.enc")
        self.runtime.backups.chmod(0o755)
        with self.assertRaisesRegex(OperationError, "private"): configure(self.runtime)


if __name__ == "__main__":
    unittest.main()
