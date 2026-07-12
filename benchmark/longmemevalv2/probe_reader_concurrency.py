#!/usr/bin/env python3
from __future__ import annotations

import argparse
import asyncio
import json
import math
import statistics
import time

from openai import AsyncOpenAI


def percentile(values: list[float], quantile: float) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    index = min(len(ordered) - 1, max(0, math.ceil(len(ordered) * quantile) - 1))
    return ordered[index]


async def run_level(
    client: AsyncOpenAI,
    *,
    model: str,
    concurrency: int,
    requests: int,
    prompt: str,
    max_tokens: int,
) -> dict[str, object]:
    semaphore = asyncio.Semaphore(concurrency)

    async def one(index: int) -> dict[str, object]:
        async with semaphore:
            started = time.perf_counter()
            try:
                response = await client.chat.completions.create(
                    model=model,
                    messages=[
                        {"role": "system", "content": "Answer using the supplied experience. Return a concise boxed answer."},
                        {"role": "user", "content": prompt + f"\nRequest marker: {index}\nQuestion: What workflow was observed?"},
                    ],
                    max_tokens=max_tokens,
                    temperature=0,
                    extra_body={"chat_template_kwargs": {"enable_thinking": False}},
                )
                elapsed = time.perf_counter() - started
                usage = response.usage
                return {
                    "ok": True,
                    "latency": elapsed,
                    "prompt_tokens": int(getattr(usage, "prompt_tokens", 0) or 0),
                    "completion_tokens": int(getattr(usage, "completion_tokens", 0) or 0),
                }
            except Exception as exc:  # noqa: BLE001 - probe must retain provider errors
                return {"ok": False, "latency": time.perf_counter() - started, "error": repr(exc)}

    wall_started = time.perf_counter()
    rows = await asyncio.gather(*(one(index) for index in range(requests)))
    wall = time.perf_counter() - wall_started
    successes = [row for row in rows if row["ok"]]
    latencies = [float(row["latency"]) for row in successes]
    completion_tokens = sum(int(row.get("completion_tokens", 0)) for row in successes)
    errors: dict[str, int] = {}
    for row in rows:
        if row["ok"]:
            continue
        key = str(row.get("error", "unknown"))
        errors[key] = errors.get(key, 0) + 1
    return {
        "concurrency": concurrency,
        "requests": requests,
        "successes": len(successes),
        "success_rate": len(successes) / requests,
        "wall_seconds": wall,
        "latency_p50_seconds": statistics.median(latencies) if latencies else 0,
        "latency_p95_seconds": percentile(latencies, 0.95),
        "latency_max_seconds": max(latencies, default=0),
        "completion_tokens_per_second": completion_tokens / wall if wall > 0 else 0,
        "prompt_tokens_per_request": successes[0]["prompt_tokens"] if successes else 0,
        "errors": errors,
    }


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--levels", default="1,2,4,8,12,16,20,24,32")
    parser.add_argument("--requests-multiplier", type=int, default=2)
    parser.add_argument("--prompt-tokens", type=int, default=6000)
    parser.add_argument("--max-tokens", type=int, default=256)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    paragraph = (
        "Observed experience: open the workspace, inspect the current state, verify the exact value, "
        "perform the requested action, check the resulting state, and retain any environment-specific warning. "
    )
    prompt = (paragraph * max(1, args.prompt_tokens // 28))[: args.prompt_tokens * 5]
    client = AsyncOpenAI(api_key="EMPTY", base_url=args.base_url, timeout=1800)
    results = []
    try:
        for level in [int(value) for value in args.levels.split(",") if value.strip()]:
            result = await run_level(
                client,
                model=args.model,
                concurrency=level,
                requests=max(level * args.requests_multiplier, 8),
                prompt=prompt,
                max_tokens=args.max_tokens,
            )
            results.append(result)
            print(json.dumps(result, ensure_ascii=False), flush=True)
            if float(result["success_rate"]) < 1.0:
                break
    finally:
        await client.close()
    with open(args.output, "w", encoding="utf-8") as output:
        json.dump({"model": args.model, "base_url": args.base_url, "levels": results}, output, indent=2)
        output.write("\n")


if __name__ == "__main__":
    asyncio.run(main())
