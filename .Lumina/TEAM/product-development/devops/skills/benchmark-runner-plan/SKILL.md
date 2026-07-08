---
name: Benchmark Runner Plan
description: Plan benchmark, evaluation, load-test, or experiment environment and reporting behavior.
when-to-use: Use for benchmark harness, evaluation suites, reproducible experiments, performance tests, or environment preparation.
user-invocable: false
context: inline
---

Specify:

- Benchmark purpose: model quality, agent harness, product performance, reliability, cost, latency, or regression detection.
- Upstream or official harness boundaries, including what must not be modified.
- Required datasets, containers, services, credentials, hardware, network access, seeds, and versions.
- Working directories, mounted resources, generated output paths, report formats, and artifact retention.
- Debug versus official run markers, limits, sampling, and how those affect score interpretation.
- Metrics to capture, including pass/fail, latency percentiles, resource use, cost, logs, and failure categories when relevant.
- Cleanup, reproducibility, and resume behavior.
