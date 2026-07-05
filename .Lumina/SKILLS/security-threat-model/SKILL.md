---
name: Security Threat Model
description: Build a repository-grounded threat model with assets, trust boundaries, entry points, abuse paths, priorities, and mitigations.
when-to-use: When the user asks to threat model a codebase, enumerate abuse paths, map trust boundaries, or produce an AppSec threat model.
user-invocable: true
disable-model-invocation: false
context: inline
---

Anchor the model to repository evidence instead of generic checklists. Start by identifying runtime components, entry points, external integrations, stateful stores, credentials, generated artifacts, logs, and privileged operations.

Separate runtime behavior from tests, examples, CI, and developer-only tooling. For each trust boundary, note the protocol or file interface, authentication, authorization, validation, serialization format, and relevant failure modes.

Produce a concise Markdown threat model with:
- scope and assumptions
- architecture summary grounded in files
- assets and attacker capabilities
- trust boundaries and entry points
- prioritized abuse paths with likelihood and impact
- existing controls with evidence
- recommended mitigations tied to concrete components
- open questions that materially affect priority

Prefer a small set of realistic threats over a long checklist. Ask focused questions only when missing deployment, authentication, exposure, or data-sensitivity context would materially change the priorities.
