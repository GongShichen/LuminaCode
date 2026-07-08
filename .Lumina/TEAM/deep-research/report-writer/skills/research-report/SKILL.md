---
name: Research Report
description: Write an evidence-backed final research report from sources and the evidence matrix.
when-to-use: Use after evidence matrix coverage is sufficient.
user-invocable: false
context: inline
---

Write a final report that is useful and traceable.

Inputs:

- Research contract.
- `sources.jsonl`.
- `evidence-matrix.jsonl`.
- `conflicts-and-limitations.md`.
- Gate findings, if any.

Procedure:

1. Confirm readiness
   - Required artifacts exist.
   - Evidence matrix covers each subquestion.
   - Major claims have source IDs.
   - Known conflicts are documented.

2. Structure the report
   - Executive answer.
   - Scope and method.
   - Findings by subquestion.
   - Comparative table when useful.
   - Engineering/product/research implications when relevant.
   - Limitations and open questions.
   - Source list.
   - Before writing the full file, decide the section outline and source IDs for each section. Keep this planning short; the deliverable is the file, not an extended drafting conversation.

3. Citation style
   - Cite source IDs inline, for example `[S003]`.
   - Do not cite search result snippets.
   - Do not cite inaccessible sources for claims.

4. Claim discipline
   - Use strong language only for high-confidence evidence.
   - Use caveats for medium/low-confidence evidence.
   - Avoid claims not represented in evidence matrix.

Output template:

```markdown
# Final Research Report

## Executive Answer
...

## Scope and Method
- Research question:
- Search/fetch tools:
- Source classes:

## Findings
### 1. Subquestion
- Finding: ... [S001, S004]
- Confidence:
- Caveats:

## Comparison / Synthesis
...

## Engineering or Decision Implications
...

## Conflicts and Limitations
...

## Sources
- [S001] Title, authors/org, date, URL/arXiv ID.

## Evidence Index
| Evidence ID | Claim / finding | Source IDs | Confidence |
|-------------|-----------------|------------|------------|
| E001 | ... | S001, S004 | high |
```

Completion criteria:

- A reader can trace key claims to source IDs and evidence IDs from inside `final-report.md`.
- The report includes both a source index and an evidence index; do not rely on separate JSONL files alone for traceability.
- The report states what is known, uncertain, and not covered.
- The final answer can point to this file without additional explanation.

Long-report execution rule:

- Prefer one complete `write_file` call for `final-report.md`.
- If the report is too large to draft confidently in one pass, write a concise but complete version first, then perform one improvement pass. Do not repeatedly ask Team Leader for retry unless a concrete artifact or evidence gap exists.
- Do not spend a long hidden planning phase before any visible output. Emit a short progress statement, then write the file.
