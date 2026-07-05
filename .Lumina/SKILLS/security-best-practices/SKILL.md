---
name: Security Best Practices
description: Review or write code with language-specific security defaults, focusing on practical vulnerabilities, safe configuration, and regression-aware fixes.
when-to-use: When the user explicitly asks for a security review, secure-by-default implementation, vulnerability report, or security hardening.
user-invocable: true
disable-model-invocation: false
context: inline
---

Use this skill only for security-focused work. First identify the in-scope language, framework, entry points, data stores, authentication model, and deployment assumptions from repository evidence.

For a report, write findings first and group them by severity. Each finding should include:
- impact
- evidence with file paths and line numbers when available
- why the issue is exploitable in this project
- a concrete mitigation
- tests or checks that should validate the fix

When implementing fixes, keep changes narrow and compatible with the existing behavior. Prefer established libraries and framework controls for validation, escaping, cryptography, secret handling, CSRF protection, rate limits, path handling, and authorization checks.

Do not over-report missing TLS, HSTS, or secure cookies for local-only development. Do not print secrets, tokens, cookies, or private keys in logs, reports, or command output. If a recommendation depends on production deployment details that are not in the repo, state the assumption clearly.
