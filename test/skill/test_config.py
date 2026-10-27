"""Tests for the kaalm skill's config script (plugins/kaalm/skills/kaalm/scripts/config.py).

The script is run as a user's agent runs it: a separate python3 process in
the user's working folder, so the tests cover the command line, the exit
status, and the file it leaves behind.
"""

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "plugins" / "kaalm" / "skills" / "kaalm" / "scripts" / "config.py"


class ConfigScriptTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.cwd = pathlib.Path(self._tmp.name)
        self.file = self.cwd / ".claude" / "kaalm.json"

    def tearDown(self):
        self._tmp.cleanup()

    def run_config(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args],
            cwd=self.cwd,
            capture_output=True,
            text=True,
            env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
        )

    def test_show_without_a_file_prints_an_empty_config(self):
        res = self.run_config("show")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(json.loads(res.stdout), {"version": 1})
        self.assertFalse(self.file.exists(), "show must not create the file")

    def test_set_creates_the_file_with_the_format_version(self):
        res = self.run_config("set", "experience", "new-to-kubernetes")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(
            json.loads(self.file.read_text()),
            {"version": 1, "experience": "new-to-kubernetes"},
        )

    def test_get_prints_a_set_value(self):
        self.run_config("set", "context.name", "k3d-demo")
        self.run_config("set", "context.server", "https://0.0.0.0:6443")
        res = self.run_config("get", "context.name")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(res.stdout.strip(), "k3d-demo")
        self.assertEqual(
            json.loads(self.file.read_text())["context"],
            {"name": "k3d-demo", "server": "https://0.0.0.0:6443"},
        )

    def test_get_of_an_unset_key_exits_1_with_no_output(self):
        res = self.run_config("get", "namespace")
        self.assertEqual(res.returncode, 1)
        self.assertEqual(res.stdout, "")

    def test_set_rejects_a_key_outside_the_format(self):
        res = self.run_config("set", "apiKey", "sk-123")
        self.assertEqual(res.returncode, 2)
        self.assertIn("apiKey", res.stderr)
        self.assertFalse(self.file.exists())

    def test_set_rejects_a_value_outside_a_field_choices(self):
        res = self.run_config("set", "goal", "everything")
        self.assertEqual(res.returncode, 2)
        self.assertIn("try-it", res.stderr)
        self.assertFalse(self.file.exists())

    def test_set_keeps_the_other_fields(self):
        self.run_config("set", "goal", "run-agents")
        self.run_config("set", "namespace", "agents")
        self.run_config("set", "install.chartVersion", "2.1.0")
        self.run_config("set", "install.namespace", "kaalm-system")
        self.run_config("set", "modelProvider", "openai")
        self.assertEqual(
            json.loads(self.file.read_text()),
            {
                "version": 1,
                "goal": "run-agents",
                "namespace": "agents",
                "install": {"chartVersion": "2.1.0", "namespace": "kaalm-system"},
                "modelProvider": "openai",
            },
        )

    def test_unset_removes_a_field(self):
        self.run_config("set", "namespace", "agents")
        res = self.run_config("unset", "namespace")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(json.loads(self.file.read_text()), {"version": 1})

    def test_a_newer_format_version_is_refused(self):
        self.file.parent.mkdir()
        self.file.write_text(json.dumps({"version": 2, "goal": "try-it"}))
        res = self.run_config("set", "namespace", "agents")
        self.assertEqual(res.returncode, 2)
        self.assertIn("version 2", res.stderr)
        self.assertEqual(json.loads(self.file.read_text()), {"version": 2, "goal": "try-it"})

    def test_a_file_that_is_not_json_is_reported_not_overwritten(self):
        self.file.parent.mkdir()
        self.file.write_text("{not json")
        res = self.run_config("set", "namespace", "agents")
        self.assertEqual(res.returncode, 2)
        self.assertIn(str(self.file.name), res.stderr)
        self.assertEqual(self.file.read_text(), "{not json")

    def test_path_prints_the_file_location(self):
        res = self.run_config("path")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(pathlib.Path(res.stdout.strip()).resolve(), self.file.resolve())


if __name__ == "__main__":
    unittest.main()
