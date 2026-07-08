---
name: Task Breakdown
description: Build and maintain a team task graph with owners, dependencies, artifacts, and gates.
when-to-use: Use when the Team Leader needs to plan or re-plan a team run.
user-invocable: false
context: inline
---

Create a task graph that can actually drive the Team Loop.

Inputs:

- User request and current working directory.
- Existing repository/project structure.
- Required product-development stages from the shared prompt.
- Current dialogue, active A2A tasks, gate status, and known artifacts.

Task graph structure:

1. Objective
   - One-sentence user outcome.
   - Completion criteria in observable terms.
   - Non-goals and explicitly deferred scope.

2. Project/artifact root
   - If the user asks for a new named project or directory and gives no absolute path, set the root to `<current working directory>/<requested name>`.
   - All owner tasks, file paths, README paths, tests, and gate checks must use that root.
   - Add a parent-workspace-clean check for named projects.
   - For Python projects, decide both the product/display name and the importable package/module name. Hyphenated names such as `mini-tasks` are product names, not valid `python -m` module names. Unless packaging creates a console script, contract commands must use the importable name such as `python -m mini_tasks`.
   - For CLI/TUI/local tools with persistent data, decide how runtime data paths are resolved and how subprocess tests isolate storage. Pick one concrete mechanism visible to real processes, such as an environment variable, cwd convention, config file, fixture path, or command flag. Do not list monkeypatch as a subprocess integration mechanism.

3. Required stage artifacts
   - PRD: `PRD.md`.
   - UX design: `UX_DESIGN.md`.
   - Backend plan: `BACKEND_PLAN.md`.
   - Frontend plan: `FRONTEND_PLAN.md`.
   - Shared interface contract: `INTERFACE_CONTRACT.md`.
   - Integration evidence: `INTEGRATION_REPORT.md`.
   - QA report: `QA_REPORT.md`.
   - Reviewer report: `REVIEW_REPORT.md`.

4. Components and ownership
   - Frontend/user-facing surface owner.
   - Backend/platform/data/integration owner.
   - For CLI/TUI/local-tool products, assign command parsing, output formatting, help/error copy, and interaction behavior to Frontend; assign data model, business services, persistence, package manifests, storage paths, and backend tests to Backend.
   - UX owner.
   - QA and Reviewer gates.
   - DevOps or Research support only when needed.

5. Dependency order
   - PRD before UX.
   - UX before frontend/backend technical plans.
   - Technical plans and interface contract before implementation.
   - For persistent local tools, the interface contract must include storage path resolution and subprocess-visible test isolation before implementation.
   - Implementation before integration.
   - Integration before QA/Reviewer pass.
   - QA/Reviewer findings before final completion.

6. Dispatch packets
   - Each specialist task must include project root, input document paths, expected output document path, constraints, and acceptance evidence.
   - Avoid assigning one task that crosses role boundaries; split it.
   - If a target already has a running/pending A2A task, do not duplicate the task.
   - Required commands must be runnable as written and preserve exit codes. Do not put `| head`, `| tail`, or other output truncation in acceptance commands unless the status is explicitly preserved.

7. Recovery path
   - Missing artifact: dispatch the owner to write it.
   - Wrong root: dispatch cleanup/move work.
   - Contract mismatch: dispatch Frontend and Backend to reconcile `INTERFACE_CONTRACT.md`.
   - Implementation/test failure: identify the owning boundary, then dispatch a narrow repair task to that specialist. Team Leader should not edit product source, tests, package manifests, runtime data, or generated build artifacts directly.
   - QA fail or Reviewer reject: dispatch concrete repair tasks to the owner, then regate.

Clarification policy:

- Ask the user only when a decision cannot be inferred safely from the request, repository, or reasonable product defaults.
- Record assumptions in PRD or Team Acceptance Contract when proceeding.
