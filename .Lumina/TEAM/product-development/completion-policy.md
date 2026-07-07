# Completion Policy

The Team Leader may complete the team run only when all required conditions are true:

- `task_complete=true` by Team Leader judgement.
- A Team Acceptance Contract has been recorded with `RecordTeamContract`.
- QA verdict has been submitted with `SubmitGateVerdict` and is `pass` or `not_applicable`.
- Reviewer verdict has been submitted with `SubmitGateVerdict` and is `pass`, or `accepted_with_notes` with no unresolved blocking findings.
- Reviewer `accepted_with_notes` is allowed only for explicitly non-blocking suggestions. It is not allowed for architecture mismatch, missing component integration, skipped user requirement, skipped required build/test command, build-breaking, security, data-loss, or correctness issues.
- Non-blocking QA or Reviewer findings are not ignored in this Team. The Team Leader must either dispatch a follow-up task to fix them and regate, or include a concrete `deferral_reasons` entry in `CompleteTeamTask` explaining why each finding is intentionally deferred. Accepted deferral keys are `Reviewer:<category>:<summary>`, `QA:<category>:<summary>`, `<category>:<summary>`, or `<summary>`.
- Any repair after a QA failure, Reviewer rejection, or blocking `accepted_with_notes` finding has been re-verified by QA and re-reviewed by Reviewer.
- For software projects, all relevant declared build/check/test commands pass, and all contract-required build/check/test commands pass. Examples include `npm run build` when `package.json` defines `build`, `npm test` when it defines `test`, `go test ./...` for Go modules, and integration smoke tests for user-facing CLIs/services.
- For multi-component projects, integration behavior required by the contract has been smoke-tested. For example, a TypeScript CLI paired with a Go backend must exercise the Go backend/API unless the user explicitly requested direct-file or independent implementations.
- For named-project tasks, the parent working directory remains clean. Verification must not leave `.lumina`, `.Lumina`, `data/`, server binaries, smoke scripts, logs, or Team/Lumina runtime files outside the named project root.
- Required artifacts named in the final call exist in the Team artifact index.
- No active required task remains running, queued, or blocked.
- The final answer is ready for the user.

If any condition is false, continue the loop by dispatching more work, requesting recovery, or asking the user for the smallest necessary clarification.
