# Audit re-walk brief (release <RELEASE>)

This is a template for the read-only agent that re-walks drifted verdicts of
the Kaalm listener authorization and SSRF audit. Fill in the placeholders, then
give the agent the filled-in text.

| Placeholder | Value |
|---|---|
| `<RELEASE>` | The version being released, such as `v2.1.0`. |
| `<AUDITED_COMMIT>` | The `# audited-commit:` value in `hack/audit/manifest.txt`. |
| `<HEAD>` | The commit to walk: the release PR's head, or `main` at release time. |
| `<REPORT_PATH>` | Where the agent writes its report. |
| `<VERDICTS>` | The verdict ids this agent re-walks, from the output of `make audit-drift`. |

---

You are re-walking verdicts of the Kaalm listener authorization and SSRF audit
against `<HEAD>`, because the files each verdict examined changed since
`<AUDITED_COMMIT>`. Your verdicts are `<VERDICTS>`. The work is read-only.

## Inputs, all in the repository

- `hack/audit/checklist.md`: the audit checklist. Each numbered verdict
  answers one checklist item.
- `hack/audit/verdicts.md`: the standing verdicts and corrections.
- `hack/audit/manifest.txt`: each verdict id, then the files it examined.
- The output of `make audit-drift`, run at `<HEAD>`: which verdicts drifted
  and which files changed.
- `docs/src/security/threat-model.md`: the accepted reasoning lives there.

Optional background: the original walk's reasoning per verdict is in
[a comment on the audit issue](https://github.com/win07xp/kaalm/issues/139).
Its line numbers come from an old commit; ignore them.

## Method, per verdict

1. Read the checklist item and the verdict's line in `verdicts.md`.
2. Read the change: `git diff <AUDITED_COMMIT> <HEAD> -- <the verdict's files>`
   and `git log --oneline <AUDITED_COMMIT>..<HEAD> -- <the verdict's files>`.
3. Read the current code paths the verdict covers, not only the diff. The
   question is whether the verdict's claim still holds in the code as it is
   now.
4. Decide:
   - **HOLDS:** the claim is still true. Say why the changes do not affect
     it, or how they strengthen it.
   - **FINDING:** the claim no longer holds. Give the file, the function, a
     concrete request or input that shows it, and the severity.
   - **NEEDS-DOCS:** the code is sound, but the threat model or a design page
     now says something false.
5. Note any new listener path, dial, or trust decision the changes added
   inside your verdicts' files that the checklist does not cover yet. Walk it
   the same way and report it as NEW-SURFACE with a verdict.

Verdicts that did not drift still stand. Skip them.

## Rules

- Work read-only. Do not edit, commit, push, open issues or PRs, or run
  `kubectl`, `helm install`, `k3d`, `docker`, or `make e2e`. `go test` and
  `go build` are fine.
- Be concrete. A FINDING needs a failure scenario a maintainer can reproduce.
  Do not report style or hypothetical hardening as findings. Put optional
  hardening ideas in a separate "Notes" list.
- Use no em dashes or en dashes in the report.

## Report

Write the report to `<REPORT_PATH>` as markdown: one section per verdict with
the verdict id, the decision, the evidence (files and functions), and for a
FINDING the scenario and the severity. End with a summary table.

Return only a short summary: the count per decision, and one line per FINDING
or NEW-SURFACE.
