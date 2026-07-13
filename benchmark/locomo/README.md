# LoCoMo Adapter

The LoCoMo runner is a black-box consumer of LuminaCode's production memory
pipeline. It imports each public conversation session with its original
timestamp, writes messages through `ExtractionController.IngestMessages`, runs
the configured local embedding runtime, and answers questions through the same
six-channel recall used by ordinary agents.

Benchmark category, annotated evidence, and expected answers are never passed
to ingestion, query expansion, retrieval, or the answer model. They are read
only after each answer for offline metrics.

The default comparable run includes categories 1-4 (1,540 questions). Category
5 contains 446 adversarial unanswerable questions and can be run separately
with `--include-adversarial`.

```bash
go run ./cmd/lumina_locomo_benchmark \
  --data ~/Documents/benchmark/locomo/data/locomo10.json \
  --parallel 16 \
  --output-dir ~/Documents/benchmark/reports/locomo-$(date +%Y%m%d-%H%M%S)
```

The output directory contains a resumable `checkpoint.jsonl`, a diagnostic
`report.json`, official-shaped `predictions.json`, and one isolated SQLite
memory store per conversation.

## Backboard-Style LLM Judge

The official token F1 remains available as a diagnostic. For direct comparison
with memory-system reports that use binary LLM judging, evaluate the completed
answers separately with DeepSeek:

```bash
python benchmark/locomo/deepseek_judge.py \
  --input ~/Documents/benchmark/reports/locomo-<timestamp>/checkpoint.jsonl \
  --output-dir ~/Documents/benchmark/reports/locomo-<timestamp>/deepseek-evaluator \
  --model deepseek-v4-pro \
  --parallel 16
```

The evaluator reuses the generous correctness prompt published by
`Backboard-io/Backboard-Locomo-Benchmark`. It is resumable and writes per-case
reasoning, overall and per-category accuracy, per-conversation accuracy, F1,
and runtime diagnostics. It reads the API key from `DEEPSEEK_API_KEY`, or from
a matching DeepSeek entry in `~/.lumina/CONFIG/defaults.json`; credentials are
never written to result files.
