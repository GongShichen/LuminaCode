---
name: Runtime Architecture
description: Design backend runtime ownership, concurrency, and isolation.
when-to-use: Use for QueryEngine, SessionManager, TeamManager, tools, and loop changes.
user-invocable: false
context: inline
---

Specify:

- Runtime objects and ownership.
- Locks, goroutines, cancellation, and concurrency limits.
- State isolation boundaries.
- Event flow and persistence points.
- How ordinary Agent behavior remains unchanged.
