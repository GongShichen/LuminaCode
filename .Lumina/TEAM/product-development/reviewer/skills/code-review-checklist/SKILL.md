---
name: Code Review Checklist
description: Review implementation changes for correctness and maintainability.
when-to-use: Use for Reviewer gate on code changes.
user-invocable: false
context: inline
---

Use this checklist to write or contribute to `<project_root>/REVIEW_REPORT.md`.

Review dimensions:

1. Requirements and process
   - User request is satisfied.
   - PRD, UX design, backend plan, frontend plan, interface contract, integration report, QA report exist when applicable.
   - Team Acceptance Contract matches the delivered architecture.

2. Integration correctness
   - User-facing component consumes the real backend/API/storage/tooling contract.
   - No direct-file shortcut or mock-only substitute unless explicitly allowed.
   - Contract fields, validation, errors, and ordering match implementation.

3. Code correctness
   - Main happy path works.
   - Edge cases and validation behave predictably.
   - Persistence, cleanup, and restart behavior are reasonable.
   - No obvious race, partial-write, stale-state, or data-loss issue.
   - CLI/terminal products cover parser-level errors as well as business errors: missing arguments, invalid argument types, unknown commands/options, empty input, nonexistent resources, and corrupted data/configuration.
   - User-visible stdout/stderr, help text, and error text obey the requested locale and wording contract when localization is part of the user request, PRD, UX design, or Team Acceptance Contract.

4. Security and privacy
   - Secrets are not logged or committed.
   - User input is handled safely for the stack.
   - Trust boundaries, file paths, local ports, and permissions are appropriate.

5. Maintainability
   - Code follows existing project conventions or a justified simple local pattern.
   - No unnecessary dependencies.
   - Names and file structure are understandable.
   - Errors and comments help future maintenance.

6. QA evidence quality
   - Required commands were actually run.
   - Required commands are valid for the delivered stack. For Python CLI/package work, `python -m <name>` must use an importable module/package name, not a hyphenated display name unless packaging defines that entrypoint.
   - Test/build evidence preserves the failing command's exit status; pipelines that truncate output must not hide failures.
   - Integration smoke proves the real contract.
   - Parent workspace cleanliness was checked.
   - Review verification must not create undeclared helper scripts or temporary source/test files. Use existing tests and product commands, or ask Team Leader to add an expected artifact before writing a helper.

Finding format:

- Severity: blocking, non-blocking, note.
- Category.
- File/path or artifact.
- Evidence.
- Consequence.
- Recommended fix.

Verdict policy:

- `reject` for unresolved blocking issues.
- Treat unresolved violations of explicit user requirements, PRD acceptance criteria, UX requirements, or runtime contract items as blocking.
- `accepted_with_notes` only for truly non-blocking residuals.
- `pass` only when process, integration, correctness, and evidence are sufficient.
