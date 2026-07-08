---
name: IPC Contract
description: Define service, API, event, storage, or process-boundary contracts between components.
when-to-use: Use when two or more components must agree on data shape, behavior, ordering, errors, or compatibility.
user-invocable: false
context: inline
---

Write or update the shared contract as `<project_root>/INTERFACE_CONTRACT.md`.

Use the vocabulary of the actual stack. A contract may be HTTP, CLI, file, database, event, queue, function, plugin, UI bridge, or another component boundary.

Size guidance:

- Small local tools or single-boundary features: 60-140 lines.
- Larger multi-component systems: expand as needed, but keep each operation concise.
- Prefer one operation table plus error/compatibility notes. Avoid long code examples for every operation unless they are necessary to remove ambiguity.

Required sections:

1. Contract summary
   - Producer/owner.
   - Consumer/owner.
   - Purpose.
   - Version or compatibility status.

2. Operations
   - Endpoint, method, command, event, topic, file, database table, or function name.
   - Request/input shape, including types, constraints, defaults, and examples.
   - Response/output shape, including types, ordering, nullability, and examples.
   - Validation rules and where they run.
   - For local persistence contracts, storage path resolution and any subprocess-visible test override such as an environment variable, cwd convention, fixture path, or command option.

3. Errors and edge behavior
   - Error shape and status/exit/event codes.
   - Retryability, idempotency, timeout, cancellation, and partial failure behavior.
   - Empty state and not-found behavior.

4. Security and trust boundary
   - Authentication and authorization when applicable.
   - Locality, privacy, secret handling, and tenant/session boundary.
   - What the consumer must not access directly.

5. Sequencing and consistency
   - Required call order.
   - Persistence timing and eventual-state expectations.
   - Subscription/update/polling behavior when relevant.

6. Compatibility and migrations
   - Backward/forward compatibility.
   - Migration plan for existing data or clients.
   - Fields reserved for future change.

7. Integration smoke
   - Minimal command or manual flow proving the consumer uses the real contract.
   - Positive, negative, and persistence checks where relevant.
   - If smoke/tests spawn a subprocess, the contract must specify how that subprocess receives isolated storage or fixtures; in-process monkeypatches do not cross this boundary.

Handoff rules:

- Send a concise A2A summary to Frontend, but do not treat A2A text as the source of truth.
- Frontend must acknowledge the contract and either accept it or propose changes.
- If contract changes during implementation, update `INTERFACE_CONTRACT.md`, then notify affected agents.
