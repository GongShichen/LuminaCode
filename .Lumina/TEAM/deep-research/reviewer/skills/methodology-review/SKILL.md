---
name: Methodology Review
description: Review research method quality, source diversity, bias, and conclusion strength.
when-to-use: Use by Reviewer before calling SubmitGateVerdict for methodology_review.
user-invocable: false
context: inline
---

Review whether the research process supports the answer.

Before reviewing:

- Call `GetTeamContext`.
- Use runtime contract as authoritative.
- Read `research-plan.md`, `sources.jsonl`, `evidence-matrix.jsonl`, `conflicts-and-limitations.md`, `final-report.md`, and `qa-report.md` if present.

Procedure:

1. Load context and required artifacts.
2. Compare the final report against the research contract.
3. Audit search breadth and source diversity.
4. Audit evidence reasoning and conflict handling.
5. Classify findings as blocking or nonblocking.
6. Write `review-report.md`.
7. Call `SubmitGateVerdict` for `methodology_review`.

Checks:

1. Research question fit
   - Does the final report answer the contracted question?
   - Are audience and decision intent respected?

2. Search breadth
   - Were multiple query families used?
   - Were primary/official/academic sources considered where appropriate?
   - Were negative/conflicting sources searched?

3. Source quality
   - Are sources credible for the claims they support?
   - Is there overreliance on secondary, outdated, vendor, or low-quality sources?

4. Evidence reasoning
   - Are conclusions proportional to evidence confidence?
   - Are conflicts acknowledged?
   - Are limitations visible enough for the user?
   - Does every key claim rest on retrieved source content rather than snippets, model memory, or unretrieved metadata?

5. Reproducibility
   - Can another agent understand what was searched, fetched, and used?
   - Are source IDs and artifacts stable?

Blocking findings:

- Conclusion stronger than evidence.
- Missing major source class.
- Missing conflict/limitation handling.
- Search strategy too narrow.
- QA failed or was skipped when required.
- Report would mislead a decision maker.
- Any final claim based on training knowledge, model memory, snippets, inaccessible sources, or undocumented fallback retrieval.
- Any final claim or evidence item citing a source without `retrieval_status` and `claim_support_allowed` in `sources.jsonl`.
- Any final claim or evidence item citing a source whose `claim_support_allowed` is false.
- Any source registry using invalid retrieval status aliases such as `success`, `fail`, `fetched`, or `fetch_failed` instead of `retrieved`, `partially_retrieved`, or `inaccessible`.
- Any workflow that skips Source Reader and treats Search Strategist candidates as registered evidence.

Output:

Write `review-report.md`:

```markdown
# Methodology Review

## Verdict
pass|accepted_with_notes|reject

## Method Review
- Question fit:
- Search breadth:
- Source quality:
- Evidence reasoning:
- Reproducibility:

## Findings
### Blocking
- ...
### Nonblocking
- ...

## Deferral Candidates
- Finding:
  - Why safe to defer:
  - Suggested follow-up:
```

Then call `SubmitGateVerdict`:

- gate/check name: `methodology_review`
- status: `pass`, `accepted_with_notes`, or `reject`
- findings: mark blocking accurately.
- evidence: cite review report sections and artifact paths.
