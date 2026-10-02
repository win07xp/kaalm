#!/usr/bin/env python3
"""check.py: the docs gate for the three mdBooks (docs/, guide/, learn/).

Checks, per book:

- every relative link and image target exists and stays inside the book,
  and every `#anchor` names a heading on the target page (mdBook slug
  rules). Inline links, links wrapped across lines, reference-style links,
  and HTML `href` and `src` attributes are all resolved. A link to this
  repository's own files on GitHub (`BLOB_URL`, the guide's links into the
  design book) is resolved against the local file the same way;
- every page under src/ is listed in SUMMARY.md;
- no em dash (U+2014) or en dash (U+2013) anywhere in a page;
- no relative-time or future-promise wording outside TIME_ALLOWLIST (see
  TIME_PATTERNS). A section named in TIME_ALLOW_SECTIONS is exempt until the
  next heading of any level, and a single line opts out with the comment
  `<!-- docs-check: allow-time -->` on that same line;
- none of the words the prose rules ban (see WORDING_PATTERNS) outside code;
- every cited `config/samples/...` or `test/e2e/testdata/...` path exists;
- the '## S<n>:' headings of docs/src/appendix/scenarios.md run S1 to S<N>
  once each, and the scenario coverage map has exactly one row for each;
- every top-level container (a Describe, Context, When, or DescribeTable at
  `var _ =`) in test/e2e or test/upgrade is quoted as a spec in some row of
  the map, or is listed in NON_SCENARIO_SPECS. Every entry of that list is
  such a container;
- a nested container, or an It with no S-number, may be quoted in any row or
  none. Only its top-level container is checked;
- every spec label the map's e2e column quotes is a Describe or It label in
  test/e2e or test/upgrade. A label is the cell's first code span, a span
  after ' + ', or a span that starts with a capital letter and contains a
  space;
- every Describe or It label that carries an S-number, as '(S<n>' or a
  leading 'S<n>:', is quoted in that S-number's row, as itself or as its
  Describe;
- on any other page, a code span that contains '(S<n>' names a real label;
- every figure the guide embeds is listed in guide/src/diagrams/SOURCES, every
  listed figure is a byte-exact copy of its design-book source, and nothing
  else sits in guide/src/diagrams.

Also runs diagram_check.py over docs/src/diagrams.

Usage: check.py [--time-report] [--ratchet-report]
  --time-report      print time-wording hits but do not fail on them.
  --ratchet-report   print the wording ban and the figure ratchets (legend
                     and note blocks, width, stale renders) but do not fail
                     on them.
Exit status 1 when any check fails.
"""

import bisect
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
# A link to this repository's own file on GitHub: (path, anchor).
BLOB_URL = re.compile(r"^https://github\.com/win07xp/kaalm/blob/main/([^#?]+)(?:#([\w-]+))?$")
BOOKS = ["docs", "guide", "learn"]

