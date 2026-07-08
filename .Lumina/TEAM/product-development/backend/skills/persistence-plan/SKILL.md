---
name: Persistence Plan
description: Plan durable state, data model, migrations, caching, and recovery behavior.
when-to-use: Use when adding or reviewing database, file, cache, session, queue, artifact, or configuration persistence.
user-invocable: false
context: inline
---

Cover the product's actual storage model:

- Data entities, ownership, retention, privacy, and deletion expectations.
- Storage location: database, object store, local files, browser storage, cache, queue, or external service.
- For CLI/TUI/local tools, define exactly how the storage path is resolved in real execution: project root, current working directory, user config directory, environment variable, command flag, or package-relative path. Prefer a testable override such as an environment variable or explicit config path when subprocess integration tests need isolated data.
- Do not ship a package-relative-only data path for a CLI tool if tests, smoke commands, or users need isolated workspaces. Provide a concrete override or cwd convention and document it in `INTERFACE_CONTRACT.md`.
- Schema shape, indexes, uniqueness, migrations, and compatibility with existing records.
- Atomicity, consistency, locking, transaction boundaries, crash recovery, and backup/restore expectations.
- Read/write paths, performance hot spots, LRU/TTL/compaction policies, and query patterns.
- Isolation between users, tenants, sessions, environments, and test data.
- Test isolation must work across real processes. If frontend/CLI tests run the product in a subprocess, monkeypatching an in-process module global is not a valid isolation mechanism; provide a subprocess-visible mechanism such as env var, cwd convention, fixture file path, or command option.
- A function parameter-only storage override is valid for in-process unit tests, but it is not sufficient for subprocess CLI integration unless the CLI exposes that override through a flag, environment variable, config path, fixture path, or cwd convention.
- If the contract or frontend plan asks for subprocess tests and only offers monkeypatching, treat the contract as incomplete and return it for correction before implementation.
- Validation or smoke checks that prove persistence survives restart/resume when required.
