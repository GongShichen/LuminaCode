---
name: Completion Check
description: Verify Team completion policy before calling CompleteTeamTask.
when-to-use: Use immediately before final completion.
user-invocable: false
context: inline
---

Check:

- User objective is satisfied.
- Required artifacts exist.
- QA verdict is pass or not_applicable.
- Reviewer verdict is pass or accepted_with_notes.
- Product Team non-blocking QA/Reviewer findings are either fixed and regated, or each has an explicit deferral reason for `CompleteTeamTask`.
- No required work is active, queued, blocked, or unreviewed.
- Final answer is concise and useful.

If any item fails, dispatch recovery work instead of completing.
