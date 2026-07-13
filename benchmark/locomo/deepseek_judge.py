#!/usr/bin/env python3
"""Backboard-style LoCoMo LLM-as-a-Judge evaluation using DeepSeek.

This is an offline evaluator. It reads completed benchmark answers and never
participates in ingestion, retrieval, or answer generation.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import threading
import time
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any, Iterable

import openai
from openai import OpenAI


CATEGORY_NAMES = {
    1: "single_hop",
    2: "temporal_reasoning",
    3: "multi_hop",
    4: "open_domain",
}

SYSTEM_PROMPT = (
    "You are evaluating conversational AI memory recall. "
    "Return JSON only with the format requested."
)

ACCURACY_PROMPT = """\
Your task is to label an answer to a question as 'CORRECT' or 'WRONG'. You will be given the following data:
    (1) a question (posed by one user to another user),
    (2) a 'gold' (ground truth) answer,
    (3) a generated answer
which you will score as CORRECT/WRONG.

The point of the question is to ask about something one user should know about the other user based on their prior conversations.
The gold answer will usually be a concise and short answer that includes the referenced topic, for example:
Question: Do you remember what I got the last time I went to Hawaii?
Gold answer: A shell necklace
The generated answer might be much longer, but you should be generous with your grading - as long as it touches on the same topic as the gold answer, it should be counted as CORRECT.

For time related questions, the gold answer will be a specific date, month, year, etc. The generated answer might be much longer or use relative time references (like "last Tuesday" or "next month"), but you should be generous with your grading - as long as it refers to the same date or time period as the gold answer, it should be counted as CORRECT. Even if the format differs (e.g., "May 7th" vs "7 May"), consider it CORRECT if it's the same date.

Now it's time for the real question:
Question: {question}
Gold answer: {expected_answer}
Generated answer: {generated_answer}

First, provide a short (one sentence) explanation of your reasoning, then finish with CORRECT or WRONG.
Do NOT include both CORRECT and WRONG in your response, or it will break the evaluation script.

