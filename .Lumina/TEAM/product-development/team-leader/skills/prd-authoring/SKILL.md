---
name: PRD Authoring
description: Produce a product requirements document that can drive design, engineering, QA, and review.
when-to-use: Use at the start of product development work before UX, technical planning, or implementation.
user-invocable: false
context: inline
---

Write the PRD as a durable project artifact, normally `<project_root>/PRD.md`.

Inputs:

- User request and any clarified constraints.
- Current working directory and inferred project root.
- Existing product/repository context when present.
- Known time, platform, dependency, data, privacy, and operational constraints.

Required structure:

1. Title and status
   - Product/workstream name.
   - Authoring date and status: draft, ready for design, revised, or deferred.

2. Problem and goal
   - Problem statement in user terms.
   - Primary goal and success signal.
   - Non-goals that prevent scope creep.

3. Users and scenarios
   - Target users or roles.
   - Core scenario and at least one failure or edge scenario.

4. Scope
   - In scope.
   - Out of scope.
   - Explicit assumptions.

5. User flows
   - Happy path.
   - Empty/loading/error path.
   - Recovery path when relevant.

6. Functional requirements
   - Numbered requirements with observable behavior.
   - Required data, persistence, integration, and validation behavior.
   - Requirements that must be verified by QA.

7. Interface and data requirements
   - Components that must integrate.
   - Data fields, ownership, lifecycle, and privacy/security considerations.
   - Any known API, event, file, command, or storage boundary.

8. Acceptance criteria
   - User-visible acceptance criteria.
   - Required artifacts and commands.
   - Integration smoke expectations.

9. Risks, dependencies, and open questions
   - Risks with owner or mitigation.
   - External dependencies.
   - Questions that block work, and questions that can proceed with assumptions.

Quality bar:

- The PRD must be concrete enough for UX Design to produce a design without inventing product scope.
- It must be concrete enough for Frontend and Backend to produce separate technical plans.
- It must avoid implementation choices unless the user or repository already constrains them.
- It must preserve the user's architecture; do not simplify multi-component work into one direct-file shortcut.

Handoff:

- Send UX Design an A2A message with the PRD path and a short summary.
- Keep the PRD path in Team dialogue and the Team Acceptance Contract.
