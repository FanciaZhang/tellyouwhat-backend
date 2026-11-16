import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from backup import Runtime, configure, prune, OperationError, cleanup_staging, staging, STAGING_SCOPE
from ops_backup import backup_secret, manifest_signature
from ops_common import atomic_json


class OffsiteStagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="albums-staging-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        env = self.root / "env"
        env.write_text("MYSQL_DATABASE=albums_test\nALBUM_BACKUP_ENCRYPTION_KEY=" + "a" * 64 + "\n")
        env.chmod(0o600)
        self.runtime = Runtime(self.root, self.root / "vault", env)
        configure(self.runtime)
        self.original = self.runtime.backups / "preserved-original"
        self.original.write_bytes(b"original synthetic recovery input")

    def test_success_failure_and_cancellation_cleanup_owned_copies_only(self):
        for failure in (None, RuntimeError, KeyboardInterrupt):
            try:
                with staging(self.runtime, "receipt", ("receipt.json",)) as folder:
                    atomic_json(folder / "receipt.json", {"synthetic": True})
                    if failure: raise failure("interrupted")
            except (RuntimeError, KeyboardInterrupt):
                pass
            self.assertFalse(folder.exists())
            self.assertEqual(getattr(self.runtime, "_child_pass_fds", ()), ())
            self.assertEqual(self.original.read_bytes(), b"original synthetic recovery input")

    def test_recovery_destination_collision_preserves_existing_and_late_files(self):
        destination = self.runtime.backups / "albums-recovered-12345678"
        destination.mkdir(mode=0o700)
        original = destination / "preserved-original"; original.write_bytes(b"existing recovery input")
        with self.assertRaises(OperationError):
            with staging(self.runtime, "offsite", ("receipt.json",), destination=destination):
                self.fail("occupied destination admitted")
        self.assertEqual(list(self.runtime.backups.glob(".albums-offsite-*")), [])
        original.unlink(); destination.rmdir()
        with self.assertRaises(OperationError):
            with staging(self.runtime, "offsite", ("receipt.json",), destination=destination) as folder:
                atomic_json(folder / "receipt.json", {"synthetic": True})
                destination.mkdir(mode=0o700)
                original.write_bytes(b"late independent recovery input")
        self.assertFalse(folder.exists())
        self.assertEqual(original.read_bytes(), b"late independent recovery input")

    def test_process_exit_releases_lock_and_next_prune_recovers_owned_staging(self):
        code = """
import os,sys
from pathlib import Path
from backup import Runtime,configure
from offsite import staging
from ops_common import atomic_json
r=Runtime(sys.argv[1],sys.argv[2],sys.argv[3]);configure(r)
with r.lock():
    with staging(r,'receipt',('receipt.json',)) as folder:
        atomic_json(folder/'receipt.json',{'synthetic':True})
        os._exit(73)
"""
        exited = subprocess.run([sys.executable, "-c", code, str(self.root), str(self.runtime.backups),
                                 str(self.runtime.environment_file)], capture_output=True, timeout=10)
        self.assertEqual(exited.returncode, 73, exited.stderr)
        self.assertEqual(len(list(self.runtime.backups.glob(".albums-receipt-*"))), 1)
        with self.runtime.lock():
            self.assertEqual(prune(self.runtime), 0)
        self.assertEqual(list(self.runtime.backups.glob(".albums-receipt-*")), [])
        self.assertTrue(self.original.exists())

    def test_child_inherits_lease_until_actual_process_exit(self):
        ready, stop = self.root / "ready", self.root / "stop"
        failures = []
        code = "import pathlib,sys,time\np=pathlib.Path(sys.argv[1]);p.write_text('ready')\nwhile not pathlib.Path(sys.argv[2]).exists():time.sleep(.01)\n"
        def child():
            try:
                self.runtime.execute("controlled-stage-reader", [sys.executable, "-c", code, str(ready), str(stop)], timeout=10)
            except BaseException as error:
                failures.append(error)
        thread = threading.Thread(target=child)
        try:
            with self.assertRaisesRegex(OperationError, "still in use"):
                with staging(self.runtime, "receipt", ("receipt.json",)) as folder:
                    atomic_json(folder / "receipt.json", {"synthetic": True})
                    thread.start()
                    deadline = time.monotonic() + 5
                    while not ready.exists() and time.monotonic() < deadline:
                        time.sleep(.01)
                    self.assertTrue(ready.exists(), failures)
            self.assertTrue(folder.exists())
            with self.assertRaisesRegex(OperationError, "still in use"):
                cleanup_staging(self.runtime)
            self.assertTrue((folder / "receipt.json").exists())
        finally:
            stop.touch()
            if thread.ident is not None: thread.join(12)
        self.assertFalse(thread.is_alive())
        self.assertEqual(failures, [])
        cleanup_staging(self.runtime)
        self.assertFalse(folder.exists())

    def test_interrupted_unlink_resumes_with_owner_marker(self):
        folder = Path(tempfile.mkdtemp(prefix=".albums-receipt-", dir=self.runtime.backups))
        owner = dict(scope=STAGING_SCOPE, database="albums_test", directory=folder.name, files=["receipt.json"])
        atomic_json(folder / "owner.json", dict(owner=owner, hmac_sha256=manifest_signature(backup_secret(self.runtime), owner)))
        atomic_json(folder / "receipt.json", {"synthetic": True})
        unlink = os.unlink
        def interrupted(path, *args, **kwargs):
            if Path(path).name == "owner.json": raise OSError("controlled interruption")
            return unlink(path, *args, **kwargs)
        with patch("backup.os.unlink", interrupted), self.assertRaises(OperationError):
            cleanup_staging(self.runtime)
        self.assertFalse((folder / "receipt.json").exists())
        self.assertTrue((folder / "owner.json").exists())
        cleanup_staging(self.runtime)
        self.assertFalse(folder.exists())

    def test_unknown_tampered_and_linked_content_is_preserved(self):
        for mode in ("unknown", "tamper", "foreign", "symlink", "hardlink", "public"):
            with self.subTest(mode=mode):
                folder = Path(tempfile.mkdtemp(prefix=".albums-receipt-", dir=self.runtime.backups))
                owner = dict(scope=STAGING_SCOPE, database="albums_test", directory=folder.name, files=["receipt.json"])
                if mode == "foreign": owner["database"] = "other"
                atomic_json(folder / "owner.json", dict(owner=owner, hmac_sha256=manifest_signature(backup_secret(self.runtime), owner)))
                if mode == "symlink": (folder / "receipt.json").symlink_to(self.original)
                elif mode == "hardlink": os.link(self.original, folder / "receipt.json")
                else: atomic_json(folder / "receipt.json", {"synthetic": True})
                if mode == "unknown": (folder / "unknown").touch()
                elif mode == "tamper": (folder / "owner.json").write_text("{}")
                elif mode == "public": (folder / "receipt.json").chmod(0o644)
                with self.assertRaises(OperationError): cleanup_staging(self.runtime)
                self.assertTrue((folder / "receipt.json").exists())
                self.assertEqual(self.original.read_bytes(), b"original synthetic recovery input")
                # Only the test fixture owns these deliberately invalid directories.
                import shutil
                shutil.rmtree(folder)
