---
name: Code Review Checklist
description: Review implementation changes for correctness and maintainability.
when-to-use: Use for Reviewer gate on code changes.
user-invocable: false
context: inline
---

Lead with findings:

- Behavioral bugs.
- Concurrency or cancellation issues.
- Missing tests.
- API compatibility issues.
- Maintainability or ownership concerns.

Include path references and severity.
