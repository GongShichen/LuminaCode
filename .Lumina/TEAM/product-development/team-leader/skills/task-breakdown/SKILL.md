---
name: Task Breakdown
description: Build and maintain a team task graph with owners, dependencies, artifacts, and gates.
when-to-use: Use when the Team Leader needs to plan or re-plan a team run.
user-invocable: false
context: inline
---

Create a concise task graph:

- Objective and completion criteria.
- Project/artifact root. If the user asks for a new named project or directory and gives no absolute path, set the root to `<current working directory>/<requested name>`. All owner tasks, file paths, README paths, tests, and gate checks must use that root.
- Required artifacts and their owners.
- Specialist tasks with dependencies.
- QA and Reviewer gate tasks.
- Recovery tasks for any failed, timed out, or rejected work.

Prefer dispatchable tasks that fit one specialist's ownership.
