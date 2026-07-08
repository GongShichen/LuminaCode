---
name: Benchmark Research
description: Identify benchmark, evaluation, experiment, or comparison requirements and upstream constraints.
when-to-use: Use when task touches benchmark integration, model/product evaluation, performance testing, or score interpretation.
user-invocable: false
context: inline
---

Report:

- Upstream benchmark contract.
- What the product may adapt versus must leave untouched.
- Required files, containers, commands, and outputs.
- Dataset/task integrity rules, scoring logic, official versus debug modes, and reproducibility requirements.
- Failure modes that indicate harness issues, product issues, environment issues, or model/agent behavior.
- Metrics and report fields needed to make the result interpretable.

Do not alter benchmark definitions or scoring logic.
