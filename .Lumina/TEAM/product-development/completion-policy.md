# Completion Policy

The Team Leader may complete the team run only when all required conditions are true:

- `task_complete=true` by Team Leader judgement.
- A Team Acceptance Contract has been recorded with `RecordTeamContract`.
- QA verdict has been submitted with `SubmitGateVerdict` and is `pass` or `not_applicable`.
- Reviewer verdict has been submitted with `SubmitGateVerdict` and is `pass`, or `accepted_with_notes` with no unresolved blocking findings.
- Reviewer `accepted_with_notes` is allowed only for explicitly non-blocking suggestions. It is not allowed for architecture mismatch, missing component integration, skipped user requirement, skipped required build/test command, build-breaking, security, data-loss, or correctness issues.
- Non-blocking QA or Reviewer findings are not ignored in this Team. The Team Leader must either dispatch a follow-up task to fix them and regate, or include a concrete `deferral_reasons` entry in `CompleteTeamTask` explaining why each finding is intentionally deferred. Accepted deferral keys are `Reviewer:<category>:<summary>`, `QA:<category>:<summary>`, `<category>:<summary>`, or `<summary>`.
- Any repair after a QA failure, Reviewer rejection, or blocking `accepted_with_notes` finding has been re-verified by QA and re-reviewed by Reviewer.
- For product development work, PRD, UX design, frontend/backend technical plans, Team Leader plan review, implementation, integration, QA, and Reviewer gates are complete or explicitly marked not applicable with reasons.
- For ordinary software product work, the project root contains the required stage documents: `PRD.md`, `UX_DESIGN.md`, `BACKEND_PLAN.md`, `FRONTEND_PLAN.md`, `INTERFACE_CONTRACT.md`, `INTEGRATION_REPORT.md`, `QA_REPORT.md`, and `REVIEW_REPORT.md`, unless the Team Acceptance Contract marks a document not applicable with a concrete reason.
- A2A dialogue does not count as a replacement for required stage documents. If a contract, integration result, QA verdict rationale, or Reviewer verdict rationale exists only in dialogue, the work is incomplete.
- For software projects, all relevant declared build/check/test commands pass, and all contract-required build/check/test commands pass. Discover commands from project manifests and conventions instead of assuming a language or framework.
- For multi-component projects, integration behavior required by the contract has been smoke-tested. A user-facing app, CLI, workflow, or service must exercise the agreed backend/API/storage/tooling contract unless the user explicitly requested direct-file or independent implementations.
- For named-project tasks, the parent working directory remains clean. Verification must not leave agent runtime directories, generated data, build outputs, server binaries, smoke scripts, logs, or verification byproducts outside the named project root.
- Required artifacts named in the final call exist in the Team artifact index.
- No active required task remains running, queued, or blocked.
- The final answer is ready for the user.

If any condition is false, continue the loop by dispatching more work, requesting recovery, or asking the user for the smallest necessary clarification.
