---
name: Review Changes
description: Review changed code for bugs, regressions, maintainability issues, and missing tests.
when-to-use: When the user asks for a code review, a bug hunt, or a risk assessment of recent changes.
user-invocable: true
disable-model-invocation: false
context: inline
---

Review the relevant code and prioritize findings that would matter to the user soonest.

Focus on:
- correctness bugs and regressions
- security or data-loss risks
- missing or weak test coverage
- maintainability issues that are likely to cause near-term trouble

Respond with findings first, ordered by severity, and include file paths when possible.
If you do not find significant issues, say so clearly and mention any residual risks or testing gaps.
