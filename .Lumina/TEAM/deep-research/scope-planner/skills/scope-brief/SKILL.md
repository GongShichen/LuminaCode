---
name: Scope Brief
description: Convert a broad research request into subquestions, assumptions, and source criteria.
when-to-use: Use before search planning or when the Team Leader needs clarification.
user-invocable: false
context: inline
---

Produce a compact scope brief that reduces ambiguity.

Inputs:

- User request.
- Current Team contract, if any.
- Prior team dialogue.

Procedure:

1. Classify the research type
   - Academic/literature.
   - Engineering implementation.
   - Product/market.
   - Policy/legal/regulatory.
   - Mixed.

2. Extract decision intent
   - What should the final report help the user decide?
   - What would make the research actionable?

3. Identify subquestions
   - Break the main question into 3-7 subquestions.
   - Make each subquestion answerable by evidence.
   - Mark dependencies between subquestions.

4. Define terminology
   - Key terms.
   - Synonyms and alternate spellings.
   - Terms that must not be conflated.

5. Define source strategy
   - Primary source types.
   - Secondary source types allowed only for context.
   - Minimum source diversity.
   - Freshness window.

6. Define risk areas
   - Likely bias.
   - Missing data.
   - Rapidly changing facts.
   - Claims likely to require primary sources.

Output format:

```markdown
## Scope Brief

### Research Type
...

### Decision Intent
...

### Subquestions
1. ...

### Terms and Synonyms
- Term: meaning; synonyms; exclusions.

### Source Criteria
- Primary:
- Secondary:
- Freshness:
- Minimum coverage:

### Assumptions
- ...

### Clarifications Needed
- Blocking:
- Nonblocking:
```

Completion criteria:

- A Search Strategist could generate queries from it.
- Team Leader could copy it into the research contract.
- It clearly distinguishes assumptions from user-stated requirements.
