---
name: Test Matrix
description: Convert requirements into focused unit, integration, and manual tests.
when-to-use: Use when QA needs a verification plan.
user-invocable: false
context: inline
---

Return a matrix that is specific to the detected stack and acceptance contract:

- Requirement.
- Test type.
- Setup.
- Assertion.
- Risk covered.
- Whether it is automated or manual.
- Command or manual action.
- Expected evidence and owner.

Include positive, negative, boundary, integration, accessibility, security/privacy, data-migration, and regression cases when they are relevant. Prefer the smallest test set that proves the contract without pretending uncovered areas are verified.
