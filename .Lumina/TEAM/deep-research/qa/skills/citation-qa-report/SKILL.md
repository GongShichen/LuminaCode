---
name: Citation QA Report
description: Verify source traceability, artifact completeness, and citation correctness before citation_qa gate.
when-to-use: Use by QA before calling SubmitGateVerdict for citation_qa.
user-invocable: false
context: inline
---

Audit the research artifacts for traceability.

Before reviewing:

- Call `GetTeamContext`.
- Use runtime contract as authoritative.
- Read required artifacts from the Lumina team runtime directory.

Procedure:

1. Load context and artifacts.
2. Validate required artifacts.
3. Parse source registry and evidence matrix.
4. Cross-check final-report source IDs.
5. Classify findings as blocking or nonblocking.
6. Write `qa-report.md`.
7. Call `SubmitGateVerdict` for `citation_qa`.

Checks:

1. Artifact completeness
   - Required artifacts exist.
   - Files are non-empty and relevant.
   - Runtime artifacts are not accidentally written into project root unless requested.

2. Source registry
   - `sources.jsonl` parses as JSONL.
   - Each source has source_id, title, url or arxiv_id, source_type, retrieval_tool, retrieval_status, claim_support_allowed, and notes_path.
   - Source IDs are unique.
   - Sources used by evidence items are `retrieved` or `partially_retrieved` and have `claim_support_allowed: true`.
   - `retrieval_status` values are only `retrieved`, `partially_retrieved`, or `inaccessible`.

3. Evidence matrix
   - `evidence-matrix.jsonl` parses as JSONL.
   - Each evidence item has claim, source_ids, confidence, support_type.
   - Referenced source IDs exist in `sources.jsonl`.

4. Final report citations
   - Key claims cite source IDs.
   - Cited source IDs exist.
   - No major claim relies only on snippets.
   - `final-report.md` contains a readable source index and evidence index. The evidence index must map evidence IDs to source IDs so the user can audit the report from the working-directory deliverable package.

5. Retrieval quality
   - Web claims have WebFetch-backed sources or documented shell/curl fallback sources after a recorded WebFetch/WebSearch failure.
   - Academic paper claims have arXiv MCP or fetched paper notes when applicable.
   - No evidence item is marked as supported by model memory, training knowledge, snippets, or unretrieved metadata.
   - Inaccessible sources are clearly excluded from final-report claim support.

Blocking findings:

- Any final-report key claim without a valid source ID.
- Any cited source ID missing from `sources.jsonl`.
- Any claim supported only by search snippets, model training knowledge, memory, unretrieved metadata, or undocumented fallback retrieval.
- Any inaccessible source used as if it were retrieved evidence.
- Any evidence item citing a source whose `retrieval_status` is missing, `inaccessible`, `fetch_failed`, or whose `claim_support_allowed` is false.
- Any source record using invalid retrieval status aliases such as `success`, `fail`, `fetched`, or `fetch_failed`.
- Any Search Strategist candidate list being treated as `sources.jsonl` without Source Reader retrieval notes.
- Missing `sources.jsonl`, `evidence-matrix.jsonl`, or `final-report.md` when required by the contract.
- Missing source/evidence index in `final-report.md`.

Output:

Write `qa-report.md`:

```markdown
# Citation QA Report

## Verdict
pass|fail|not_applicable

## Artifact Checks
| Artifact | Status | Evidence |

## Source Registry Checks
...

## Evidence Matrix Checks
...

## Citation Checks
...

## Findings
### Blocking
- ...
### Nonblocking
- ...

## Gate Evidence
- ...
```

Then call `SubmitGateVerdict`:

- gate/check name: `citation_qa`
- status: `pass`, `fail`, or `not_applicable`
- evidence: artifact checks and report path.
- findings: include blocking flags where appropriate.

Pass requires concrete evidence, not just "looks good".
