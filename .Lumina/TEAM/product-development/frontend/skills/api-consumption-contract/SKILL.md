---
name: API Consumption Contract
description: Verify and document how the user-facing surface consumes the agreed backend/API contract.
when-to-use: Use before frontend implementation and during integration with a backend/API/service.
user-invocable: false
context: inline
---

Use this skill to keep frontend work from drifting into direct-file, mock-only, or independent behavior.

Required contract artifacts:

- Read `INTERFACE_CONTRACT.md` before implementation when it exists.
- If the contract was only sent through A2A, collaborate with Backend to write `<project_root>/INTERFACE_CONTRACT.md`.
- Keep frontend assumptions in `FRONTEND_PLAN.md` aligned with the contract.

Frontend consumption checklist:

1. Data loading
   - Exact endpoint/command/event used.
   - Request method, headers, body shape, and timeout/retry behavior.
   - Loading, empty, and error UI states.

2. Data mutation
   - Validation before request.
   - Payload shape.
   - Success update behavior.
   - Failure behavior that preserves user input when appropriate.

3. Rendering
   - Field mapping from contract to UI.
   - Date/number/status formatting.
   - Missing/null/unknown value behavior.

4. Integration evidence
   - Smoke command or browser/manual steps proving real backend consumption.
   - For subprocess/CLI tests, storage fixtures and overrides must be visible to the spawned process through the agreed contract. Use env vars, cwd, config files, flags, or fixture paths; do not rely on in-process monkeypatches that the subprocess cannot see.
   - Evidence to add to `INTEGRATION_REPORT.md`.

5. Boundaries
   - Do not directly read/write backend persistence files.
   - Do not hardcode fixture data except for explicitly marked demos/tests.
   - Do not redesign backend internals.
