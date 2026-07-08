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
- Contract-required integration behavior has real evidence, not only independent component tests.
- Declared stack-native build/check/test commands relevant to the delivered artifacts have been run or explicitly classified as not applicable.
- Every contract command and smoke command is executable as written. For Python CLI/package work, `python -m <name>` must use an importable module name, not a hyphenated product display name unless a package entrypoint exists.
- Verification evidence preserves command failure exit codes. Evidence from commands piped through `head`, `tail`, `tee`, `grep`, or formatters is insufficient unless the original status was preserved and reported.
- Named-project work did not pollute the parent workspace with generated artifacts.
- No required work is active, queued, blocked, or unreviewed.
- Final answer is concise and useful.

If any item fails, dispatch recovery work instead of completing.