Return your response in JSON format with two keys: "reasoning" for your explanation and "label" for CORRECT or WRONG.
"""


def record_key(record: dict[str, Any]) -> str:
    return f"{record['sample_id']}:{int(record['question_index'])}"


def load_jsonl(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    records: list[dict[str, Any]] = []
    with path.open(encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, start=1):
            if not line.strip():
                continue
            try:
                value = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"invalid JSONL at {path}:{line_number}: {exc}") from exc
            if isinstance(value, dict):
                records.append(value)
    return records


def load_api_key(env_name: str, config_path: Path, base_url: str) -> str:
    if value := os.environ.get(env_name, "").strip():
        return value
    if not config_path.exists():
        return ""
    config = json.loads(config_path.read_text(encoding="utf-8"))
    candidates = [
        (config.get("fallback_api_base_url", ""), config.get("fallback_api_key", "")),
        (config.get("api_base_url", ""), config.get("api_key", "")),
    ]
    requested_host = base_url.rstrip("/").lower()
    for candidate_url, candidate_key in candidates:
        if (
            isinstance(candidate_url, str)
            and isinstance(candidate_key, str)
            and candidate_key.strip()
            and candidate_url.rstrip("/").lower().startswith(requested_host)
        ):
            return candidate_key.strip()
    return ""


def parse_judge_content(content: str) -> tuple[bool, str, str]:
    normalized = content.strip()
    fenced = re.fullmatch(r"```(?:json)?\s*(.*?)\s*```", normalized, flags=re.DOTALL | re.IGNORECASE)
    if fenced:
        normalized = fenced.group(1).strip()
    try:
        payload = json.loads(normalized)
    except json.JSONDecodeError:
        match = re.search(r"\{.*\}", normalized, flags=re.DOTALL)
        if not match:
            raise ValueError("judge response did not contain a JSON object")
        payload = json.loads(match.group(0))
    if not isinstance(payload, dict):
        raise ValueError("judge response JSON must be an object")
    label = str(payload.get("label", "")).strip().upper()
    if label not in {"CORRECT", "WRONG"}:
        raise ValueError(f"invalid judge label {label!r}")
    reasoning = str(payload.get("reasoning", "")).strip()
    return label == "CORRECT", label, reasoning


def mean(values: Iterable[float]) -> float:
    items = list(values)
    return sum(items) / len(items) if items else 0.0


def metric_row(records: list[dict[str, Any]]) -> dict[str, Any]:
    evaluated = [item for item in records if not item.get("judge_error")]
    correct = sum(bool(item["judge"]["is_correct"]) for item in evaluated)
    return {
        "total": len(records),
        "evaluated": len(evaluated),
        "correct": correct,
        "accuracy": correct / len(evaluated) if evaluated else 0.0,
        "evaluation_coverage": len(evaluated) / len(records) if records else 0.0,
        "runner_mean_f1": mean(float(item.get("f1", 0.0)) for item in records),
        "average_retrieval_seconds": mean(float(item.get("retrieval_ms", 0)) / 1000 for item in records),
        "average_response_seconds": mean(float(item.get("answer_ms", 0)) / 1000 for item in records),
    }


def aggregate_results(
    source_records: list[dict[str, Any]],
    judged_by_key: dict[str, dict[str, Any]],
    model: str,
    base_url: str,
    official_evaluation: dict[str, Any] | None = None,
) -> dict[str, Any]:
    combined: list[dict[str, Any]] = []
    for source in source_records:
        key = record_key(source)
        if key in judged_by_key:
            combined.append(judged_by_key[key])
        else:
            pending = dict(source)
            pending["judge_error"] = "not evaluated"
            combined.append(pending)

    by_category: dict[int, list[dict[str, Any]]] = defaultdict(list)
    by_conversation: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for item in combined:
        by_category[int(item["category"])].append(item)
        by_conversation[str(item["sample_id"])].append(item)

    category_rows = {
        CATEGORY_NAMES.get(category, f"category_{category}"): {
            "category": category,
            **metric_row(items),
        }
        for category, items in sorted(by_category.items())
    }
    if official_evaluation:
        official_categories = official_evaluation.get("categories", {})
        for name, row in category_rows.items():
            official_row = official_categories.get(str(row["category"]), {})
            row["official_mean_f1"] = float(official_row.get("mean_f1", 0.0))
    conversation_rows = [
        {"sample_id": sample_id, **metric_row(items)}
        for sample_id, items in sorted(by_conversation.items())
    ]
    overall = metric_row(combined)
    if official_evaluation:
        overall["official_mean_f1"] = float(official_evaluation.get("mean_f1", 0.0))
    overall["average_conversation_accuracy"] = mean(
        float(row["accuracy"]) for row in conversation_rows
    )
    return {
        "benchmark": "LoCoMo",
        "protocol": "Backboard-style binary LLM-as-a-Judge",
        "categories_included": [1, 2, 3, 4],
        "category_5_included": False,
        "judge": {
            "provider": "deepseek",
            "model": model,
            "base_url": base_url,
            "temperature": 0.1,
            "thinking": "disabled",
            "prompt_source": "Backboard-io/Backboard-Locomo-Benchmark",
        },
        "overall": overall,
        "by_question_type": category_rows,
        "per_conversation": conversation_rows,
        "questions": sorted(
            combined,
            key=lambda item: (str(item["sample_id"]), int(item["question_index"])),
        ),
    }


def percent(value: float) -> str:
    return f"{value * 100:.2f}%"


def render_markdown(summary: dict[str, Any]) -> str:
    overall = summary["overall"]
    lines = [
        "# LuminaCode LoCoMo Results",
        "",
        "## LLM Judge Evaluation",
        "",
        f"- Judge: `{summary['judge']['model']}` via `{summary['judge']['base_url']}`",
        f"- Questions evaluated: {overall['evaluated']}/{overall['total']}",
        f"- Correct answers: {overall['correct']}/{overall['evaluated']}",
        f"- Overall accuracy: **{percent(overall['accuracy'])}**",
        f"- Official token F1 diagnostic: {percent(overall.get('official_mean_f1', overall['runner_mean_f1']))}",
        f"- Average accuracy across conversations: {percent(overall['average_conversation_accuracy'])}",
        "",
        "## Breakdown By Question Type",
        "",
        "| Question Type | Questions | Correct | Accuracy | Token F1 |",
        "|---|---:|---:|---:|---:|",
    ]
    display_names = {
        "single_hop": "Single-Hop",
        "temporal_reasoning": "Temporal Reasoning",
        "multi_hop": "Multi-Hop",
        "open_domain": "Open Domain",
    }
    for name in ("single_hop", "multi_hop", "open_domain", "temporal_reasoning"):
        row = summary["by_question_type"].get(name)
        if not row:
            continue
        lines.append(
            f"| {display_names[name]} | {row['evaluated']} | {row['correct']} | "
            f"{percent(row['accuracy'])} | "
            f"{percent(row.get('official_mean_f1', row['runner_mean_f1']))} |"
        )
    lines.extend(
        [
            f"| **Overall** | **{overall['evaluated']}** | **{overall['correct']}** | "
            f"**{percent(overall['accuracy'])}** | "
            f"**{percent(overall.get('official_mean_f1', overall['runner_mean_f1']))}** |",
            "",
            "## Per-Conversation Accuracy",
            "",
            "| Conversation | Questions | Correct | Accuracy |",
            "|---|---:|---:|---:|",
        ]
    )
    for row in summary["per_conversation"]:
        lines.append(
            f"| {row['sample_id']} | {row['evaluated']} | {row['correct']} | {percent(row['accuracy'])} |"
        )
    lines.extend(
        [
            "",
            "## Runtime",
            "",
            f"- Average retrieval time: {overall['average_retrieval_seconds']:.2f}s",
            f"- Average answer time: {overall['average_response_seconds']:.2f}s",
            "",
            "> LLM Judge accuracy is the Backboard-compatible comparison metric. Token F1 is retained as a LoCoMo diagnostic and is not substituted for Judge accuracy.",
            "",
        ]
    )
    return "\n".join(lines)


def write_summary(
    source_records: list[dict[str, Any]],
    judged_by_key: dict[str, dict[str, Any]],
    output_dir: Path,
    model: str,
    base_url: str,
    official_evaluation: dict[str, Any] | None = None,
) -> dict[str, Any]:
    summary = aggregate_results(
        source_records, judged_by_key, model, base_url, official_evaluation
    )
    output_dir.mkdir(parents=True, exist_ok=True)
    summary_path = output_dir / "deepseek-locomo-results.json"
    report_path = output_dir / "deepseek-locomo-report.md"
    summary_path.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    report_path.write_text(render_markdown(summary), encoding="utf-8")
    compact = dict(summary)
    compact.pop("questions", None)
    (output_dir / "deepseek-locomo-summary.json").write_text(
        json.dumps(compact, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    return summary


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", required=True, help="LoCoMo checkpoint.jsonl")
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--base-url", default="https://api.deepseek.com")
    parser.add_argument("--model", default="deepseek-v4-pro")
    parser.add_argument("--parallel", type=int, default=16)
    parser.add_argument("--max-retries", type=int, default=7)
    parser.add_argument("--api-key-env", default="DEEPSEEK_API_KEY")
    parser.add_argument(
        "--official-evaluation",
        default="",
        help="snap-research official-evaluation.json; defaults to the input directory",
    )
    parser.add_argument(
        "--lumina-config",
        default=str(Path.home() / ".lumina" / "CONFIG" / "defaults.json"),
        help="fallback source for a matching API key; the key is never written to results",
    )
    args = parser.parse_args()

    source_records = [
        item
        for item in load_jsonl(Path(args.input))
        if int(item.get("category", 0)) in CATEGORY_NAMES
    ]
    source_by_key = {record_key(item): item for item in source_records}
    if len(source_by_key) != len(source_records):
        raise SystemExit("input contains duplicate sample_id/question_index records")

    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    judge_path = output_dir / "deepseek-judge.jsonl"
    judged_by_key = {
        record_key(item): item
        for item in load_jsonl(judge_path)
        if isinstance(item.get("judge"), dict) and "is_correct" in item["judge"]
    }
    official_path = (
        Path(args.official_evaluation)
        if args.official_evaluation
        else Path(args.input).parent / "official-evaluation.json"
    )
    official_evaluation = (
        json.loads(official_path.read_text(encoding="utf-8"))
        if official_path.exists()
        else None
    )
    pending = [item for key, item in source_by_key.items() if key not in judged_by_key]
    if not pending:
        summary = write_summary(
            source_records,
            judged_by_key,
            output_dir,
            args.model,
            args.base_url,
            official_evaluation,
        )
        print(json.dumps(summary["overall"], ensure_ascii=False, indent=2))
        return 0

    api_key = load_api_key(args.api_key_env, Path(args.lumina_config), args.base_url)
    if not api_key:
        raise SystemExit(
            f"{args.api_key_env} is required, or the matching key must exist in {args.lumina_config}"
        )

    lock = threading.Lock()
    thread_local = threading.local()
    error_counts: dict[str, int] = defaultdict(int)

    def client() -> OpenAI:
        if not hasattr(thread_local, "client"):
            thread_local.client = OpenAI(api_key=api_key, base_url=args.base_url, timeout=180.0)
        return thread_local.client

    def judge(source: dict[str, Any]) -> dict[str, Any]:
        prompt = ACCURACY_PROMPT.format(
            question=source.get("question", ""),
            expected_answer=source.get("answer", ""),
            generated_answer=source.get("prediction", ""),
        )
        last_error: Exception | None = None
        for attempt in range(max(1, args.max_retries)):
            try:
                response = client().chat.completions.create(
                    model=args.model,
                    messages=[
                        {"role": "system", "content": SYSTEM_PROMPT},
                        {"role": "user", "content": prompt},
                    ],
                    response_format={"type": "json_object"},
                    temperature=0.1,
                    max_tokens=300,
                    extra_body={"thinking": {"type": "disabled"}},
                )
                content = (response.choices[0].message.content or "").strip()
                is_correct, label, reasoning = parse_judge_content(content)
                result = dict(source)
                result["judge"] = {
                    "provider": "deepseek",
                    "model": args.model,
                    "base_url": args.base_url,
                    "label": label,
                    "is_correct": is_correct,
                    "reasoning": reasoning,
                }
                result["judged_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
                return result
            except openai.RateLimitError as exc:
                last_error = exc
                with lock:
                    error_counts["429"] += 1
            except openai.APITimeoutError as exc:
                last_error = exc
                with lock:
                    error_counts["timeout"] += 1
            except openai.APIConnectionError as exc:
                last_error = exc
                with lock:
                    error_counts["connection"] += 1
            except (openai.APIError, ValueError, RuntimeError) as exc:
                last_error = exc
                with lock:
                    error_counts["api_or_parse"] += 1
            if attempt + 1 < max(1, args.max_retries):
                time.sleep(min(math.pow(2, attempt), 30.0))
        raise RuntimeError(f"judge failed for {record_key(source)}: {last_error}")

    file_mode = "a" if judge_path.exists() else "w"
    failures: dict[str, str] = {}
    with judge_path.open(file_mode, encoding="utf-8") as output_handle:
        with ThreadPoolExecutor(max_workers=max(1, args.parallel)) as executor:
            futures = {executor.submit(judge, item): record_key(item) for item in pending}
            for future in as_completed(futures):
                key = futures[future]
                try:
                    result = future.result()
                except Exception as exc:  # Keep other independent judge calls running.
                    failures[key] = str(exc)
                    print(f"failed {key}: {exc}", flush=True)
                    continue
                with lock:
                    judged_by_key[key] = result
                    output_handle.write(json.dumps(result, ensure_ascii=False) + "\n")
                    output_handle.flush()
                    os.fsync(output_handle.fileno())
                    print(
                        f"evaluated {len(judged_by_key)}/{len(source_records)} {key} "
                        f"{result['judge']['label']}",
                        flush=True,
                    )

    summary = write_summary(
        source_records,
        judged_by_key,
        output_dir,
        args.model,
        args.base_url,
        official_evaluation,
    )
    if failures:
        (output_dir / "deepseek-judge-errors.json").write_text(
            json.dumps(
                {"failures": failures, "transient_error_counts": error_counts},
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
    print(json.dumps(summary["overall"], ensure_ascii=False, indent=2), flush=True)
    return 1 if summary["overall"]["evaluated"] != summary["overall"]["total"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
