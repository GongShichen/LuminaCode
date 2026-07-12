#!/usr/bin/env python3
from pathlib import Path
import sys


LUMINA_ROOT = Path(__file__).resolve().parents[2]
V2_ROOT = Path(__file__).resolve().parents[3] / "LongMemEval-V2"
for path in (str(LUMINA_ROOT), str(V2_ROOT)):
    if path not in sys.path:
        sys.path.insert(0, path)

from benchmark.longmemevalv2.lumina_memory import LuminaMemory  # noqa: E402,F401
from evaluation.harness import main  # noqa: E402


if __name__ == "__main__":
    main()
