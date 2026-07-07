---
name: Security Review
description: Review permissions, local IPC, filesystem, and command execution risks.
when-to-use: Use when changes touch daemon, tools, permissions, or local files.
user-invocable: false
context: inline
---

Check:

- Localhost and token enforcement.
- Permission routing and denial behavior.
- File path trust boundaries.
- Tool execution and destructive operations.
- Leakage between ordinary and Team contexts.
