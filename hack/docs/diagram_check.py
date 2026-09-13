#!/usr/bin/env python3
"""diagram_check.py: catch the PlantUML mistakes that render without an error.

docs/src/diagrams/_style.puml documents three traps. Each one parses, renders,
and only shows up when someone looks at the image. This script fails the build
on them instead:

1. A quoted hex literal in a skinparam (BackgroundColor "#FFD580") falls back
   to white and the element vanishes.
2. The prefix form of an activity-node colour (#FFD580:label;) draws a
   deprecation banner and breaks `kill`.
3. A label starting with `--` renders struck through (Creole strikethrough)
   unless it is escaped as `~--`.

It also checks that every .puml has a rendered .svg beside it and every .svg
has a source, so a diagram is never committed half-way.

Usage: diagram_check.py [DIAGRAM_DIR]   (default docs/src/diagrams)
Exit status 1 when any problem is found.
"""

import pathlib
import re
import sys

QUOTED_HEX = re.compile(r'\b\w*Color\s+"#[0-9A-Fa-f]{3,8}"')
PREFIX_NODE_COLOUR = re.compile(r"^\s*#[A-Za-z0-9]+:.*;\s*$")
# A label that starts with a double dash: at the start of the line, or right
# after the ':' that opens an activity or note label, or after an opening
# quote or bracket. A double dash later in a sentence renders fine. The ~
# escape is removed before matching so ~-- never trips the rule.
LEADING_DASHES = re.compile(r'(?:^|[:"\[])\s*--(?=[A-Za-z])')


def check_file(path: pathlib.Path) -> list[str]:
    problems = []
    for lineno, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        stripped = line.strip()
        if stripped.startswith("'"):
            continue  # PlantUML comment
        if QUOTED_HEX.search(line):
            problems.append(f"{path}:{lineno}: quoted hex colour in skinparam (renders white)")
        if PREFIX_NODE_COLOUR.match(line):
            problems.append(f"{path}:{lineno}: prefix-form node colour (use :label;<<$var>>)")
        if LEADING_DASHES.search(line.replace("~--", "")):
            problems.append(f"{path}:{lineno}: label starts with -- (escape as ~--)")
    return problems


def main(argv: list[str]) -> int:
    diagram_dir = pathlib.Path(argv[1] if len(argv) > 1 else "docs/src/diagrams")
    problems = []
    sources = {p.stem: p for p in diagram_dir.glob("*.puml") if not p.name.startswith("_")}
    renders = {p.stem: p for p in diagram_dir.glob("*.svg")}
    for stem in sorted(sources.keys() - renders.keys()):
        problems.append(f"{sources[stem]}: no rendered {stem}.svg (run make diagrams)")
    for stem in sorted(renders.keys() - sources.keys()):
        problems.append(f"{renders[stem]}: no {stem}.puml source")
    for path in sorted(diagram_dir.glob("*.puml")):
        problems.extend(check_file(path))
    for problem in problems:
        print(problem)
    if problems:
        print(f"diagram_check: {len(problems)} problem(s)", file=sys.stderr)
        return 1
    print(f"diagram_check: {len(sources)} diagrams ok")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