FENCE = re.compile(r"^(```|~~~)")
# Inline links and images. The text part may span lines; the target may not.
LINK = re.compile(r"!?\[[^\]]*\]\(\s*([^)\s]+)(?:\s+\"[^\"]*\")?\s*\)")
REF_USE = re.compile(r"(?<!\])\[[^\]]+\]\[([^\]]*)\]")
REF_DEF = re.compile(r"^\s{0,3}\[([^\]]+)\]:\s*(\S+)", re.MULTILINE)
HTML_LINK = re.compile(r"<(?:a|img)\b[^>]*?\b(?:href|src)=\"([^\"]+)\"", re.IGNORECASE)
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
INLINE_CODE = re.compile(r"`[^`]*`")
LINK_TARGET = re.compile(r"\]\([^)]*\)")
HTML_TAG = re.compile(r"<[^>]+>")
CITED_PATH = re.compile(r"\b((?:config/samples|test/e2e/testdata)/[\w./-]+\.ya?ml)\b")
DASHES = re.compile("[–—]")
# Ginkgo node labels, checked against the scenario coverage map and against
# spec labels quoted on other pages.
SPEC_DIRS = ("test/e2e", "test/upgrade")
COVERAGE_MAP = "docs/src/appendix/scenario-coverage.md"
SCENARIOS = "docs/src/appendix/scenarios.md"
# Applied to the text of a HEADING match: "S7: title" gives 7.
SCENARIO_HEADING = re.compile(r"^S(\d+):")
TOP_LEVEL_PREFIX = re.compile(r"var\s+_\s*=\s*")
# Top-level e2e and upgrade containers that no row of the coverage map covers.
# Each key is a container label; each value is the reason it has no row.
NON_SCENARIO_SPECS = {
    "Mock LLM provider": "test infrastructure; it checks the in-cluster mock upstream that the LLM scenarios use",
    "Metric catalog on the wire (#97)": "the metric catalog, not an acceptance scenario",
    "Per-workload spend (#100)": "per-workload spend attribution, not an acceptance scenario",
    "FQDN egress on a Cilium CNI": (
        "runs only under `make e2e-cilium`, which selects it with -ginkgo.focus=\"FQDN\"; "
        "the map's prose describes it. A rename must keep \"FQDN\" or update the Makefile focus"
    ),
}
SPEC_NODE = re.compile(
    r'\b[FPX]?(Describe|Context|When|It|Specify|DescribeTable)\(\s*(?:"((?:[^"\\]|\\.)*)"|`([^`]*)`)'
)
SPEC_SNUM = re.compile(r"\(S(\d+)\b|^S(\d+):")
QUOTED_LABEL = re.compile(r"\(S\d+\b")
GUIDE_EMBED = re.compile(r"\]\((?:\.\./)+diagrams/([\w-]+\.svg)\)")
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

# Words the prose rules ban outright (root CLAUDE.md and the
# google-dev-docs-style skill). Judgment calls (metaphors, "shape" as a
# design noun) stay a hand sweep; only the flat bans are mechanical.
WORDING_PATTERNS = [
    r"\bvia\b",
    r"\be\.g\.",
    r"\bi\.e\.",
    r"\bjust\b",
    r"\beasy\b",
    r"\bsimple\b",
    r"\bsimply\b",
    r"\bseam\b",
    r"\bposture\b",
    r"\bmachinery\b",
    r"\bplumbing\b",
    r"\bload-bearing\b",
]
WORDING_RE = re.compile("|".join(WORDING_PATTERNS), re.IGNORECASE)

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


class ProseText:
    """The prose of a page joined into one string, with offset-to-line lookup."""

    def __init__(self, prose: list[tuple[int, str]]):
        self.lines = prose
        starts, pos, parts = [], 0, []
        for _, line in prose:
            starts.append(pos)
            parts.append(line)
            pos += len(line) + 1
        self.starts = starts
        self.text = "\n".join(parts)

    def line_at(self, offset: int) -> int:
        i = bisect.bisect_right(self.starts, offset) - 1
        return self.lines[i][0] if self.lines else 0


