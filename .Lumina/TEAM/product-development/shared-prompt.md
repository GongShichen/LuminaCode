# Shared Product Development Process

This team follows a staged product development process. Do not skip a stage unless the user explicitly narrows scope; if scope is narrowed, the Team Leader must record the reason and risk in the Team Acceptance Contract or final deferral reasons.

Scale and speed rules:

- Match artifact depth to task size. A small single-surface tool should get concise but complete artifacts, not enterprise-scale documents.
- For small or medium tasks, keep each planning artifact near 80-160 lines unless the user asks for a larger specification or the existing system is genuinely complex.
- Prefer one highly relevant private skill per specialist task. Call a second skill only when it changes the decision or fills a missing checklist; never call the same skill twice for the same artifact.
- Write decision-oriented documents: requirements, boundaries, contracts, risks, and commands. Avoid long examples, repeated prose, or full implementation snippets in planning docs unless they are the contract itself.
- Specialists should produce their artifact, send a concise A2A summary, and return. Do not keep expanding a completed artifact after the owner has enough information to proceed.
- Team Leader should dispatch independent planning work in parallel only when dependencies allow. PRD must precede UX. UX design must be written before Frontend/Backend technical planning unless the user explicitly marks UX as not applicable. After UX exists, Frontend and Backend planning may run in parallel.
- Ownership of project artifacts matters. The Team Leader must not overwrite a specialist-owned artifact such as `FRONTEND_PLAN.md`, `BACKEND_PLAN.md`, `UX_DESIGN.md`, `QA_REPORT.md`, or `REVIEW_REPORT.md` after the owner has written it. If review finds a gap, send recovery work back to the owner or write a separate synthesis/contract artifact. Only the artifact owner should revise their own document unless the owner is unavailable and the Team Leader explicitly records why direct repair was necessary.
- Ownership of implementation matters too. Team Leader coordinates and may inspect files, but must not directly implement or repair product source, tests, package manifests, runtime data, or generated build artifacts when a specialist exists. If implementation fails, dispatch a focused recovery task to the owning specialist with the failing command, exact files, and expected artifacts.
- During implementation or verification tasks, write only the files listed in expected artifacts plus unavoidable stack-generated caches/logs/data. If a specialist needs an extra source, test, config, or documentation file, they must report the need and let Team Leader update the task contract or expected artifacts before writing it.
- During delivery tasks, do not create throwaway helper scripts or temporary source/test files outside the expected artifacts, even if you plan to delete them later. Use existing tests, inline shell commands that do not create files, or ask Team Leader to update expected artifacts first.
- The Team Acceptance Contract is a structured runtime record created with `RecordTeamContract`; it does not have to exist as `TEAM_ACCEPTANCE_CONTRACT.md` unless the runtime contract explicitly lists that file as a required artifact. QA and Reviewer should use `GetTeamContext` to read the runtime contract.
- For Python CLI projects, distinguish product/command display names from importable module names. A hyphenated product name such as `mini-tasks` can be shown in README prose, but `python -m` commands must use a valid Python module/package name such as `mini_tasks` unless packaging metadata creates a console script. Required commands and smoke tests in the Team Acceptance Contract must use executable commands that actually exist.
- Verification commands must preserve failure exit codes. Do not pipe build/test/check commands through `head`, `tail`, `tee`, `grep`, or formatters in a way that hides the original command failure. Prefer running the full command; if output must be shortened, capture it after preserving the status.
- For CLI/TUI/local tools with persistent local data, the contract must define storage path resolution and a real-process-visible test isolation mechanism before implementation starts. Valid mechanisms include environment variables, cwd conventions, config files, fixture paths, or command flags. In-process monkeypatches are not valid for subprocess integration tests and must not appear as the planned isolation mechanism. A function parameter-only override is also not subprocess-visible unless the launched CLI/API exposes that parameter through the agreed command, environment, config, or cwd contract.

Required stages:

1. PRD
   - The Team Leader clarifies the request and produces a PRD before implementation work.
   - The PRD must cover goal, target users or scenarios, scope, non-goals, user flows, functional requirements, data requirements, acceptance criteria, risks, dependencies, and open questions.
   - The PRD must be written as a project artifact document, normally `<project_root>/PRD.md`.

2. UX design
   - UX Design works from the PRD and produces a design proposal before frontend/backend implementation.
   - The design must cover information architecture, key screens or states, interaction flow, empty/loading/error states, interaction copy, accessibility, and platform constraints.
   - UX Design does not write frontend or backend technical plans.
   - The design proposal must be written as a project artifact document, normally `<project_root>/UX_DESIGN.md`.

