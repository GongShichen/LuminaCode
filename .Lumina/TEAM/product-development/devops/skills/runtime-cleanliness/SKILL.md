---
name: Runtime Cleanliness
description: Keep generated runtime, verification, cache, log, and build artifacts inside the intended project root.
when-to-use: Use before QA or release checks for named-project work and local smoke tests.
user-invocable: false
context: inline
---

Use this skill to prevent test or runtime byproducts from leaking into the parent workspace.

Checklist:

1. Identify roots
   - Parent working directory.
   - Project root.
   - Runtime/session storage.

2. Before running commands
   - Run build/test/smoke commands from the project root unless the command is explicitly global.
   - Redirect logs, pid files, smoke scripts, temporary files, data fixtures, coverage, and caches inside the project root or runtime storage.

3. During server/process tests
   - Record pid and command.
   - Use a teardown command.
   - Avoid leaving background processes running.

4. After verification
   - List parent directory contents.
   - Confirm only preexisting files plus the requested project directory remain.
   - Remove accidental byproducts or dispatch recovery work.

Evidence:

- Add cleanliness findings to `INTEGRATION_REPORT.md`, `QA_REPORT.md`, or release notes.
- If cleanup cannot be completed, mark it as blocking unless Team Leader explicitly defers with a reason.
