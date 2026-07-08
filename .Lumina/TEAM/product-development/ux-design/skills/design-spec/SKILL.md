---
name: Design Spec
description: Convert a PRD into a durable UX design document with states, flows, copy, and accessibility.
when-to-use: Use when UX Design is asked to produce or revise product-facing design.
user-invocable: false
context: inline
---

Write the design as `<project_root>/UX_DESIGN.md` unless Team Leader gives a different path.

Size guidance:

- Small CLI, TUI, single-screen, or narrow workflow: 60-140 lines.
- Medium multi-screen feature: 120-220 lines.
- Large product surface: expand only when the PRD or existing system requires it.
- Prefer compact tables and bullet decisions over long prose. Do not include every possible state if it is not relevant.

Inputs:

- PRD path and summary.
- Platform constraints and existing UI conventions.
- Known backend/data contract assumptions.

Required sections:

1. Design objective
   - What user workflow this design optimizes.
   - What is intentionally not designed.

2. Information architecture
   - Primary regions, navigation/routing, hierarchy, and reading order.
   - Responsive or terminal/window-size behavior when relevant.

3. User flows
   - Happy path.
   - Empty/loading/error flow.
   - Retry/recovery flow.
   - Keyboard and accessibility flow.

4. Screen/state inventory
   - Initial state.
   - Loading state.
   - Empty state.
   - Populated state.
   - Validation error.
   - Network/server error.
   - Success confirmation.

5. Interaction and copy
   - Labels, buttons, status text, empty/error messages.
   - Focus behavior and live-region/announcement behavior.

6. Data needs
   - Fields consumed from backend/API/storage.
   - Formatting decisions for dates, status, counts, names, and validation.
   - What the design expects but does not define as backend internals.

7. Accessibility and constraints
   - Semantic structure, labels, keyboard access, focus, color contrast, reduced motion.
   - Platform constraints and no-go decisions.

8. Handoff notes
   - Frontend implications.
   - Backend/API assumptions that must be confirmed through `INTERFACE_CONTRACT.md`.

Quality bar:

- Do not produce frontend or backend technical plans.
- Do not let visual polish override repeated-use ergonomics.
- Do not invent backend internals; identify contract needs instead.
- Make missing PRD details explicit as assumptions or questions.
