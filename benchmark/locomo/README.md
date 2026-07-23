# LoCoMo Evaluation

The former v2 six-channel LoCoMo runner was retired with the legacy memory
store. A Memory Fabric adapter will consume immutable ledger/index
snapshots and must keep benchmark labels and expected answers out of ingest,
semantic compilation, and retrieval. No benchmark is launched automatically.

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
