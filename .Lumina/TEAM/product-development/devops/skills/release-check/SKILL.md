---
name: Release Check
description: Verify release readiness after implementation.
when-to-use: Use before final gate or commit.
user-invocable: false
context: inline
---

Check:

- Repository status contains only intentional source, docs, config examples, tests, and required assets.
- Manifest-declared build, test, lint, type-check, packaging, migration, or smoke commands have evidence.
- Install/setup/local-run instructions match the implemented behavior.
- Versioning, changelog, README, examples, API docs, or migration notes are updated when user-facing behavior changes.
- Generated files, caches, logs, reports, credentials, local databases, and temporary artifacts are excluded unless intentionally tracked.
- Release or deployment risks are called out with concrete mitigation or deferral reasons.
