---
name: Architecture Risk Review
description: Review system boundaries, coupling, and long-term risks.
when-to-use: Use for backend/frontend architecture changes.
user-invocable: false
context: inline
---

Check:

- Component boundaries and whether responsibilities match the Team Acceptance Contract.
- Data ownership, schema/API compatibility, migrations, and lifecycle.
- Event ordering, consistency, retries, idempotency, and failure recovery paths.
- Security, privacy, permissions, tenancy/session isolation, and trust boundaries.
- Extension points, dependency direction, and whether the design creates avoidable coupling.
- Operational concerns: observability, resource limits, rollout, rollback, and cleanup.

Recommend specific changes when risks are material. Mark architecture mismatch, missing integration, skipped requirements, correctness risk, security risk, build-breaking risk, and data-loss risk as blocking.
