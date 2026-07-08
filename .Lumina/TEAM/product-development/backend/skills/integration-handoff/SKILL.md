---
name: Backend Integration Handoff
description: Prepare backend implementation evidence and integration handoff for frontend, QA, and Reviewer.
when-to-use: Use after backend implementation or whenever integration behavior changes.
user-invocable: false
context: inline
---

Create a backend handoff that can be merged into `<project_root>/INTEGRATION_REPORT.md`.

Required content:

- Backend files changed or created.
- Runtime command and working directory.
- Required environment variables, ports, fixtures, seed data, and cleanup.
- API/command/event/storage contract implemented, with link to `INTERFACE_CONTRACT.md`.
- Positive smoke command and expected output.
- Negative/validation smoke command and expected output.
- Persistence behavior and where data is stored.
- Known limitations and ownership of any open issue.

Backend integration checklist:

1. Start the service from the project root.
2. Verify every endpoint/command/event that the frontend consumes.
3. Verify error behavior and validation behavior.
4. Verify persistence by writing, restarting if practical, and reading again.
5. Confirm no generated artifacts escaped the project root.
6. Send Frontend and Team Leader the exact commands and results.

Do not claim integration is complete if the frontend has not exercised the real contract.
