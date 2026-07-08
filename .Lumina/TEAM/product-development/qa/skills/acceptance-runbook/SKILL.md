---
name: Acceptance Runbook
description: Provide concrete verification commands and manual acceptance steps.
when-to-use: Use at QA gate time.
user-invocable: false
context: inline
---

Produce a runnable acceptance runbook and use it to populate `<project_root>/QA_REPORT.md`.

Inputs:

- Runtime Team Acceptance Contract from `GetTeamContext`.
- Required documents and implementation files.
- Project manifest/scripts and stack conventions.
- Integration report, if present.

Contract handling:

- The runtime Team Acceptance Contract is the source of truth for required artifacts, commands, smoke tests, and completion criteria.
- Do not require a standalone contract file unless the runtime contract explicitly lists one in required artifacts.
- If a required artifact is absent from `GetTeamContext.artifacts`, verify whether the file exists on disk before marking it missing; then report the exact missing path.

Runbook structure:

1. Setup
   - Working directory.
   - Runtime version and dependencies.
   - Environment variables, ports, fixtures, seed data.
   - Background process start/stop plan.

2. Required command matrix
   - Command.
   - Purpose.
   - Expected pass signal.
   - Actual result and evidence summary.
   - Blocking or non-blocking if unavailable.
   - Confirm the command is executable as written. For Python projects, `python -m <name>` must use an importable module/package name; a hyphenated product name is not valid unless packaging metadata creates a console entrypoint.
   - Preserve failure exit codes. Do not accept evidence from `command | head`, `command | tail`, or similar pipelines unless the original command status is captured and reported.

3. Integration smoke
   - Real user-facing path.
   - Real backend/API/storage/tooling contract.
   - Positive case.
   - Negative/validation case.
   - Persistence or restart case when relevant.

4. Manual checks
   - User-visible workflow.
   - Accessibility/keyboard basics.
   - Error and empty states.
   - For CLI/terminal products, include parameter-parse failures and validation failures:
     missing required argument, invalid type such as a nonnumeric ID, unknown command/option, empty input, nonexistent resource, and corrupted data/configuration when relevant.
   - If the user request, PRD, UX design, or contract requires localized output, every user-visible stderr/stdout message in these failures must be checked for that locale.
   - If an explicit requirement fails, classify it as blocking unless the Team Leader recorded a specific deferral accepted by the user.

5. Cleanup
   - Stop background processes.
   - Remove temporary files or confirm they are inside project root.
   - Verify parent workspace cleanliness.

6. Verdict
   - pass, fail, or not_applicable.
   - Findings with owner and reproduction.
   - Evidence paths and command summaries for `SubmitGateVerdict`.

If a required command cannot be run, mark the reason precisely and classify whether it blocks completion. Missing evidence for a contract-required command is a QA failure.

Embedded HTTP smoke helper:

For small HTTP API checks, you may copy this helper into the project root or run it inline with `python3 - <<'PY' ... PY`. Adapt endpoint paths and payloads to the actual interface contract. Do not rely on a separate helper file in the skill directory.

```python
#!/usr/bin/env python3
"""Small stdlib HTTP smoke helper for QA runbooks.

Usage:
  http_smoke.py GET http://localhost:8000/api/feedback 200
  http_smoke.py POST http://localhost:8000/api/feedback 201 '{"author":"QA","message":"ok"}'
"""

from __future__ import annotations

import json
import sys
import urllib.error
import urllib.request


def main(argv: list[str]) -> int:
    if len(argv) not in (4, 5):
        print(__doc__.strip(), file=sys.stderr)
        return 2
    method, url, expected_raw = argv[1], argv[2], argv[3]
    body = argv[4].encode("utf-8") if len(argv) == 5 else None
    try:
        expected = int(expected_raw)
    except ValueError:
        print(f"expected status must be an integer: {expected_raw}", file=sys.stderr)
        return 2
    headers = {}
    if body is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=body, headers=headers, method=method.upper())
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            status = response.status
            payload = response.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        status = exc.code
        payload = exc.read().decode("utf-8", errors="replace")
    print(f"status={status}")
    if payload:
        try:
            print(json.dumps(json.loads(payload), indent=2, ensure_ascii=False))
        except json.JSONDecodeError:
            print(payload[:2000])
    if status != expected:
        print(f"expected status {expected}, got {status}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
```
