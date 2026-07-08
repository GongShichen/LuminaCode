---
name: Evidence Matrix
description: Convert source notes into claim-level evidence with confidence and limitations.
when-to-use: Use before report writing and whenever evidence gaps or conflicts appear.
user-invocable: false
context: inline
---

Build an evidence matrix that can support a final report.

Inputs:

- Research contract.
- Source notes.
- `sources.jsonl`.
- Existing `evidence-matrix.jsonl`.
- Conflicts or gate findings.

Procedure:

1. Enumerate answer claims
   - Extract candidate claims from source notes.
   - Group claims by subquestion.
   - Remove claims that do not answer the contract.

2. Link sources
   - Each evidence item must reference one or more source IDs.
   - Use multiple independent sources for major claims when possible.
   - Use only sources whose `sources.jsonl` record has `retrieval_status` of `retrieved` or `partially_retrieved` and `claim_support_allowed: true`.
   - Exclude sources marked `inaccessible`, `claim_support_allowed: false`, metadata-only, snippet-only, or fetch_failed.

3. Classify support
   - direct empirical result.
   - benchmark/dataset evidence.
   - theoretical argument.
   - official statement/specification.
   - expert opinion.
   - indirect/contextual evidence.

4. Assess confidence
   - high: direct, credible, recent, and corroborated.
   - medium: credible but limited or single-source.
   - low: weak, old, indirect, or contested.

5. Capture conflicts
   - Record conflicting evidence and why it differs.
   - Do not hide inconvenient sources.

6. Identify gaps
   - Missing source class.
   - Missing recent evidence.
   - Missing primary evidence.
   - Unsupported final-report claim.

Output JSONL schema:

```json
{"claim_id":"C001","subquestion":"...","claim":"...","source_ids":["S001","S002"],"support_type":"direct empirical result","confidence":"high|medium|low","evidence_summary":"...","limitations":"...","conflicts":["..."],"report_recommendation":"use|caveat|exclude"}
```

Also update `conflicts-and-limitations.md` with:

- Strongest conflicts.
- Evidence gaps.
- How these affect final recommendations.

Completion criteria:

- Report Writer can cite every key claim from the matrix.
- QA can verify source IDs.
- Reviewer can judge whether the method is adequate.
- No evidence item depends on a source that Source Reader did not retrieve.
