---
name: Commit Help
description: Prepare a high-quality commit message for the current work.
when-to-use: When the user asks for a commit message, wants help summarizing changes, or wants to prepare a commit.
user-invocable: true
disable-model-invocation: false
context: inline
---

Help prepare a clear commit message for the current changes.

Prefer:
- a concise subject line
- an optional body only when it adds useful context
- wording that reflects user-facing behavior changes, fixes, or refactors

If the user has not asked to actually create a commit, do not assume one should be made.
If you need more context, inspect the changed files or diff before proposing the message.
