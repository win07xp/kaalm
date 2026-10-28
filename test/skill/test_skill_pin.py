"""Tests for hack/skill-pin.sh, which the release PR runs to move the kaalm
skill to the release version. It runs on a copy of the plugin folder, so the
real skill is never edited."""

import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack" / "skill-pin.sh"
BLOB = "https://github.com/win07xp/kaalm/blob"


class SkillPinTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.plugin = pathlib.Path(self._tmp.name) / "kaalm"
        (self.plugin / ".claude-plugin").mkdir(parents=True)
        (self.plugin / "skills" / "kaalm" / "procedures").mkdir(parents=True)
        (self.plugin / ".claude-plugin" / "plugin.json").write_text(
            json.dumps({"name": "kaalm", "version": "2.0.1", "description": "d"}, indent=2) + "\n"
        )
        self.skill = self.plugin / "skills" / "kaalm" / "SKILL.md"
        self.skill.write_text(
            "---\nname: kaalm\n---\n\nThis skill matches Kaalm v2.0.1.\n\n"
            f"See [install]({BLOB}/main/guide/src/getting-started/installation.md).\n"
        )
        self.proc = self.plugin / "skills" / "kaalm" / "procedures" / "first-agent.md"
        self.proc.write_text(
            f"Old [link]({BLOB}/v2.0.1/guide/src/developers/first-agent.md#1-pick-an-image)\n"
            f"Other repo https://github.com/other/kaalm/blob/main/x.md stays.\n"
        )
        (self.plugin / "skills" / "kaalm" / "examples").mkdir()
        self.example = self.plugin / "skills" / "kaalm" / "examples" / "agent.yaml"
        self.example.write_text(
            "spec:\n  image: ghcr.io/win07xp/kaalm-agent-python:2.0.1\n"
            "  other: ghcr.io/win07xp/kaalm-agent-go:2.0.1\n"
            "  mine: ghcr.io/example/my-agent:2.0.1\n"
        )

    def tearDown(self):
        self._tmp.cleanup()

    def pin(self, *args):
        return subprocess.run(
            ["bash", str(SCRIPT), *args],
            capture_output=True,
            text=True,
            env={**os.environ, "SKILL_PLUGIN_DIR": str(self.plugin)},
        )

    def test_pins_links_version_and_stated_version(self):
        res = self.pin("2.1.0")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(json.loads((self.plugin / ".claude-plugin" / "plugin.json").read_text())["version"], "2.1.0")
        skill = self.skill.read_text()
        self.assertIn("This skill matches Kaalm v2.1.0.", skill)
        self.assertIn(f"{BLOB}/v2.1.0/guide/src/getting-started/installation.md", skill)
        proc = self.proc.read_text()
        self.assertIn(f"{BLOB}/v2.1.0/guide/src/developers/first-agent.md#1-pick-an-image", proc)
        self.assertIn("https://github.com/other/kaalm/blob/main/x.md", proc)
        example = self.example.read_text()
        self.assertIn("ghcr.io/win07xp/kaalm-agent-python:2.1.0\n", example)
        self.assertIn("ghcr.io/win07xp/kaalm-agent-go:2.1.0\n", example)
        self.assertIn("ghcr.io/example/my-agent:2.0.1\n", example)

    def test_accepts_a_leading_v_and_a_prerelease(self):
        res = self.pin("v2.1.0-rc.1")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn("This skill matches Kaalm v2.1.0-rc.1.", self.skill.read_text())

    def test_rejects_a_version_that_is_not_semver(self):
        res = self.pin("latest")
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("This skill matches Kaalm v2.0.1.", self.skill.read_text())

    def test_without_a_version_prints_usage(self):
        res = self.pin()
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("usage", res.stderr.lower())


if __name__ == "__main__":
    unittest.main()
