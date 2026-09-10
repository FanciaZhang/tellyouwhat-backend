import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import blue_green as bg
from ops_common import OperationError, Runtime, atomic_json, read_environment
from release import copy_runtime, KEY_FILES


class BlueGreenTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        (self.root / "secrets").mkdir()
        (self.root / "deploy/tencent").mkdir(parents=True)
        for key in KEY_FILES:
            (self.root / "secrets" / key).write_text("fixture key")
        (self.root / ".env.production").write_text("IMAGE_TAG=old\nIMAGE_REGISTRY_PREFIX=registry/app\n"
              "HEALTH_API_DOMAIN=health.example\nJOURNAL_API_DOMAIN=journal.example\nADMIN_DOMAIN=admin.example\n"
              "PUBLIC_PROXY_MODE=external\n")
        (self.root / "compose.production.yaml").write_text("name: fixture\n")
        self.runtime = Runtime(self.root)
        self.legacy = {"slot": "legacy", "path": ".", "tag": "old", "registry": "registry/app", "healthy": True, "automationVersion": 1}
        atomic_json(self.runtime.state / bg.STATE, {"phase": "STABLE", "current": self.legacy, "previous": None})
        self.processes = {"legacy": self.processes_for("legacy", True)}
        self.route = "legacy"
        self.events = []
        self.fail = None
        for target, replacement in [("resource_check", lambda *a: None), ("ready", lambda *a: True),
                                    ("switch_proxy", self.switch), ("assert_route", self.assert_route),
                                    ("image_metadata", self.images)]:
            patcher = patch.object(bg, target, replacement)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = patch.object(self.runtime, "execute", self.execute)
        patcher.start()
        self.addCleanup(patcher.stop)

    def processes_for(self, slot, active=False):
        return {s: {"protocol": 1, "slot": slot, "bootID": str(i + 1) * 32,
                    "httpEnabled": active, "backgroundEnabled": active, "http": 0, "background": 0}
                for i, s in enumerate(bg.SERVICES)}

    def images(self, runtime, tag, registry):
        return {s: registry + "-" + s + "@sha256:" + "a" * 64 for s in (*bg.SERVICES, "adminctl", "migrate", "maintenance")}

    def bundle(self, tag):
        target = self.root / (".incoming-" + tag)
        copy_runtime(self.root, target)
        (target / "deploy/tencent").mkdir(parents=True)
        (target / "deploy/tencent/runtime.py").write_text("# fixture\n")
        return target

    def execute(self, label, command, **kwargs):
        if label == self.fail:
            raise OperationError("injected " + label)
        if "--env-file" not in command:
            return b""
        cfg = read_environment(command[command.index("--env-file") + 1])
        slot = cfg.get("TELLYOUWHAT_DEPLOYMENT_SLOT", "legacy")
        if label == "slot-config":
            value = {"name": "tellyouwhat-" + slot, "services": {}}
            for service, port, limit in zip(bg.SERVICES, bg.PORTS[slot], (384, 768, 192)):
                value["services"][service] = {"image": cfg[service.upper() + "_IMAGE"],
                    "environment": {"TELLYOUWHAT_DEPLOYMENT_SLOT": slot}, "mem_limit": limit * 1024**2,
                    "ports": [{"host_ip": "127.0.0.1", "published": str(port)}]}
            return json.dumps(value).encode()
        if label in ("slot-start", "slot-rollback-start") and not self.processes.get(slot):
            self.processes[slot] = self.processes_for(slot)
        if label == "slot-running":
            return "\n".join(self.processes.get(slot, {})).encode()
        if label == "slot-stop":
            if any(v["http"] or v["background"] or v["httpEnabled"] or v["backgroundEnabled"] for v in self.processes.get(slot, {}).values()):
                raise AssertionError("stopped an unsealed instance")
            self.events.append((slot, "stop"))
            self.processes[slot] = {}
        controls = [v for v in command if v.startswith("--lifecycle=")]
        if controls:
            action = controls[0].split("=", 1)[1]
            service = next(v.split("=", 1)[1] for v in command if v.startswith("--role="))
            value = self.processes.get(slot, {}).get(service)
            if value is None:
                raise OperationError("process unavailable")
            if action != "status":
                boot = next(v.split("=", 1)[1] for v in command if v.startswith("--boot-id="))
                if boot != value["bootID"]:
                    raise OperationError("process changed")
                self.events.append((slot, action))
                if action == "serve": value["httpEnabled"] = True
                if action == "pause": value["backgroundEnabled"] = False
                if action == "resume": value["backgroundEnabled"] = True
                if action == "seal":
                    if value["http"] or value["background"] or value["backgroundEnabled"]:
                        raise OperationError("busy")
                    value["httpEnabled"] = False
            return json.dumps(value).encode()
        return b""

    def switch(self, runtime, slot):
        self.events.append((slot["slot"], "proxy"))
        self.route = slot["slot"]
        return "revision-" + self.route

    def assert_route(self, slot):
        if slot["slot"] != self.route:
            raise OperationError("proxy mismatch")

    def deploy(self, tag="new"):
        return bg.deploy(self.runtime, tag, "registry/app", "internal", self.bundle(tag), 1)

    def drain(self):
        state = bg.read_state(self.runtime)
        state.update(quiet_since=1, switched_at=1)
        atomic_json(self.runtime.state / bg.STATE, state)
        with patch.object(bg.time, "time", return_value=200):
            return bg.maintain(self.runtime)

    def test_switch_drain_and_next_release_preserve_slot_snapshots(self):
        self.deploy()
        state = bg.read_state(self.runtime)
        self.assertEqual(state["phase"], "DRAINING")
        self.assertEqual(self.route, "green")
        self.assertLess(self.events.index(("legacy", "pause")), self.events.index(("green", "proxy")))
        self.assertLess(self.events.index(("green", "proxy")), self.events.index(("green", "resume")))
        self.assertNotIn(("legacy", "stop"), self.events)
        with self.assertRaisesRegex(OperationError, "draining"):
            bg.deploy(self.runtime, "blocked", "registry/app", "internal", None, 1)
        self.processes["legacy"]["gateway"]["background"] = 1
        self.assertEqual(bg.maintain(self.runtime)["deployment"], "DRAINING")
        self.assertNotIn(("legacy", "stop"), self.events)
        self.processes["legacy"]["gateway"]["background"] = 0
        self.assertEqual(self.drain()["deployment"], "STABLE")
        config = self.root / state["current"]["path"] / ".env.production"
        original = config.read_text()
        selected = Runtime(self.root)
        self.assertEqual(selected.environment_file, config)
        self.assertIn(str(config), selected.compose("ps"))
        self.deploy("next")
        self.assertEqual(self.route, "blue")
        self.assertEqual(config.read_text(), original)
        self.assertEqual(read_environment(self.root / ".env.production")["IMAGE_TAG"], "old")

    def test_failed_migration_restores_old_without_starting_candidate(self):
        self.fail = "slot-migrate"
        with self.assertRaisesRegex(OperationError, "previous route restored"):
            self.deploy()
        self.assertEqual(self.route, "legacy")
        self.assertTrue(all(v["backgroundEnabled"] for v in self.processes["legacy"].values()))
        self.assertNotIn(("legacy", "stop"), self.events)
        self.drain()
        with self.assertRaisesRegex(OperationError, "never passed"):
            bg.rollback(self.runtime, 1)

    def test_candidate_readiness_failure_does_not_cancel_old_work(self):
        self.processes["legacy"]["worker"]["http"] = 1
        with patch.object(bg, "ready", side_effect=lambda r, s, n: s["slot"] == "legacy"):
            with self.assertRaisesRegex(OperationError, "previous route restored"):
                self.deploy()
        self.assertEqual(self.processes["legacy"]["worker"]["http"], 1)
        self.assertEqual(self.route, "legacy")
        self.assertFalse(any(v["backgroundEnabled"] for v in self.processes["green"].values()))

    def test_crash_after_proxy_reload_before_record_can_recover(self):
        def crash(runtime, slot):
            self.switch(runtime, slot)
            raise KeyboardInterrupt("simulated host controller interruption")
        with patch.object(bg, "switch_proxy", crash):
            with self.assertRaises(KeyboardInterrupt): self.deploy()
        self.assertEqual(bg.read_state(self.runtime)["phase"], "SWITCHING")
        self.assertEqual(self.route, "green")
        with self.assertRaisesRegex(OperationError, "interrupted"):
            bg.maintain(self.runtime)
        bg.recover(self.runtime, 1)
        self.assertEqual(self.route, "legacy")
        self.assertTrue(all(v["backgroundEnabled"] for v in self.processes["legacy"].values()))
        self.assertFalse(any(v["backgroundEnabled"] for v in self.processes["green"].values()))

    def test_rollback_and_restart_reconciliation(self):
        self.deploy()
        self.drain()
        result = bg.rollback(self.runtime, 1)
        self.assertEqual(result["current"]["slot"], "legacy")
        self.assertEqual(self.route, "legacy")
        self.processes["legacy"] = self.processes_for("legacy")
        bg.maintain(self.runtime)
        self.assertTrue(all(v["httpEnabled"] and v["backgroundEnabled"] for v in self.processes["legacy"].values()))
        self.assertFalse(any(v["backgroundEnabled"] for v in self.processes["green"].values()))

    def test_proxy_mismatch_and_shared_data_changes_fail_before_mutation(self):
        self.route = "green"
        with self.assertRaisesRegex(OperationError, "proxy mismatch"): self.deploy()
        self.assertEqual(self.events, [])
        self.route = "legacy"
        bundle = self.bundle("changed")
        with (bundle / ".env.production").open("a") as f: f.write("MYSQL_DATABASE=other\n")
        with self.assertRaisesRegex(OperationError, "shared data"):
            bg.deploy(self.runtime, "changed", "registry/app", "internal", bundle, 1)
        self.assertEqual(self.events, [])

    def test_resource_failure_retains_active_service(self):
        with patch.object(bg, "resource_check", side_effect=OperationError("insufficient memory")):
            with self.assertRaisesRegex(OperationError, "memory"): self.deploy()
        self.assertEqual(self.events, [])
        self.assertEqual(bg.read_state(self.runtime)["phase"], "STABLE")


