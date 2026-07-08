---
name: Artifact Gate
description: Check that each product-development stage has durable documents before moving to the next gate.
when-to-use: Use before implementation, before QA/Reviewer dispatch, and before final completion.
user-invocable: false
context: inline
---

Use this skill to prevent "the team talked about it" from replacing durable project artifacts.

Default document set for ordinary software product work:

- `PRD.md`
- `UX_DESIGN.md`
- `BACKEND_PLAN.md`
- `FRONTEND_PLAN.md`
- `INTERFACE_CONTRACT.md`
- `INTEGRATION_REPORT.md`
- `QA_REPORT.md`
- `REVIEW_REPORT.md`

Gate checks:

1. Before UX Design
   - `PRD.md` exists and includes goals, scope, user flow, functional requirements, data requirements, and acceptance criteria.

2. Before technical plans
   - `PRD.md` and `UX_DESIGN.md` exist.
   - Design includes states, interaction flow, copy, accessibility, and platform constraints.

3. Before implementation
   - `BACKEND_PLAN.md` and `FRONTEND_PLAN.md` exist.
   - `INTERFACE_CONTRACT.md` exists and is acknowledged by both Frontend and Backend.
   - Team Leader has checked plan boundaries and called `RecordTeamContract`.
   - For CLI/TUI/local tools with persistence, `INTERFACE_CONTRACT.md` names one concrete storage path resolution and subprocess-visible test isolation mechanism. Reject vague language such as "environment variable or monkeypatch" because monkeypatch does not cross subprocess boundaries. Reject function parameter-only overrides unless the launched CLI/API exposes that parameter through a flag, environment variable, config path, fixture path, or cwd convention.

4. Before QA/Reviewer
   - Implementation files exist under the project root.
   - `INTEGRATION_REPORT.md` exists or the Team Leader has explicitly dispatched integration evidence work.

5. Before completion
   - `QA_REPORT.md` exists and QA has called `SubmitGateVerdict`.
   - `REVIEW_REPORT.md` exists and Reviewer has called `SubmitGateVerdict`.
   - Required commands and integration smokes in the Team Acceptance Contract have evidence.

If any document is missing:

- Do not continue as if the stage is complete.
- Dispatch a recovery task to the owner.
- If the document is truly not applicable, record the reason in the Team Acceptance Contract or `CompleteTeamTask.deferral_reasons`.

Embedded helper:

If you want a mechanical check, create this temporary helper inside the project root or run it with `python3 - <<'PY' ... PY`. Do not rely on a separate file in the skill directory.

```python
#!/usr/bin/env python3
"""Check required product-development stage documents under a project root."""

from __future__ import annotations

import pathlib
import sys


REQUIRED = [
    "PRD.md",
    "UX_DESIGN.md",
    "BACKEND_PLAN.md",
    "FRONTEND_PLAN.md",
    "INTERFACE_CONTRACT.md",
    "INTEGRATION_REPORT.md",
    "QA_REPORT.md",
    "REVIEW_REPORT.md",
]


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: check_stage_docs.py <project_root>", file=sys.stderr)
        return 2
    root = pathlib.Path(argv[1]).expanduser().resolve()
    missing = [name for name in REQUIRED if not (root / name).is_file()]
    if missing:
        print("missing stage documents:")
        for name in missing:
            print(f"- {name}")
        return 1
    print("all required stage documents exist")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
```