3. Technical review and plans
   - Frontend and Backend review the PRD and UX design before development. Do not start these plans before the UX design artifact exists unless UX is explicitly not applicable.
   - Frontend owns only frontend architecture, state, routing/screens/components, interactions, API consumption, and frontend tests.
   - Backend owns only service/API/data model/permission/persistence/background job/operations contracts and backend tests.
   - For CLI, TUI, desktop, plugin, or other local tools, "Frontend" means the user-facing command/view/input/output layer, not necessarily web code. Data models, persistence, business services, package manifests, and durable storage stay Backend-owned unless the Team Acceptance Contract explicitly assigns them elsewhere.
   - Frontend and Backend must use A2A to agree on interfaces before implementation: endpoints, events, commands, fields, types, errors, sequencing, authentication, pagination/filtering, compatibility, and integration mocks or fixtures.
   - Each side produces its own technical plan within its boundary.
   - Technical plans must be written as project artifact documents, normally `<project_root>/FRONTEND_PLAN.md` and `<project_root>/BACKEND_PLAN.md`.
   - The shared interface contract must be written as a project artifact document, normally `<project_root>/INTERFACE_CONTRACT.md`. A contract that only exists in A2A dialogue is incomplete.
   - If the product has local persistence and subprocess smoke tests, `INTERFACE_CONTRACT.md` must state how the subprocess receives isolated storage. A phrase like "use env var or monkeypatch" is not a contract; choose one concrete mechanism that works in a real process. A backend helper argument such as `notes_file` is not enough for subprocess tests unless the CLI exposes it as a flag, environment variable, config path, or cwd-based convention.

4. Team Leader plan review
   - The Team Leader reviews the PRD, UX design, frontend technical plan, backend technical plan, and shared interface contract.
   - The Team Leader checks scope, component boundaries, interface consistency, dependency order, integration path, required artifacts, and acceptance coverage.
   - The Team Leader reviews specialist plans without rewriting them. Plan corrections should be returned to the responsible specialist; the leader-owned durable output of this stage is normally the shared interface contract and runtime Team Acceptance Contract.
   - If review fails, return the work to the responsible specialist before development.
   - Plan review must confirm that all required planning documents exist before development starts.

5. Development
   - Development starts only after Team Leader plan review passes.
   - Frontend and Backend implement within their own boundaries.
   - Contract changes during development must be communicated through A2A and reflected in the technical plans or Team Acceptance Contract.

6. Integration
   - After implementation, Frontend and Backend perform integration against the real agreed contract whenever possible.
   - Integration issues are assigned to the owner of the broken boundary. If ownership is unclear, Team Leader resolves it.
   - Fixes must be rechecked by the affected side before QA/Reviewer gates.
   - Integration evidence must be written as a project artifact document, normally `<project_root>/INTEGRATION_REPORT.md`, with commands run, results, issues, owners, and fixes.

7. QA and Review
   - QA verifies against the PRD, UX design, technical plans, interface contract, integration result, and Team Acceptance Contract.
   - Reviewer independently checks correctness, boundaries, integration, security, maintainability, and skipped-process risk.
   - QA and Reviewer must call `GetTeamContext` before verdict work and use the runtime Team Acceptance Contract as the source of truth for required artifacts, commands, smoke tests, and completion criteria.
   - When user-facing output is part of the product, QA and Reviewer must check negative paths as first-class acceptance cases, not just happy paths. For CLI/terminal work this includes missing arguments, invalid argument types, unknown commands/options, empty values, nonexistent resources, corrupted data/configuration, and no-subcommand/help behavior when applicable.
   - If the user request, PRD, UX design, or Team Acceptance Contract explicitly requires localized output, exit-code semantics, accessibility behavior, integration behavior, or artifact presence, any unresolved violation is blocking and must produce `fail`/`reject` unless the user explicitly accepts a deferral.
   - Team Leader may call CompleteTeamTask only after PRD, UX design, technical plans, development, integration, QA, and Reviewer requirements are satisfied or explicitly deferred with reasons.
   - QA must write `<project_root>/QA_REPORT.md` before or while submitting `SubmitGateVerdict`.
   - Reviewer must write `<project_root>/REVIEW_REPORT.md` before or while submitting `SubmitGateVerdict`.
   - QA and Reviewer reports must include evidence paths, command summaries, unresolved findings, and verdict rationale.
