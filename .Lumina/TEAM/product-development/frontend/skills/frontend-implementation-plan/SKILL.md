---
name: Frontend Implementation Plan
description: Plan user-facing state, rendering, interaction, and integration changes for any interface stack.
when-to-use: Use before implementing web, mobile, desktop, CLI, TUI, plugin, or documentation-driven product surface changes.
user-invocable: false
context: inline
---

Write the plan as `<project_root>/FRONTEND_PLAN.md`.

Size guidance:

- Small CLI/TUI/single-screen work: 60-140 lines.
- Medium feature: 120-220 lines.
- Large app or multi-platform work: expand only where the design/contract requires it.
- Keep the plan actionable: files, state, command/view structure, contract consumption, and tests. Do not restate the whole PRD or UX document.

Inputs:

- `PRD.md`
- `UX_DESIGN.md`
- `INTERFACE_CONTRACT.md` or an A2A contract draft that must be persisted.
- Existing frontend code/conventions, if any.

Required sections:

1. Surface and stack
   - Web/mobile/desktop/CLI/TUI/docs/plugin surface.
   - Existing conventions, files, framework, or no-framework choice.
   - Accessibility and platform constraints.

2. User flows and states
   - Happy path.
   - Loading, empty, validation error, server/network error, success, disabled/submitting state.
   - Focus, keyboard, and screen-reader behavior when relevant.

3. Structure
   - Pages/routes/screens/components or command/view layout.
   - File structure.
   - Ownership of local state and derived display state.

4. Contract consumption
   - Exact backend/API/storage/tooling contract consumed.
   - Request/response mapping.
   - Error mapping.
   - For CLI/TUI/local tools with persistence, how subprocesses receive isolated storage during integration tests: environment variable, current working directory convention, config file, fixture path, or command option from the contract.
   - Why the implementation is not direct-file, mock-only, or independent.

5. Implementation steps
   - Ordered steps that fit frontend ownership.
   - What must wait for backend/contract confirmation.
   - Integration points to verify with Backend.
   - Verification helpers must stay inside expected artifacts. Do not create temporary scripts or extra test files during implementation unless Team Leader included them in expected artifacts.

6. Test and smoke plan
   - Unit or simple logic checks.
   - Integration smoke against real backend/API.
   - Subprocess-based tests must use mechanisms visible to the subprocess. Do not rely on monkeypatching in-process module globals to isolate storage or backend state.
   - Do not accept a backend function parameter as subprocess isolation unless the user-facing CLI/API exposes it through the durable contract.
   - If `INTERFACE_CONTRACT.md` does not define a subprocess-visible storage override for a local persistent tool, stop and ask Team Leader/Backend to fix the contract instead of inventing test-only monkeypatches.
   - Manual UX checks.
   - Accessibility checks.

7. Risks and assumptions
   - Missing design/contract details.
   - Browser/platform compatibility.
   - Performance or data-volume concerns.

Do not include backend internals except for the contract you consume. If the contract is not durable, request or create `INTERFACE_CONTRACT.md` before implementation.
