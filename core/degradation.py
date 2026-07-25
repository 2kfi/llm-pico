from __future__ import annotations

import asyncio
import logging
from enum import Enum

_log = logging.getLogger("llm-pico.degradation")


class DegradationMode(Enum):
    NORMAL = "normal"
    REJECT = "reject"
    QUEUE = "queue"
    FALLBACK_ONLY = "fallback_only"


class DegradationManager:
    def __init__(self, mode: DegradationMode = DegradationMode.NORMAL) -> None:
        self.mode = mode
        self._queue: asyncio.Queue = asyncio.Queue(maxsize=1000)

    def set_mode(self, mode: DegradationMode) -> None:
        _log.info("degradation mode: %s -> %s", self.mode.value, mode.value)
        self.mode = mode

    async def handle(self, model_name: str, handler):
        if self.mode == DegradationMode.NORMAL:
            return await handler()
        if self.mode == DegradationMode.REJECT:
            from fastapi import HTTPException
            raise HTTPException(status_code=503, detail={
                "error": {"message": "Service degraded — requests temporarily rejected", "type": "degraded", "code": 503}
            })
        if self.mode == DegradationMode.QUEUE:
            await self._queue.put((model_name, handler))
            return await self._queue.get()
        if self.mode == DegradationMode.FALLBACK_ONLY:
            return await handler()
        return await handler()

    @property
    def queue_depth(self) -> int:
        return self._queue.qsize()
