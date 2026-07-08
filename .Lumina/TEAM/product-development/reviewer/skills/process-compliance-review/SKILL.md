---
name: Process Compliance Review
description: Review whether the product-development process and required artifacts were actually followed.
when-to-use: Use during Reviewer gate before submitting a verdict.
user-invocable: false
context: inline
---

Review the work as a process and evidence audit, not just a code review.

Before reviewing:

- Call `GetTeamContext`.
- Use `GetTeamContext.contract` as the authoritative Team Acceptance Contract.
- A standalone `TEAM_ACCEPTANCE_CONTRACT.md` is optional unless the runtime contract lists it in `required_artifacts`.

Required checks:

- `PRD.md` exists and matches the user request.
- `UX_DESIGN.md` exists and is based on the PRD.
- `BACKEND_PLAN.md` and `FRONTEND_PLAN.md` exist and stay within their boundaries.
- `INTERFACE_CONTRACT.md` exists and matches the implementation.
- `INTEGRATION_REPORT.md` exists and proves real component integration.
- `QA_REPORT.md` exists and includes required command evidence.
- Implementation files are under the project root.
- The parent working directory is clean for named-project tasks.
- QA and Reviewer gate verdicts are submitted through runtime tools, not only described in prose.
- Reviewer evidence collection stays within expected artifacts. A reviewer-created smoke script or helper file is a process violation unless it was declared as an expected artifact before the review task started.

Blocking findings:

- Missing required document artifact.
- Missing runtime Team Acceptance Contract when the team gates require one.
- A contract document is listed in `required_artifacts` but is absent from both runtime artifacts and disk.
- Any unresolved mismatch with an explicit user requirement, PRD acceptance criterion, UX requirement, or runtime contract item.
- Frontend bypasses backend/API/storage contract.
- Backend implements unused or incompatible contract.
- Required verification command is skipped without a specific reason.
- Security, data-loss, correctness, build-breaking, or architecture mismatch risk.

Write review evidence to `<project_root>/REVIEW_REPORT.md` and call `SubmitGateVerdict`.