class Checker:
    def __init__(self, time_report: bool, ratchet_report: bool):
        self.problems: list[str] = []
        self.time_hits: list[str] = []
        self.ratchet_hits: list[str] = []
        self.time_report = time_report
        self.ratchet_report = ratchet_report
        self.anchor_cache: dict[pathlib.Path, set[str]] = {}
        self.spec_cache: (
            tuple[dict[str, str], list[tuple[int, str, str, str]], list[tuple[str, str]]] | None
        ) = None

    def anchors(self, path: pathlib.Path) -> set[str]:
        if path not in self.anchor_cache:
            self.anchor_cache[path] = page_anchors(path.read_text(encoding="utf-8").splitlines())
        return self.anchor_cache[path]

    def spec_labels(
        self,
    ) -> tuple[dict[str, str], list[tuple[int, str, str, str]], list[tuple[str, str]]]:
        """The Ginkgo node labels in SPEC_DIRS.

        Returns (labels, numbered, top): labels maps each label to its first
        'path:line'; numbered holds (n, label, enclosing Describe, 'path:line')
        for each S-number a label carries; top holds (label, 'path:line') for
        each top-level container, which is a Describe, Context, When, or
        DescribeTable on a line that starts with `var _ =`. A nested container
        is not top-level. The glob is not recursive, so the mock servers'
        plain Go tests under test/e2e/mock* are skipped. The enclosing
        Describe is the nearest preceding Describe in the file; no spec file
        nests Describes.
        """
        if self.spec_cache is not None:
            return self.spec_cache
        labels: dict[str, str] = {}
        numbered: list[tuple[int, str, str, str]] = []
        top: list[tuple[str, str]] = []
        for d in SPEC_DIRS:
            for f in sorted((ROOT / d).glob("*_test.go")):
                rel = f.relative_to(ROOT).as_posix()
                text = f.read_text(encoding="utf-8")
                describe = ""
                for m in SPEC_NODE.finditer(text):
                    kind = m.group(1)
                    label = m.group(2) if m.group(2) is not None else m.group(3)
                    where = f"{rel}:{text.count(chr(10), 0, m.start()) + 1}"
                    if kind in ("Describe", "DescribeTable"):
                        describe = label
                    labels.setdefault(label, where)
                    line_start = text.rfind("\n", 0, m.start()) + 1
                    if kind in ("Describe", "Context", "When", "DescribeTable") and TOP_LEVEL_PREFIX.fullmatch(
                        text[line_start : m.start()]
                    ):
                        top.append((label, where))
                    for s in SPEC_SNUM.finditer(label):
                        numbered.append((int(s.group(1) or s.group(2)), label, describe, where))
        self.spec_cache = (labels, numbered, top)
        return self.spec_cache

    def scenario_ids(self) -> list[int]:
        """The S-numbers of the '## S<n>: title' headings in SCENARIOS.

        Only level-2 headings count, so a '### S7: ...' subheading is not a
        scenario.
        """
        lines = (ROOT / SCENARIOS).read_text(encoding="utf-8").splitlines()
        ids = []
        for _, line in strip_fences(lines):
            h = HEADING.match(line)
            s = SCENARIO_HEADING.match(h.group(2)) if h and h.group(1) == "##" else None
            if s:
                ids.append(int(s.group(1)))
        if not ids:
            self.problems.append(f"{SCENARIOS}: no scenario headings of the form '## S<n>: title'")
            return []
        if sorted(ids) != list(range(1, len(ids) + 1)):
            self.problems.append(
                f"{SCENARIOS}: scenario headings are S{sorted(ids)}, not S1 to S{len(ids)} once each"
            )
        return sorted(set(ids))

    def check_book(self, book: str) -> None:
        src = ROOT / book / "src"
        pages = sorted(p for p in src.rglob("*.md"))
        summary = (src / "SUMMARY.md").read_text(encoding="utf-8")
        listed = {src / t for t in re.findall(r"\]\(([^)#]+\.md)\)", summary)}
        for page in pages:
            rel = page.relative_to(ROOT).as_posix()
            if page.name != "SUMMARY.md" and page not in listed:
                self.problems.append(f"{rel}: not listed in SUMMARY.md")
            self.check_page(src, page, rel)

    def check_page(self, src: pathlib.Path, page: pathlib.Path, rel: str) -> None:
        raw = page.read_text(encoding="utf-8")
        for n, line in enumerate(raw.splitlines(), 1):
            if DASHES.search(line):
                self.problems.append(f"{rel}:{n}: em or en dash")
        prose = strip_fences(raw.splitlines())
        joined = ProseText(prose)
        no_code = INLINE_CODE.sub(lambda m: " " * len(m.group(0)), joined.text)
        for m in LINK.finditer(no_code):
            self.check_link(src, page, rel, joined.line_at(m.start()), m.group(1))
        for m in HTML_LINK.finditer(no_code):
            self.check_link(src, page, rel, joined.line_at(m.start()), m.group(1))
        refs = {}
        for m in REF_DEF.finditer(no_code):
            refs[m.group(1).lower()] = m.group(2)
            self.check_link(src, page, rel, joined.line_at(m.start()), m.group(2))
        for m in REF_USE.finditer(no_code):
            ref = (m.group(1) or "").lower()
            if ref and ref not in refs:
                self.problems.append(f"{rel}:{joined.line_at(m.start())}: undefined link reference: [{ref}]")
        for n, line in prose:
            for cited in CITED_PATH.findall(line):
                if not (ROOT / cited).exists():
                    self.problems.append(f"{rel}:{n}: cited file does not exist: {cited}")
        if rel != COVERAGE_MAP:
            labels = self.spec_labels()[0]
            for m in INLINE_CODE.finditer(joined.text):
                inner = " ".join(m.group(0)[1:-1].split())
                if QUOTED_LABEL.search(inner) and inner not in labels:
                    self.problems.append(
                        f"{rel}:{joined.line_at(m.start())}: quotes spec label `{inner}`, "
                        "which no Describe or It in test/e2e or test/upgrade carries"
                    )
        if rel not in TIME_ALLOWLIST:
            self.check_time(rel, prose)
        self.check_wording(rel, prose)

    def check_link(self, src: pathlib.Path, page: pathlib.Path, rel: str, n: int, target: str) -> None:
        blob = BLOB_URL.match(target)
        if blob:
            dest = ROOT / blob.group(1)
            if not dest.exists():
                self.problems.append(f"{rel}:{n}: link target does not exist: {target}")
            elif blob.group(2) and dest.suffix == ".md" and blob.group(2) not in self.anchors(dest):
                self.problems.append(f"{rel}:{n}: no heading for anchor: {target}")
            return
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
            if src not in dest.parents:
                self.problems.append(f"{rel}:{n}: link target leaves the book: {target}")
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

    def check_wording(self, rel: str, prose: list[tuple[int, str]]) -> None:
        for n, line in prose:
            hit = WORDING_RE.search(LINK_TARGET.sub("]()", INLINE_CODE.sub("", line)))
            if hit:
                self.ratchet_hits.append(f"{rel}:{n}: banned wording: {hit.group(0)!r}")

    def check_guide_figures(self) -> None:
        sources = ROOT / "guide/src/diagrams/SOURCES"
        listed = set()
        for name in sources.read_text(encoding="utf-8").splitlines():
            name = name.strip()
            if not name or name.startswith("#"):
                continue
            listed.add(name)
            src, copy = ROOT / "docs/src/diagrams" / name, ROOT / "guide/src/diagrams" / name
            if not src.exists():
                self.problems.append(f"guide/src/diagrams/SOURCES: no such figure in docs/src/diagrams: {name}")
            elif not copy.exists() or copy.read_bytes() != src.read_bytes():
                self.problems.append(f"guide/src/diagrams/{name}: missing or differs from the source (run make diagrams)")
        for svg in sorted((ROOT / "guide/src/diagrams").glob("*.svg")):
            if svg.name not in listed:
                self.problems.append(f"guide/src/diagrams/{svg.name}: not listed in SOURCES")
        for page in sorted((ROOT / "guide/src").rglob("*.md")):
            rel = page.relative_to(ROOT).as_posix()
            for n, line in strip_fences(page.read_text(encoding="utf-8").splitlines()):
                for name in GUIDE_EMBED.findall(line):
                    if name not in listed:
                        self.problems.append(f"{rel}:{n}: embeds {name}, which is not listed in guide/src/diagrams/SOURCES")

    def check_coverage_map(self) -> None:
        path = ROOT / COVERAGE_MAP
        text = path.read_text(encoding="utf-8")
        rows = re.findall(r"^\|\s*S(\d+)\b", text, re.MULTILINE)
        ids = sorted(int(r) for r in rows)
        expected = self.scenario_ids()
        if expected and ids != expected:
            self.problems.append(
                f"{COVERAGE_MAP}: rows are S{ids}, but {SCENARIOS} heads S{expected}; each scenario needs exactly one row"
            )
        labels, numbered, top = self.spec_labels()
        quoted: dict[int, set[str]] = {}
        for n, line in enumerate(text.splitlines(), 1):
            row = re.match(r"^\|\s*S(\d+)\b", line)
            if not row:
                continue
            sid = int(row.group(1))
            parts = line.split("|")
            if len(parts) < 4:
                self.problems.append(f"{COVERAGE_MAP}:{n}: S{sid} row does not have three cells")
                continue
            cell = parts[2]
            seen = quoted.setdefault(sid, set())
            for m in re.finditer(r"`([^`]*)`", cell):
                # A code span names a spec when it opens the cell, follows the
                # ' + ' joiner, or starts with a capital and holds a space. A
                # non-label span of that last form (`Authorization: Bearer`)
                # fails as a missing label; reword the cell to fix it.
                before = cell[: m.start()].strip()
                span = m.group(1)
                if not (before == "" or before.endswith("+") or (span[:1].isupper() and " " in span)):
                    continue
                seen.add(span)
                if span not in labels:
                    self.problems.append(
                        f"{COVERAGE_MAP}:{n}: S{sid} names spec `{span}`, "
                        "which no Describe or It in test/e2e or test/upgrade carries"
                    )
        for sid, label, describe, where in numbered:
            if sid not in quoted:
                self.problems.append(f'{where}: spec "{label}" carries S{sid}, which has no row in {COVERAGE_MAP}')
            elif label not in quoted[sid] and describe not in quoted[sid]:
                quotes = "does not quote it" if label == describe else f'quotes neither it nor its Describe "{describe}"'
                self.problems.append(f'{where}: spec "{label}" carries S{sid}, but the S{sid} row of {COVERAGE_MAP} {quotes}')
        mapped: set[str] = set().union(*quoted.values())
        for label, where in top:
            if label not in mapped and label not in NON_SCENARIO_SPECS:
                self.problems.append(
                    f'{where}: top-level spec "{label}" is quoted in no row of {COVERAGE_MAP} '
                    "and is not listed in NON_SCENARIO_SPECS in hack/docs/check.py"
                )
        top_labels = {label for label, _ in top}
        for name in NON_SCENARIO_SPECS:
            if name not in top_labels:
                self.problems.append(
                    f'hack/docs/check.py: NON_SCENARIO_SPECS lists "{name}", '
                    "which is not a top-level container (Describe, Context, "
                    "When, or DescribeTable) in test/e2e or test/upgrade"
                )

    def check_diagrams(self) -> None:
        result = subprocess.run(
            [sys.executable, str(ROOT / "hack/docs/diagram_check.py"), str(ROOT / "docs/src/diagrams")],
            capture_output=True, text=True,
        )
        for line in result.stdout.strip().splitlines():
            if line.startswith("ratchet: "):
                self.ratchet_hits.append(line[len("ratchet: "):])
            elif line.startswith("diagram_check:"):
                continue
            else:
                self.problems.append(line)


def main(argv: list[str]) -> int:
    time_report = "--time-report" in argv
    ratchet_report = "--ratchet-report" in argv
    checker = Checker(time_report, ratchet_report)
    for book in BOOKS:
        checker.check_book(book)
    checker.check_guide_figures()
    checker.check_coverage_map()
    checker.check_diagrams()
    for p in checker.problems:
        print(p)
    for h in checker.time_hits:
        print(h)
    for h in checker.ratchet_hits:
        print(h)
    failed = (
        len(checker.problems)
        + (0 if time_report else len(checker.time_hits))
        + (0 if ratchet_report else len(checker.ratchet_hits))
    )
    print(
        f"docs-check: {len(checker.problems)} problem(s), {len(checker.time_hits)} time-wording hit(s)"
        + (" (report only)" if time_report and checker.time_hits else "")
        + f", {len(checker.ratchet_hits)} ratchet hit(s)"
        + (" (report only)" if ratchet_report and checker.ratchet_hits else ""),
        file=sys.stderr,
    )
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
