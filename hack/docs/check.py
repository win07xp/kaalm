#!/usr/bin/env python3
"""check.py: the docs gate for the three mdBooks (docs/, guide/, learn/).

Checks, per book:

- every relative link and image target exists, and every `#anchor` names a
  heading on the target page (mdBook slug rules);
- every page under src/ is listed in SUMMARY.md;
- no em dash (U+2014) or en dash (U+2013) anywhere in a page;
- product pages carry no relative-time or future-promise wording (see
  TIME_PATTERNS); pages whose subject is time (the roadmap, versioning,
  upgrading) are allowlisted, and a single line opts out with the comment
  `<!-- docs-check: allow-time -->`;
- every cited `config/samples/...` or `test/e2e/testdata/...` path exists;
- the scenario coverage map lists S1 to S24 once each;
- every figure the guide embeds (guide/src/diagrams/SOURCES) is a byte-exact
  copy of its design-book source.

Also runs diagram_check.py over docs/src/diagrams.

Usage: check.py [--time-report]
  --time-report   print time-wording hits but do not fail on them.
Exit status 1 when any check fails.
"""

import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
BOOKS = ["docs", "guide", "learn"]

FENCE = re.compile(r"^(```|~~~)")
LINK = re.compile(r"!?\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
INLINE_CODE = re.compile(r"`[^`]*`")
HTML_TAG = re.compile(r"<[^>]+>")
CITED_PATH = re.compile(r"\b((?:config/samples|test/e2e/testdata)/[\w./-]+\.ya?ml)\b")
DASHES = re.compile("[\u2013\u2014]")
ALLOW_TIME = "docs-check: allow-time"

# Wording that anchors a page to a point in time or promises the future.
# Word boundaries keep "known" out of "now" and "v1.16" out of "v1.1".
TIME_PATTERNS = [
    r"\bcurrently\b",
    r"\bat this time\b",
    r"\bat present\b",
    r"\bas of this writing\b",
    r"\bas of (?:today|now|v\d)",
    r"\bfor now\b",
    r"\bnot yet\b",
    r"\bin the future\b",
    r"\bwill be (?:added|introduced|supported|extended|able)\b",
    r"\bplanned\b",
    r"\bnew in\b",
    r"\bsince v\d",
    r"\bfrom v\d+\.\d+",
    r"\bin v\d+\.\d+\.\d+\b",
    r"\bused to\b",
    r"\bpreviously\b",
    r"(?<!as )\bsoon\b",
    r"\bv1\.1\b",
    r"\bpost-1\.0\b",
    r"\bTODO\b",
    r"\bTBD\b",
]
TIME_RE = re.compile("|".join(TIME_PATTERNS), re.IGNORECASE)

# Pages whose subject is time: release state, versioning, upgrades.
TIME_ALLOWLIST = {
    "docs/src/ROADMAP.md",
    "docs/src/operations/api-versioning.md",
    "guide/src/getting-started/upgrading.md",
}
# Sections allowed to name future work, by (page, heading prefix).
TIME_ALLOW_SECTIONS = {
    ("docs/src/concepts/vision-and-scope.md", "Scope for v1"),
}


def slug(text: str) -> str:
    """mdBook's heading id: strip tags, keep [A-Za-z0-9_-] lowercased, spaces to '-'."""
    text = HTML_TAG.sub("", text)
    out = []
    for ch in text:
        if ch.isalnum() or ch in "_-":
            out.append(ch.lower())
        elif ch.isspace():
            out.append("-")
    return "".join(out)


def strip_fences(lines: list[str]) -> list[tuple[int, str]]:
    """Lines outside fenced code blocks, with their 1-based numbers."""
    out, in_fence = [], False
    for n, line in enumerate(lines, 1):
        if FENCE.match(line.strip()):
            in_fence = not in_fence
            continue
        if not in_fence:
            out.append((n, line))
    return out


def page_anchors(lines: list[str]) -> set[str]:
    seen: dict[str, int] = {}
    anchors = set()
    for _, line in strip_fences(lines):
        m = HEADING.match(line)
        if not m:
            continue
        base = slug(m.group(2))
        n = seen.get(base, 0)
        seen[base] = n + 1
        anchors.add(base if n == 0 else f"{base}-{n}")
    return anchors


class Checker:
    def __init__(self, time_report: bool):
        self.problems: list[str] = []
        self.time_hits: list[str] = []
        self.time_report = time_report
        self.anchor_cache: dict[pathlib.Path, set[str]] = {}

    def anchors(self, path: pathlib.Path) -> set[str]:
        if path not in self.anchor_cache:
            self.anchor_cache[path] = page_anchors(path.read_text(encoding="utf-8").splitlines())
        return self.anchor_cache[path]

    def check_book(self, book: str) -> None:
        src = ROOT / book / "src"
        pages = sorted(p for p in src.rglob("*.md"))
        summary = (src / "SUMMARY.md").read_text(encoding="utf-8")
        listed = {src / t for t in re.findall(r"\]\(([^)#]+\.md)\)", summary)}
        for page in pages:
            rel = page.relative_to(ROOT).as_posix()
            if page.name != "SUMMARY.md" and page not in listed:
                self.problems.append(f"{rel}: not listed in SUMMARY.md")
            self.check_page(book, page, rel)

    def check_page(self, book: str, page: pathlib.Path, rel: str) -> None:
        raw = page.read_text(encoding="utf-8")
        for n, line in enumerate(raw.splitlines(), 1):
            if DASHES.search(line):
                self.problems.append(f"{rel}:{n}: em or en dash")
        prose = strip_fences(raw.splitlines())
        for n, line in prose:
            for target in LINK.findall(INLINE_CODE.sub("", line)):
                self.check_link(page, rel, n, target)
            if book != "docs":
                for cited in CITED_PATH.findall(line):
                    if not (ROOT / cited).exists():
                        self.problems.append(f"{rel}:{n}: cited file does not exist: {cited}")
        if rel not in TIME_ALLOWLIST:
            self.check_time(rel, prose)

    def check_link(self, page: pathlib.Path, rel: str, n: int, target: str) -> None:
        if re.match(r"^[a-z]+:", target):
            return  # http, https, mailto
        path_part, _, anchor = target.partition("#")
        if path_part == "":
            dest = page
        else:
            dest = (page.parent / path_part).resolve()
            if not dest.exists():
                self.problems.append(f"{rel}:{n}: link target does not exist: {target}")
                return
        if anchor and dest.suffix == ".md" and anchor not in self.anchors(dest):
            self.problems.append(f"{rel}:{n}: no heading for anchor: {target}")

    def check_time(self, rel: str, prose: list[tuple[int, str]]) -> None:
        allowed_prefixes = {h for (p, h) in TIME_ALLOW_SECTIONS if p == rel}
        in_allowed = False
        for n, line in prose:
            m = HEADING.match(line)
            if m:
                text = m.group(2)
                in_allowed = any(text.startswith(h) for h in allowed_prefixes)
            if in_allowed or ALLOW_TIME in line:
                continue
            hit = TIME_RE.search(INLINE_CODE.sub("", line))
            if hit:
                self.time_hits.append(f"{rel}:{n}: time-bound wording: {hit.group(0)!r}")

    def check_guide_figures(self) -> None:
        sources = ROOT / "guide/src/diagrams/SOURCES"
        for name in sources.read_text(encoding="utf-8").splitlines():
            name = name.strip()
            if not name or name.startswith("#"):
                continue
            src, copy = ROOT / "docs/src/diagrams" / name, ROOT / "guide/src/diagrams" / name
            if not src.exists():
                self.problems.append(f"guide/src/diagrams/SOURCES: no such figure in docs/src/diagrams: {name}")
            elif not copy.exists() or copy.read_bytes() != src.read_bytes():
                self.problems.append(f"guide/src/diagrams/{name}: missing or differs from the source (run make diagrams)")

    def check_coverage_map(self) -> None:
        path = ROOT / "docs/src/appendix/scenario-coverage.md"
        rows = re.findall(r"^\|\s*S(\d+)\b", path.read_text(encoding="utf-8"), re.MULTILINE)
        ids = sorted(int(r) for r in rows)
        expected = list(range(1, 25))
        if ids != expected:
            self.problems.append(
                f"docs/src/appendix/scenario-coverage.md: rows are S{ids} not S1 to S24 once each"
            )


def main(argv: list[str]) -> int:
    time_report = "--time-report" in argv
    checker = Checker(time_report)
    for book in BOOKS:
        checker.check_book(book)
    checker.check_guide_figures()
    checker.check_coverage_map()
    diagrams = subprocess.run(
        [sys.executable, str(ROOT / "hack/docs/diagram_check.py"), str(ROOT / "docs/src/diagrams")],
        capture_output=True, text=True,
    )
    if diagrams.returncode != 0:
        checker.problems.extend(diagrams.stdout.strip().splitlines())
    for p in checker.problems:
        print(p)
    for h in checker.time_hits:
        print(h)
    failed = len(checker.problems) + (0 if time_report else len(checker.time_hits))
    print(
        f"docs-check: {len(checker.problems)} problem(s), {len(checker.time_hits)} time-wording hit(s)"
        + (" (report only)" if time_report and checker.time_hits else ""),
        file=sys.stderr,
    )
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
