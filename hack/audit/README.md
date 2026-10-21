# Listener authorization and SSRF audit

This directory holds the audit of Kaalm's listeners (who may call each one) and outbound dials (who supplies each URL, and what constrains the dial), and the tools that keep the audit current. [RELEASING.md](../../RELEASING.md) runs the re-walk before each release.

- `manifest.txt` is the machine-readable record. Each line names a verdict and the files it examined. The `# audited-commit:` line names the commit at which every verdict held.
- `drift-check.sh` reads the manifest and diffs each verdict's files from the audited commit to `HEAD`. A verdict with a non-empty diff has drifted and needs a re-walk. Run it with `make audit-drift`. CI runs it without blocking the build, because drift between releases is expected.
- `checklist.md` lists what each verdict checks. A re-walk walks a drifted verdict against its checklist item.
- `brief.md` is the template for the read-only agent that does the re-walk. Fill in its placeholders, then give the agent the text and the drifted verdict ids.
- `verdicts.md` is the human-readable record: each verdict's result, the findings behind it, and the corrections. It is the human half of the manifest.

At release time, run `make audit-drift`. Re-walk each drifted verdict with `brief.md` and `checklist.md`. In the release PR, move the `# audited-commit:` line in `manifest.txt`, add any newly examined files to the verdict's line, and add a line to `verdicts.md`. The accepted reasoning for each item lives in the [threat model](../../docs/src/security/threat-model.md).
