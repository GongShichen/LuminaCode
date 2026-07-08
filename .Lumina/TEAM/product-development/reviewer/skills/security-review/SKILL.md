---
name: Security Review
description: Review permissions, local IPC, filesystem, and command execution risks.
when-to-use: Use when changes touch daemon, tools, permissions, or local files.
user-invocable: false
context: inline
---

Check the product's actual threat surface:

- Authentication, authorization, session/tenant isolation, token handling, and privilege boundaries.
- Permission routing, denial behavior, auditability, destructive operations, and user confirmation.
- File path, upload/download, command execution, dependency, plugin, extension, and external-service trust boundaries.
- Secrets, credentials, environment variables, logs, telemetry, error messages, and data retention.
- Injection risks: SQL/NoSQL, shell, template, path traversal, XSS, SSRF, prompt/tool injection, or unsafe deserialization as relevant.
- Privacy leakage between users, sessions, projects, environments, or visible/hidden product surfaces.
- Safe defaults, least privilege, and recovery behavior after partial failure.
