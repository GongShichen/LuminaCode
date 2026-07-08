# DeepResearch Completion Policy

The task is complete only when:

- A research contract exists.
- Required artifacts exist or the Team Leader records why a requested artifact is not applicable.
- Key claims in the final report map to source IDs in the evidence matrix.
- `final-report.md` contains a readable source/evidence index section, so the user can trace report claims to `sources.jsonl` and `evidence-matrix.jsonl` from the report itself.
- `citation_qa` is pass or not_applicable.
- `methodology_review` is pass or accepted_with_notes without blocking findings.
- Nonblocking findings are either addressed or deferred with explicit reasons.
- The final answer tells the user where the working-directory deliverable package is stored, and where the complete runtime evidence trail remains available.

The Team Leader should finalize promptly once the configured gates are satisfied. Do not create additional ad hoc QA, review, or smoke tasks after acceptable gate verdicts unless a concrete blocking finding remains unresolved.

Missing contract-required source classes are blocking when the final report makes claims in that area. For example, a report section about regulation, clinical safety, or deployment risk needs official/regulatory/medical-authority evidence or an explicit scope limitation.

The working-directory deliverable package should contain the final report and evidence files, while raw runtime logs, web-search cache, tool outputs, timeline, and diagnostics remain under Lumina runtime storage.
