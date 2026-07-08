---
name: Regression Risk
description: Identify likely regressions from a proposed change.
when-to-use: Use before or after implementation to scope verification.
user-invocable: false
context: inline
---

Consider:

- Existing commands and compatibility.
- Data persistence, migrations, resume/retry behavior, caches, and background work.
- Permission, authentication, authorization, privacy, and destructive-action flows.
- User-facing interaction, accessibility, error handling, and long-content behavior.
- API, schema, event, or file-format compatibility.
- Build, install, deployment, configuration, and environment paths.
- Performance, reliability, observability, and cleanup behavior.

Prioritize risks by user impact.
