---
name: Repo Map
description: Map repository structure, ownership boundaries, and relevant files.
when-to-use: Use when a task requires understanding the current codebase.
user-invocable: false
context: inline
---

Inspect the repository with fast file search and targeted reads. Return:

- High-level modules and entry points.
- Files most relevant to the current task.
- Existing patterns to reuse.
- Unknowns that need direct code reading.
- Detected stack, package/build/test manifests, generated-code boundaries, and important conventions.
- Ownership boundaries: client, server, data, integration, infra, tests, docs, and scripts where applicable.
- Risky areas: global state, migrations, public APIs, permission checks, async jobs, deployment config, or compatibility layers.

Include paths and short evidence, not guesses.
