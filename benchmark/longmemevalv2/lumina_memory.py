from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import threading
import uuid

from memory_modules.memory import Memory, MemoryContextItem, register_memory, require


@register_memory
class LuminaMemory(Memory):
    memory_type = "lumina"

    def __init__(self, memory_params: dict[str, object]) -> None:
        super().__init__(memory_params)
        bridge_binary = str(memory_params.get("bridge_binary", "")).strip()
        store_path = str(memory_params.get("store_path", "")).strip()
        project_root = str(memory_params.get("project_root", "")).strip()
        require(Path(bridge_binary).is_file(), f"Missing Lumina bridge: {bridge_binary}")
        require(store_path, "lumina store_path is required")
        require(project_root, "lumina project_root is required")
        self._lock = threading.Lock()
        self._last_meta: dict[str, object] = {}
        stderr_path = Path(str(memory_params.get("bridge_stderr", store_path + ".bridge.log")))
        stderr_path.parent.mkdir(parents=True, exist_ok=True)
        self._stderr = stderr_path.open("a", encoding="utf-8")
        self._process = subprocess.Popen(
            [bridge_binary, "--store", store_path, "--project-root", project_root],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=self._stderr,
            text=True,
            bufsize=1,
            env=os.environ.copy(),
        )
        self._call("ping")

    def _call(self, operation: str, **payload: object) -> dict[str, object]:
        request_id = uuid.uuid4().hex
        request = {"id": request_id, "op": operation, **payload}
        with self._lock:
            require(self._process.poll() is None, "Lumina bridge exited unexpectedly")
            assert self._process.stdin is not None
            assert self._process.stdout is not None
            self._process.stdin.write(json.dumps(request, ensure_ascii=False) + "\n")
            self._process.stdin.flush()
            line = self._process.stdout.readline()
        require(bool(line), "Lumina bridge returned EOF")
        response = json.loads(line)
        require(response.get("id") == request_id, "Lumina bridge response ID mismatch")
        require(response.get("ok") is True, str(response.get("error", "Lumina bridge failed")))
        self._last_meta = dict(response.get("meta") or {})
        return response

    def insert(self, trajectory: dict[str, object]) -> None:
        self._call("insert", trajectory=trajectory)

    def query(self, query: str, query_image: str | None = None) -> list[MemoryContextItem]:
        response = self._call("query", query=query, query_image=query_image or "")
        context = str(response.get("context", "")).strip()
        if not context:
            return []
        return [{"type": "text", "value": context}]

    def post_query_hook(
        self,
        *,
        query: str,
        query_image: str | None,
        memory_context: list[MemoryContextItem],
    ) -> dict[str, object] | None:
        return {"lumina_retrieval": dict(self._last_meta)}

    def close(self) -> None:
        process = getattr(self, "_process", None)
        if process is None or process.poll() is not None:
            return
        try:
            self._call("close")
        finally:
            process.wait(timeout=10)
            self._stderr.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass
