from __future__ import annotations

import asyncio
import json
from typing import Any

_subs: set[asyncio.Queue] = set()


def emit(event: dict[str, Any]) -> None:
    payload = json.dumps(event)
    for q in list(_subs):
        try:
            q.put_nowait(payload)
        except asyncio.QueueFull:
            try:
                q.get_nowait()
                q.put_nowait(payload)
            except (asyncio.QueueEmpty, asyncio.QueueFull):
                pass


def subscribe() -> asyncio.Queue:
    q: asyncio.Queue = asyncio.Queue(maxsize=256)
    _subs.add(q)
    return q


def unsubscribe(q: asyncio.Queue) -> None:
    _subs.discard(q)
