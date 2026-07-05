---
name: Jupyter Notebook Work
description: Create, edit, or refactor `.ipynb` notebooks for experiments, analysis, and tutorials while preserving valid notebook structure.
when-to-use: When the user asks to create, modify, clean up, execute, or review a Jupyter notebook.
user-invocable: true
disable-model-invocation: false
context: inline
---

Work with notebooks as structured JSON, not plain text. Preserve metadata unless there is a clear reason to change it, and keep edits scoped to the requested cells or narrative.

For new notebooks, choose the shape first:
- experiment: objective, setup, data loading, method, results, checks, conclusion
- tutorial: goal, prerequisites, step-by-step sections, runnable examples, recap

For existing notebooks:
- inspect cell order and metadata before editing
- keep code cells small and runnable
- avoid large noisy outputs unless the user needs them
- clear or summarize stale outputs when they would mislead readers
- execute top-to-bottom when the environment supports it

Prefer notebook-aware tools or JSON libraries for edits. If execution is not possible, say exactly what was not run and provide the command or kernel setup needed to validate locally.
