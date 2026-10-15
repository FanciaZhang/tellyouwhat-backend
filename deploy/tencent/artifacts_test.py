from datetime import datetime, timezone
import json
import os
from pathlib import Path
import tempfile
import unittest
from contextlib import contextmanager
from unittest.mock import Mock, patch

from ops_artifacts import GRACE_SECONDS, development_deploy, plan_cleanup, register
from ops_common import OperationError


class ArtifactRetentionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve() / "backend"
        self.root.mkdir()
        self.now = 1800000000
        self.state = {"current": {"path": ".slots/green/" + "a" * 40 + "-1", "images": {"gateway": "registry@sha256:current"}},
                      "previous": {"path": ".slots/blue/" + "b" * 40 + "-2", "images": {"gateway": "registry@sha256:previous"}}}

    def file(self, path, age=GRACE_SECONDS + 1):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("fixture")
        os.utime(path, (self.now - age, self.now - age))
        return path

    def image(self, suffix, tags=None, digest=None):
        return {"Id": "sha256:" + suffix, "RepoTags": tags or ["ghcr.io/fanciazhang/tellyouwhat-ai-gateway:" + suffix],
                "RepoDigests": [digest] if digest else [],
                "Created": datetime.fromtimestamp(self.now - GRACE_SECONDS - 1, timezone.utc).isoformat()}

    def test_current_previous_candidate_and_stopped_container_mounts_are_protected(self):
        images = [self.image("current", digest="registry@sha256:current"), self.image("previous", digest="registry@sha256:previous"),
                  self.image("mounted"), self.image("obsolete")]
        old = self.root / ".releases/healthy-old"
        mounted = self.root / ".releases/healthy-mounted"
        for p in (old, mounted):
            self.file(p / "config")
            os.utime(p, (self.now - GRACE_SECONDS - 1,) * 2)
        containers = [{"Image": "sha256:mounted", "Mounts": [{"Type": "bind", "Source": str(mounted / "config")}]}]
        plan = plan_cleanup(self.root, self.state, containers, images, now=self.now)
        self.assertEqual([p["path"] for p in plan["files"]], [str(old)])
        self.assertEqual([i["id"] for i in plan["images"]], ["sha256:obsolete"])

    def test_journal_keeps_two_versions_and_any_container_reference(self):
        directory = self.root.parent / "journal-development/bin"
        versions = [self.file(directory / ("journaldevserver-" + str(i) * 16), age=GRACE_SECONDS + 10 + i) for i in range(5)]
        containers = [{"Image": "sha256:live", "Mounts": [{"Type": "bind", "Source": str(versions[3])}]}]
        self.file(directory / "source.go")
        plan = plan_cleanup(self.root, self.state, containers, [], now=self.now)
        self.assertEqual({p["path"] for p in plan["files"]}, {str(versions[2]), str(versions[4])})

    def test_foreign_alias_and_recent_images_are_not_deleted(self):
        foreign = self.image("foreign", ["ghcr.io/fanciazhang/tellyouwhat-ai-gateway:old", "other-project:keep"])
        recent = self.image("recent")
        recent["Created"] = datetime.fromtimestamp(self.now - 60, timezone.utc).isoformat()
        self.assertEqual(plan_cleanup(self.root, self.state, [], [foreign, recent], now=self.now)["images"], [])

    def test_development_current_and_previous_images_survive_without_containers(self):
        images = [self.image(str(i), ["birthday-development:" + str(i)]) for i in range(4)]
        for i, image in enumerate(images):
            image["Created"] = datetime.fromtimestamp(self.now - GRACE_SECONDS - 10 - i, timezone.utc).isoformat()
        plan = plan_cleanup(self.root, self.state, [], images, now=self.now)
        self.assertEqual([i["id"] for i in plan["images"]], ["sha256:2", "sha256:3"])

    def test_platform_test_cleanup_does_not_select_production_or_other_apps(self):
        images = [self.image(str(i), ["tellyouwhat-platform-test-gateway:" + str(i)]) for i in range(4)]
        for i, image in enumerate(images):
            image["Created"] = datetime.fromtimestamp(self.now - GRACE_SECONDS - 10 - i, timezone.utc).isoformat()
        images += [self.image("production"), self.image("albums", ["tellyouwhat-albums-test-albumgateway:old"])]
        plan = plan_cleanup(self.root, self.state, [], images, owners=["platform-test"], now=self.now)
        self.assertEqual([i["id"] for i in plan["images"]], ["sha256:2", "sha256:3"])

    def test_symlink_directory_and_escape_fail_closed(self):
        directory = self.root.parent / "journal-development"
        directory.mkdir()
        (directory / "bin").symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(OperationError, "symlink"):
            plan_cleanup(self.root, self.state, [], [], now=self.now)
        with self.assertRaisesRegex(OperationError, "escapes"):
            plan_cleanup(self.root, {"current": {"path": "../outside"}}, [], [], now=self.now)

    def test_registered_previous_and_unexpired_lease_are_preserved(self):
        directory = self.root.parent / "journal-development/bin"
        versions = [self.file(directory / ("journaldevserver-" + str(i) * 16), age=GRACE_SECONDS + 10 + i) for i in range(5)]
        registry = self.root / ".operations/artifact-owners"
        registry.mkdir(parents=True)
        (registry / "journal-development.json").write_text(json.dumps({"owner": "journal-development",
            "previous": {"paths": [str(versions[4])]}, "lease": {"expires_at": self.now + 1, "paths": [str(versions[3])]}}))
        plan = plan_cleanup(self.root, self.state, [], [], now=self.now)
        self.assertEqual({p["path"] for p in plan["files"]}, {str(p) for p in versions[:3]})

    def deployment_runtime(self):
        runtime = Mock()
        runtime.root = self.root
        runtime.state = self.root / ".operations"
        events = []

        @contextmanager
        def lock(name="operations", wait_seconds=0):
            events.append(("enter", name, wait_seconds))
            yield
            events.append(("exit", name))

        runtime.lock.side_effect = lock
        return runtime, events

    def test_wrong_operator_cannot_replace_recovery_registration_or_run_producer(self):
        runtime, events = self.deployment_runtime()
        register(runtime, "albums-test", images=["albums:previous"], source="previous")
        record = runtime.state / "artifact-owners/albums-test.json"
        before = record.read_bytes()
        with patch("ops_artifacts.os.geteuid", return_value=self.root.stat().st_uid + 1):
            with self.assertRaisesRegex(OperationError, "backend directory owner"):
                register(runtime, "albums-test", images=["albums:new"], source="new")
            with self.assertRaisesRegex(OperationError, "backend directory owner"):
                development_deploy(runtime, "albums-test", [], ["albums:new"], "new", "run", ["producer"], "http://127.0.0.1:18090/readyz")
        self.assertEqual(record.read_bytes(), before)
        runtime.execute.assert_not_called()
        self.assertEqual(events, [])

    def test_development_registers_only_after_readiness_under_both_locks(self):
        runtime, events = self.deployment_runtime()
        response = Mock(status=200)
        response.geturl.return_value = "http://127.0.0.1:18090/readyz"
        response.read.return_value = b'{"code":"ok"}'
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        with patch("ops_artifacts.urllib.request.urlopen", return_value=response) as open_url, \
                patch("ops_artifacts.register") as register, patch("ops_artifacts.cleanup", return_value={"passed": True}) as cleanup:
            result = development_deploy(runtime, "albums-test", [], ["albums:test"], "revision", "run", ["producer"], response.geturl())
        self.assertTrue(result["passed"])
        self.assertEqual(open_url.call_args.args[0].get_header("Host"), "api.albums.tellyouwhat.cn")
        self.assertEqual(events, [("enter", "operations", 600), ("enter", "deployment", 600),
                                  ("exit", "deployment"), ("exit", "operations")])
        register.assert_called_once_with(runtime, "albums-test", [], ["albums:test"], "revision", "run")
        cleanup.assert_called_once_with(runtime, "albums-test")

    def test_albums_development_readiness_uses_only_its_explicit_host(self):
        runtime, _ = self.deployment_runtime()
        response = Mock(status=200)
        response.geturl.return_value = "http://127.0.0.1:18092/readyz"
        response.read.return_value = b'{"code":"ok"}'
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        with patch("ops_artifacts.urllib.request.urlopen", return_value=response) as open_url, \
                patch("ops_artifacts.register"), patch("ops_artifacts.cleanup", return_value={"passed": True}):
            development_deploy(runtime, "albums-test", [], ["albums:test"], "revision", "run", ["producer"],
                               response.geturl(), verify_host="api-test.albums.tellyouwhat.cn")
        self.assertEqual(open_url.call_args.args[0].get_header("Host"), "api-test.albums.tellyouwhat.cn")
        for owner, host in [("albums-test", "other.example"), ("platform-test", "api-test.albums.tellyouwhat.cn")]:
            runtime, events = self.deployment_runtime()
            with self.assertRaises(OperationError):
                development_deploy(runtime, owner, [], ["test:image"], "revision", "run", ["producer"],
                                   response.geturl(), verify_host=host)
            runtime.execute.assert_not_called()
            self.assertEqual(events, [])

    def test_albums_release_cleanup_preserves_registered_and_mounted_configurations(self):
        base = self.root / '.albums-development/releases'
        folders = [base / (str(i) * 40) for i in range(5)]
        for folder in folders:
            self.file(folder / 'compose.env')
            os.utime(folder, (self.now - GRACE_SECONDS - 1,) * 2)
        register(Mock(root=self.root, state=self.root / '.operations'), 'albums-test',
                 paths=folders[:2], source='current')
        self.file(base / 'operator-note')
        containers = [{'Image': 'sha256:live', 'Mounts': [{'Type': 'bind', 'Source': str(folders[2])}]}]
        plan = plan_cleanup(self.root, self.state, containers, [], owners=['albums-test'], now=self.now)
        self.assertEqual({x['path'] for x in plan['files']}, {str(x) for x in folders[3:]})
        self.assertEqual(plan_cleanup(self.root, self.state, containers, [], owners=['production'], now=self.now)['files'], [])

    def test_platform_readiness_accepts_only_its_test_host(self):
        runtime, _ = self.deployment_runtime()
        response = Mock(status=200)
        response.geturl.return_value = "http://127.0.0.1:18790/readyz"
        response.read.return_value = b'{"status":"ready"}'
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        with patch("ops_artifacts.urllib.request.urlopen", return_value=response) as open_url, \
                patch("ops_artifacts.register"), patch("ops_artifacts.cleanup", return_value={"passed": True}):
            development_deploy(runtime, "platform-test", [], ["test:image"], "revision", "run", ["producer"],
                               response.geturl(), verify_host="api-test.birthday.tellyouwhat.cn")
        self.assertEqual(open_url.call_args.args[0].get_header("Host"), "api-test.birthday.tellyouwhat.cn")
        for host in ("api.birthday.tellyouwhat.cn", "api-test.albums.tellyouwhat.cn", "other.example"):
            with self.subTest(host=host), self.assertRaises(OperationError):
                development_deploy(runtime, "platform-test", [], ["test:image"], "revision", "run", ["producer"],
                                   response.geturl(), verify_host=host)

    def test_platform_release_cleanup_preserves_current_previous_and_live_configurations(self):
        folders = [self.root / '.platform-test/releases' / (str(i) * 40) for i in range(5)]
        for folder in folders:
            self.file(folder / 'compose.env')
            os.utime(folder, (self.now - GRACE_SECONDS - 1,) * 2)
        runtime = Mock(root=self.root, state=self.root / '.operations')
        register(runtime, 'platform-test', paths=folders[:2], source='current')
        containers = [{'Image': 'sha256:live', 'Mounts': [{'Type': 'bind', 'Source': str(folders[2])}]}]
        plan = plan_cleanup(self.root, self.state, containers, [], owners=['platform-test'], now=self.now)
        self.assertEqual({x['path'] for x in plan['files']}, {str(x) for x in folders[3:]})
        self.assertFalse(plan_cleanup(self.root, self.state, containers, [], owners=['production'], now=self.now)['files'])

    def test_readiness_keeps_each_owners_protocol(self):
        for owner, body, accepted in [("birthday-development", b'{"status":"ready"}', True),
                                       ("platform-test", b'{"status":"ready"}', True),
                                       ("platform-test", b'{"code":"ok"}', False),
                                       ("birthday-development", b'{"code":"ok"}', False),
                                       ("albums-test", b'{"code":"unavailable"}', False)]:
            with self.subTest(owner=owner, body=body):
                runtime, _ = self.deployment_runtime()
                response = Mock(status=200)
                response.geturl.return_value = "http://127.0.0.1:18090/readyz"
                response.read.return_value = body
                response.__enter__ = Mock(return_value=response)
                response.__exit__ = Mock(return_value=False)
                with patch("ops_artifacts.urllib.request.urlopen", return_value=response) as open_url, \
                        patch("ops_artifacts.time.sleep"), patch("ops_artifacts.register") as register, \
                        patch("ops_artifacts.cleanup", return_value={"passed": True}):
                    if accepted:
                        development_deploy(runtime, owner, [], ["test:image"], "revision", "run", ["producer"], response.geturl())
                        register.assert_called_once()
                        self.assertIsNone(open_url.call_args.args[0].get_header("Host"))
                    else:
                        with self.assertRaises(OperationError):
                            development_deploy(runtime, owner, [], ["test:image"], "revision", "run", ["producer"], response.geturl())
                        register.assert_not_called()

    def test_failed_readiness_retains_previous_registration_and_skips_cleanup(self):
        runtime, _ = self.deployment_runtime()
        with patch("ops_artifacts.urllib.request.urlopen", side_effect=OSError), patch("ops_artifacts.time.sleep"), \
                patch("ops_artifacts.register") as register, patch("ops_artifacts.cleanup") as cleanup:
            with self.assertRaisesRegex(OperationError, "previous registered artifacts retained"):
                development_deploy(runtime, "albums-test", [], ["albums:test"], "revision", "run", ["producer"], "http://127.0.0.1:18090/readyz")
        register.assert_not_called()
        cleanup.assert_not_called()

    def test_external_readiness_url_is_rejected_before_producer_runs(self):
        runtime, _ = self.deployment_runtime()
        with self.assertRaises(OperationError):
            development_deploy(runtime, "albums-test", [], ["albums:test"], "revision", "run", ["producer"], "http://other-host/readyz")
        runtime.execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