class ProxyRenderingTests(unittest.TestCase):
    def test_preserves_development_route_and_streaming_settings(self):
        source = (Path(__file__).parents[1] / "single-server/Caddyfile.external").read_text()
        rendered = bg.render_proxy(source, {"slot": "green"})
        self.assertIn("reverse_proxy 127.0.0.1:18180", rendered)
        self.assertIn("reverse_proxy 127.0.0.1:18182", rendered)
        self.assertIn("reverse_proxy 127.0.0.1:18787", rendered)
        self.assertIn("stream_close_delay 4h", rendered)
        self.assertEqual(bg.render_proxy(rendered, {"slot": "green"}), rendered)
        self.assertIn("respond @internal 404", rendered)
        with self.assertRaisesRegex(OperationError, "protection"):
            bg.render_proxy(rendered.replace("stream_close_delay 4h", "stream_close_delay 1s"), {"slot": "blue"})

    def test_refuses_unrecognized_proxy_layout(self):
        with self.assertRaisesRegex(OperationError, "layout"):
            bg.render_proxy("reverse_proxy another-server:8080", {"slot": "green"})


if __name__ == "__main__":
    unittest.main()

class ProxyCommitTests(unittest.TestCase):
    def test_reload_failure_atomically_restores_previous_fragment(self):
        import shutil
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            fragment = root / 'backend.caddy'
            main = root / 'Caddyfile'
            original = (Path(__file__).parents[1] / 'single-server/Caddyfile.external').read_text()
            fragment.write_text(original)
            main.write_text('import ' + str(fragment) + '\n')
            class Fixture:
                state = root / 'state'
                def execute(self, label, command):
                    if label == 'proxy-reload':
                        raise OperationError('injected reload failure')
                    if 'install' in command:
                        # The active file is never a copy destination.
                        if label != 'proxy-stage':
                            assert Path(command[-1]) != fragment
                        shutil.copyfile(command[-2], command[-1])
                    if 'mv' in command:
                        Path(command[-2]).replace(command[-1])
                    return b''
            runtime = Fixture()
            with patch.object(bg, 'PROXY_FILE', fragment), patch.object(bg, 'PROXY_ROOT', main):
                with self.assertRaisesRegex(OperationError, 'reload failure'):
                    bg.switch_proxy(runtime, {'slot': 'green'})
            self.assertEqual(fragment.read_text(), original)
