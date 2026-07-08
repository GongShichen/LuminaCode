---
name: QA Report
description: Write a durable QA report with evidence, command results, unresolved findings, and verdict rationale.
when-to-use: Use before submitting a QA gate verdict.
user-invocable: false
context: inline
---

Write QA evidence as `<project_root>/QA_REPORT.md`.

Before writing:

- Call `GetTeamContext` and use its runtime `contract` as the authoritative Team Acceptance Contract.
- Do not invent a missing-contract finding because there is no `TEAM_ACCEPTANCE_CONTRACT.md` file. A file is required only if the runtime contract lists that file in `required_artifacts`.
- Keep the report concise enough for repair: normally 80-180 lines for small/medium tasks.

Required sections:

1. Scope verified
   - User request summary.
   - Runtime Team Acceptance Contract summary from `GetTeamContext`.
   - Documents checked: PRD, UX design, plans, interface contract, integration report.

2. Environment
   - Working directory.
   - Runtime/tool versions when relevant.
   - Setup commands and services started.

3. Command evidence
   - Each required command from the Team Acceptance Contract.
   - Exit status.
   - Short stdout/stderr summary.
   - Whether output proves the requirement.

4. Integration evidence
   - Real user-facing component path exercised.
   - Backend/API/storage/tooling contract exercised.
   - Positive, negative, and persistence checks where applicable.

5. Parent workspace cleanliness
   - Files present in the parent directory before/after when named-project work is used.
   - Any byproducts and cleanup performed.

6. Findings
   - Blocking findings.
   - Non-blocking findings.
   - Deferred items and why they are safe.
   - Any failure of an explicit user requirement, PRD acceptance criterion, UX requirement, or runtime contract item belongs in blocking findings. Examples include localized-error requirements, required exit-code semantics, required integration behavior, or required artifacts.
   - Do not classify an explicit requirement violation as non-blocking just because the happy path works or the issue is common in a framework.

7. Verdict
   - `pass`, `fail`, or `not_applicable`.
   - Rationale and evidence references.

After writing the report, call `SubmitGateVerdict` and reference `QA_REPORT.md` in the evidence.
