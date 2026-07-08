---
name: Search Plan
description: Design and execute a multi-round SearxNG/arXiv search strategy.
when-to-use: Use when finding candidate sources for DeepResearch.
user-invocable: false
context: inline
---

Build a source discovery plan and execute it.

Boundary:

- Do not write `sources.jsonl`, `evidence-matrix.jsonl`, source notes, final reports, QA reports, or review reports.
- Search Strategist output is a candidate handoff only. Source Reader is responsible for fetching content and registering retrievable sources.
- If you use WebFetch to sanity-check a candidate, label it as `fetch_status` in the handoff. Do not treat it as a registered source.

Inputs:

- Research contract.
- Scope brief.
- Existing sources and evidence gaps.

Procedure:

1. Generate query families
   - Core concept query.
   - Synonym query.
   - Primary-source query.
   - Critical/limitations query.
   - Recent query when freshness matters.
   - Academic query when papers matter.

2. Select channels
   - Use `WebSearch` with `categories: "science"` for academic/technical discovery when useful.
   - Use `WebSearch` with `site` for official docs, standards, or specific domains.
   - Use arXiv MCP `mcp__arxiv__search_arxiv` for academic paper discovery.
   - Use arXiv MCP `mcp__arxiv__get_paper_details` for candidate metadata or content previews.
   - Use arXiv MCP `mcp__arxiv__search_and_summarize` when a query needs a compact paper set overview.
   - If the contract requires official, regulatory, standard, or authoritative medical sources and WebSearch returns no results for that source class, use documented shell/curl fallback against the public domain itself, such as `curl -L https://www.fda.gov/...` or a public site search endpoint. Record the prior WebSearch error or empty result and mark the discovery channel as fallback.

3. Execute in rounds
   - Round 1: broad discovery, 5-10 results.
   - Round 2: targeted source types/domains.
   - Round 3: conflicts, critiques, negative evidence.
   - Stop when the contract coverage is met. Do not keep searching after you have enough credible candidates for every required source class.
   - For interactive use, prefer a bounded handoff: normally 12-20 candidates, at least the requested minimum plus 2-4 backups. More than 25 candidates usually slows Source Reader without improving quality.

4. Score candidates
   - Relevance: high/medium/low.
   - Credibility: primary/peer-reviewed/official/secondary/unknown.
   - Freshness.
   - Likely use: definition, empirical evidence, benchmark, method, limitation, context.

5. Return candidate list
   - Do not claim snippets as evidence.
   - Mark which sources Source Reader should fetch first.
   - Mark `fetch_status: not_fetched|fetched_preview|fetch_failed` if known.
   - Mark `evidence_allowed: no` for every candidate, because only Source Reader can promote a candidate to evidence after reading the source.
   - Include a `coverage_summary` that explicitly states whether every required source class is covered. If a required class is missing, do not hand off as complete; run targeted recovery or fallback first.

Output format:

```markdown
## Search Plan and Candidate Sources

### Query Families
- F1: ...

### Searches Run
| Round | Tool | Query | Category/Site | Result count | Notes |
|---|---|---|---|---|---|

### Candidate Sources
| Candidate ID | Title | URL/arXiv ID | Type | Relevance | Credibility | Read Priority | Fetch Status | Evidence Allowed | Reason |
|---|---|---|---|---|---|---|---|---|---|

### Coverage Gaps
- ...
```

Recovery examples:

- If SearxNG general engines time out, retry with `categories: "science"` or `site:` restrictions.
- If all web search fails, use arXiv MCP for academic topics and report WebSearch errors exactly.
- If official/regulatory/medical authority searches are empty but required, use shell/curl fallback on the public official site and record the exact failed WebSearch query and fallback reason.
- If a query family produces many irrelevant arXiv hits, stop broad querying and switch to known exact titles, author names, or site/domain-restricted queries.
