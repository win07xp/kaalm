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

Three figure rules are ratchets, printed with a `ratchet: ` prefix so that
check.py can report them without failing when it is told to:

- a .puml is never newer than its .svg by more than a few seconds, so an
  edited source is re-rendered before it is committed (`make diagrams`);
- no `legend` and no `note` block: both become prose or a table beside the
  figure;
- no figure wider than MAX_WIDTH pixels, read from the SVG viewBox, so every
  figure renders at full size in the book's column.

Usage: diagram_check.py [DIAGRAM_DIR]   (default docs/src/diagrams)
Exit status 1 when any problem, ratchets included, is found.
"""

import pathlib
import re
import sys

MAX_WIDTH = 1090
STALE_SECONDS = 5

QUOTED_HEX = re.compile(r'\b\w*Color\s+"#[0-9A-Fa-f]{3,8}"')
PREFIX_NODE_COLOUR = re.compile(r"^\s*#[A-Za-z0-9]+:.*;\s*$")
# A label that starts with a double dash: at the start of the line, or right
# after the ':' that opens an activity or note label, or after an opening
# quote or bracket. A double dash later in a sentence renders fine. The ~
# escape is removed before matching so ~-- never trips the rule.
LEADING_DASHES = re.compile(r'(?:^|[:"\[])\s*--(?=[A-Za-z])')
LEGEND_OR_NOTE = re.compile(r"^\s*(legend|note)\b")
VIEWBOX = re.compile(r'viewBox="[\d.]+ [\d.]+ ([\d.]+) [\d.]+"')


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
        block = LEGEND_OR_NOTE.match(line)
        if block:
            problems.append(f"ratchet: {path}:{lineno}: {block.group(1)} block (put it in prose or a table beside the figure)")
    return problems


def check_render(source: pathlib.Path, render: pathlib.Path) -> list[str]:
    problems = []
    if source.stat().st_mtime > render.stat().st_mtime + STALE_SECONDS:
        problems.append(f"ratchet: {render}: older than {source.name} (run make diagrams)")
    m = VIEWBOX.search(render.read_text(encoding="utf-8", errors="replace")[:2000])
    if m and float(m.group(1)) > MAX_WIDTH:
        problems.append(f"ratchet: {render}: {float(m.group(1)):.0f} px wide, over the {MAX_WIDTH} px limit (split or strip it)")
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
    for stem in sorted(sources.keys() & renders.keys()):
        problems.extend(check_render(sources[stem], renders[stem]))
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
