"""Run snapshot 加载 cache. 内存 LRU 以防同一 worker 反复读同一快照."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any, Dict


class SnapshotCache:
    """延迟加载快照文件 + sha256 校验."""

    def __init__(self, max_entries: int = 8):
        self._cache: Dict[str, dict] = {}
        self._max_entries = max_entries

    def get(self, path: str, expected_sha256_hex: str | None = None) -> dict:
        """读快照: 命中 cache 直接返回; 否则读盘 + 校验 + 存 cache."""
        key = f"{path}:{expected_sha256_hex or ''}"
        doc = self._cache.get(key)
        if doc is not None:
            return doc
        raw = Path(path).read_text(encoding="utf-8")
        if expected_sha256_hex:
            actual = hashlib.sha256(raw.encode("utf-8")).hexdigest()
            if actual != expected_sha256_hex:
                raise RuntimeError(f"snapshot hash mismatch at {path}: {actual} != {expected_sha256_hex}")
        doc = json.loads(raw, parse_float=float)
        if len(self._cache) >= self._max_entries:
            self._cache.pop(next(iter(self._cache)))  # FIFO 边加边淘汰
        self._cache[key] = doc
        return doc

    def invalidate(self) -> None:
        self._cache.clear()
