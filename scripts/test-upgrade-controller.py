#!/usr/bin/env python3
"""Controller failure-injection tests; no Docker daemon or provider access."""
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("upgrade", Path(__file__).with_name("seamless-upgrade.py"))
upgrade = importlib.util.module_from_spec(spec)
spec.loader.exec_module(upgrade)


class ControllerTest(unittest.TestCase):
    def simulate(self, failure=None):
        with tempfile.TemporaryDirectory() as tmp:
            journal = str(Path(tmp) / "release.json")
            argv = ["upgrade", "--old-container", "old", "--new-container", "new", "--check-container", "check",
                    "--old-release", "a", "--new-release", "b", "--old-control", "old.sock", "--new-control", "new.sock",
                    "--gateway-control", "gateway.sock", "--new-target", "http://127.0.0.1:7865", "--journal", journal,
                    "--drain-seconds", "0.01"]
            states = {"old": True, "new": False, "check": False}
            events = []

            def inspect(name):
                return {"State": {"Running": states[name], "ExitCode": 0}}

            def docker(action, name):
                if action == "start":
                    states[name] = name != "check"
                return ""

            def call(path, action="status", **params):
                events.append((path, action))
                if path == "old.sock" and action == "stop":
                    states["old"] = False
                    if failure == "lost_stop":
                        raise OSError("injected lost acknowledgment")
                if action == "status":
                    if path == "gateway.sock":
                        return {"release": "a", "held": False, "active": 0, "wait_seconds": 90}
                    return {"release": "a" if path == "old.sock" else "b", "active": 0,
                            "background": 1 if failure == "busy" else 0, "ready": True}
                if action == "switch" and failure == "switch":
                    raise OSError("injected switch failure")

            class Opener:
                def open(self, *a, **kw):
                    return io.BytesIO(b'{"release":"b"}')

            with patch.object(sys, "argv", argv), patch.object(upgrade, "validate_containers"), \
                 patch.object(upgrade, "inspect", inspect), patch.object(upgrade, "docker", docker), \
                 patch.object(upgrade, "call", call), patch.object(upgrade.urllib.request, "build_opener", return_value=Opener()):
                if failure:
                    with self.assertRaises((TimeoutError, OSError)):
                        upgrade.main()
                else:
                    upgrade.main()
            return json.loads(Path(journal).read_text())["phase"], events, states

    def test_success(self):
        phase, events, states = self.simulate()
        self.assertEqual(phase, "serving")
        self.assertFalse(states["old"])
        self.assertTrue(states["new"])
        self.assertLess(events.index(("old.sock", "stop")), events.index(("gateway.sock", "switch")))

    def test_busy_drain_restores_old(self):
        phase, events, states = self.simulate("busy")
        self.assertEqual(phase, "aborted_old_serving")
        self.assertIn(("old.sock", "resume"), events)
        self.assertIn(("gateway.sock", "resume"), events)
        self.assertTrue(states["old"])
        self.assertFalse(states["new"])

    def test_ambiguous_stop_never_restarts_writer(self):
        phase, events, states = self.simulate("lost_stop")
        self.assertEqual(phase, "recovery_required")
        self.assertNotIn(("old.sock", "resume"), events)
        self.assertFalse(states["new"])

    def test_switch_failure_does_not_restore_old_database(self):
        phase, events, states = self.simulate("switch")
        self.assertEqual(phase, "recovery_required")
        self.assertTrue(states["new"])
        self.assertFalse(states["old"])
        self.assertNotIn(("old.sock", "resume"), events)


if __name__ == "__main__":
    unittest.main()
