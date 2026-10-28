#!/usr/bin/env python3
"""Read and write the kaalm skill's user config, ./.claude/kaalm.json.

Usage:
  config.py show               print the whole config as JSON
  config.py get KEY            print one value; exit 1 if it is not set
  config.py set KEY VALUE      set one value, creating the file if needed
  config.py unset KEY          remove one value
  config.py path               print the file's path

The file lives in the folder the user works from. This script is also the
description of the format: an agent without python3 edits the file by hand
and keeps to FIELDS below.

Format (version 1): a JSON object with "version": 1 and any of these keys.
Nested keys are written with a dot (context.name).

  experience            new-to-kubernetes, uses-kubernetes, or runs-clusters
  goal                  try-it, run-agents, or team-setup
  context.name          the kube context the user confirmed
  context.server        that context's API server address when confirmed
  namespace             the default namespace for the user's agents
  modelProvider         the name of the ModelProvider the user normally uses
  install.chartVersion  the Kaalm chart version that is installed
  install.namespace     the namespace Kaalm is installed in

No other key is accepted, so nothing else, and no secret, lands in the file.

Exit status: 0 on success, 1 when get finds no value, 2 on a usage error, an
unknown key or value, or a file this script cannot read or does not know.
"""

import json
import os
import pathlib
import sys
import tempfile

FORMAT_VERSION = 1
PATH = pathlib.Path(".claude") / "kaalm.json"

# Each settable key, mapped to its allowed values (None allows any non-empty
# string).
FIELDS = {
    "experience": ("new-to-kubernetes", "uses-kubernetes", "runs-clusters"),
    "goal": ("try-it", "run-agents", "team-setup"),
    "context.name": None,
    "context.server": None,
    "namespace": None,
    "modelProvider": None,
    "install.chartVersion": None,
    "install.namespace": None,
}


class ConfigError(Exception):
    pass


def load():
    if not PATH.exists():
        return {"version": FORMAT_VERSION}
    try:
        data = json.loads(PATH.read_text())
    except (OSError, ValueError) as exc:
        raise ConfigError(f"cannot read {PATH}: {exc}") from exc
    if not isinstance(data, dict):
        raise ConfigError(f"{PATH} does not hold a JSON object")
    version = data.get("version")
    if version != FORMAT_VERSION:
        raise ConfigError(
            f"{PATH} has format version {version}; this script knows version "
            f"{FORMAT_VERSION}. Use the skill version that wrote it."
        )
    return data


def save(data):
    PATH.parent.mkdir(parents=True, exist_ok=True)
    # Write a temp file in the same folder and rename it, so a crash never
    # leaves a half-written config.
    fd, tmp = tempfile.mkstemp(dir=PATH.parent, prefix=".kaalm.", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(data, f, indent=2)
            f.write("\n")
        os.replace(tmp, PATH)
    except BaseException:
        os.unlink(tmp)
        raise


def check_key(key):
    if key not in FIELDS:
        raise ConfigError(f"unknown key {key!r}; known keys: {', '.join(FIELDS)}")


def get(data, key):
    node = data
    for part in key.split("."):
        if not isinstance(node, dict) or part not in node:
            return None
        node = node[part]
    return node


def set_value(data, key, value):
    choices = FIELDS[key]
    if choices is not None and value not in choices:
        raise ConfigError(f"{key} must be one of: {', '.join(choices)}")
    if not value:
        raise ConfigError(f"{key} must not be empty")
    *parents, leaf = key.split(".")
    node = data
    for part in parents:
        node = node.setdefault(part, {})
    node[leaf] = value


def unset_value(data, key):
    *parents, leaf = key.split(".")
    node = data
    chain = []
    for part in parents:
        if part not in node:
            return
        chain.append((node, part))
        node = node[part]
    node.pop(leaf, None)
    # Drop a parent object the removal left empty.
    for parent, part in reversed(chain):
        if parent[part]:
            break
        del parent[part]


def main(argv):
    if not argv:
        raise ConfigError(__doc__.split("\n\n")[1])
    cmd, args = argv[0], argv[1:]
    if cmd == "path" and not args:
        print(PATH.resolve())
        return 0
    if cmd == "show" and not args:
        print(json.dumps(load(), indent=2))
        return 0
    if cmd == "get" and len(args) == 1:
        check_key(args[0])
        value = get(load(), args[0])
        if value is None:
            return 1
        print(value)
        return 0
    if cmd == "set" and len(args) == 2:
        check_key(args[0])
        data = load()
        set_value(data, args[0], args[1])
        save(data)
        return 0
    if cmd == "unset" and len(args) == 1:
        check_key(args[0])
        data = load()
        unset_value(data, args[0])
        save(data)
        return 0
    raise ConfigError(__doc__.split("\n\n")[1])


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except ConfigError as exc:
        print(f"config.py: {exc}", file=sys.stderr)
        sys.exit(2)
