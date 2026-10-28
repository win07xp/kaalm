"""Tests for the kaalm skill's diagnose script
(plugins/kaalm/skills/kaalm/scripts/diagnose.sh).

A fake kubectl first on PATH records every call and prints a canned line, so
the tests show which commands the script runs, that every one of them only
reads, and that every one carries the context it was given.
"""

import os
import pathlib
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "plugins" / "kaalm" / "skills" / "kaalm" / "scripts" / "diagnose.sh"

FAKE_KUBECTL = """#!/usr/bin/env bash
printf '%s\\n' "$*" >> "$KUBECTL_LOG"
echo "fake output for: $*"
"""

READ_VERBS = {"get", "describe", "logs"}


class DiagnoseScriptTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        tmp = pathlib.Path(self._tmp.name)
        bindir = tmp / "bin"
        bindir.mkdir()
        kubectl = bindir / "kubectl"
        kubectl.write_text(FAKE_KUBECTL)
        kubectl.chmod(0o755)
        self.log = tmp / "calls.log"
        self.env = {**os.environ, "PATH": f"{bindir}:{os.environ['PATH']}", "KUBECTL_LOG": str(self.log)}

    def tearDown(self):
        self._tmp.cleanup()

    def run_diagnose(self, *args):
        return subprocess.run(["bash", str(SCRIPT), *args], capture_output=True, text=True, env=self.env)

    def calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def verb(self, call):
        # The first word that is not a flag or a flag's value.
        words = call.split()
        i = 0
        while i < len(words):
            if words[i] in ("--context", "-n", "--namespace"):
                i += 2
                continue
            return words[i]
        return ""

    def test_agent_reads_the_object_its_events_pods_and_controller_log(self):
        res = self.run_diagnose("--context", "k3d-demo", "-n", "team-a", "agent", "support-bot")
        self.assertEqual(res.returncode, 0, res.stderr)
        calls = self.calls()
        joined = "\n".join(calls)
        self.assertIn("get agent support-bot -o yaml", joined)
        self.assertIn("involvedObject.name=support-bot", joined)
        self.assertIn("-l kaalm.io/agent=support-bot", joined)
        self.assertIn("app.kubernetes.io/component=controller", joined)
        self.assertIn("fake output for:", res.stdout)

    def test_agenttask_selects_pods_by_the_task_label(self):
        res = self.run_diagnose("--context", "k3d-demo", "agenttask", "nightly")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("-l kaalm.io/task=nightly", "\n".join(self.calls()))

    def test_cluster_scoped_kind_reads_no_pods(self):
        res = self.run_diagnose("--context", "k3d-demo", "modelprovider", "openai")
        self.assertEqual(res.returncode, 0, res.stderr)
        joined = "\n".join(self.calls())
        self.assertIn("get modelprovider openai -o yaml", joined)
        self.assertNotIn("kaalm.io/agent=", joined)
        self.assertNotIn("get pods", joined)

    def test_every_call_reads_only_and_carries_the_context(self):
        self.run_diagnose("--context", "k3d-demo", "-n", "team-a", "agent", "support-bot")
        calls = self.calls()
        self.assertTrue(calls)
        for call in calls:
            self.assertIn("--context k3d-demo", call)
            self.assertIn(self.verb(call), READ_VERBS, call)

    def test_context_is_required(self):
        res = self.run_diagnose("agent", "support-bot")
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("--context", res.stderr)
        self.assertEqual(self.calls(), [])

    def test_unknown_kind_is_refused(self):
        res = self.run_diagnose("--context", "k3d-demo", "pod", "x")
        self.assertNotEqual(res.returncode, 0)
        self.assertEqual(self.calls(), [])


if __name__ == "__main__":
    unittest.main()
