---
name: Runtime Architecture
description: Design runtime ownership, concurrency, async work, and isolation for the product's backend or platform layer.
when-to-use: Use when changing service/runtime loops, workers, queues, background jobs, permissions, stateful processes, or integration orchestration.
user-invocable: false
context: inline
---

Work stack-neutrally. First identify the language, framework, hosting model, and existing runtime patterns. Then specify:

For small local tools or simple product slices, keep this as a compact checklist of decisions and risks. Do not expand into a full platform architecture unless the task involves services, workers, concurrency, permissions, deployment, or other real runtime complexity.

- Runtime objects, processes, workers, queues, jobs, or services and who owns each piece of state.
- Concurrency or async strategy: locks, event loops, promises, threads, workers, actors, transactions, cancellation, retries, backpressure, and resource limits as appropriate for the stack.
- Isolation boundaries for users, tenants, sessions, requests, permissions, cache entries, secrets, and temporary files.
- Event flow, ordering assumptions, failure modes, retry/idempotency behavior, and persistence points.
- Compatibility with existing APIs and behavior, including migration or rollout risks.
- Observability needed to debug failures without leaking sensitive data.
