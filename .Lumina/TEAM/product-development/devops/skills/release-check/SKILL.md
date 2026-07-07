---
name: Release Check
description: Verify release readiness after implementation.
when-to-use: Use before final gate or commit.
user-invocable: false
context: inline
---

Check:

- `go test ./...`
- frontend build/test commands.
- install resources present.
- README/docs updated when behavior changes.
- No accidental generated files in git status.
