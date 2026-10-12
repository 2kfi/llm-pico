from __future__ import annotations

import time
from collections import defaultdict


class LatencyTracker:
    def __init__(self, window_size: int = 1000):
        self._window_size = window_size
        self._samples: dict[str, list[int]] = defaultdict(list)

    def _key(self, model: str, provider: str) -> str:
        return f"{model}:{provider}"

    def record(self, model: str, provider: str, latency_ms: int) -> None:
        k = self._key(model, provider)
        buf = self._samples[k]
        if len(buf) >= self._window_size:
            buf.pop(0)
        buf.append(latency_ms)

    def get_p50(self, model: str, provider: str) -> int:
        buf = self._samples.get(self._key(model, provider))
        if not buf:
            return 0
        s = sorted(buf)
        return s[len(s) // 2]

    def get_p99(self, model: str, provider: str) -> int:
        buf = self._samples.get(self._key(model, provider))
        if not buf:
            return 0
        s = sorted(buf)
        idx = max(0, int(len(s) * 0.99) - 1)
        return s[idx]

    def is_slow(self, model: str, provider: str, latency_ms: int) -> bool:
        p99 = self.get_p99(model, provider)
        return latency_ms > p99 * 1.5 if p99 else False

    def clear(self) -> None:
        self._samples.clear()


_latency_tracker = LatencyTracker()
