# Lumina LongMemEval-V2 Adapter

This adapter is isolated from the production runtime. It converts public agent
trajectories into ordinary visible messages, writes them through
`ExtractionController.IngestMessages`, and queries them through
`RunMemoryRecallWithRuntime`.

The adapter never reads the benchmark question ID, question category, expected
answer, evaluator specification, or gold trajectory metadata. Hidden trajectory
reasoning is excluded during ingestion.
