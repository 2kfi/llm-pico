from __future__ import annotations

import asyncio
import json
from typing import AsyncIterator


async def parse_stream_usage(chunk: bytes) -> dict | None:
    """Extract usage from an SSE data chunk if present."""
    if b"data: " not in chunk or b"usage" not in chunk:
        return None
    try:
        text = chunk.decode("utf-8", errors="replace")
        for line in text.split("\n"):
            if line.startswith("data: ") and "[DONE]" not in line:
                data = json.loads(line[6:])
                if "usage" in data:
                    return data["usage"]
    except (json.JSONDecodeError, UnicodeDecodeError):
        pass
    return None


async def heartbeat_generator(interval: float = 15.0) -> AsyncIterator[bytes]:
    """Yield SSE keepalive comments at `interval` seconds."""
    while True:
        await asyncio.sleep(interval)
        yield b": keepalive\n\n"


async def merge_streams(
    *streams: AsyncIterator[bytes],
) -> AsyncIterator[bytes]:
    """Merge multiple async generators, yielding chunks as they arrive."""
    queues: list[asyncio.Queue[bytes | None]] = []
    tasks: list[asyncio.Task] = []

    async def _pump(idx: int, stream: AsyncIterator[bytes]):
        try:
            async for chunk in stream:
                await queues[idx].put(chunk)
        except (asyncio.CancelledError, GeneratorExit):
            pass
        finally:
            await queues[idx].put(None)

    for i, s in enumerate(streams):
        q: asyncio.Queue[bytes | None] = asyncio.Queue()
        queues.append(q)
        tasks.append(asyncio.create_task(_pump(i, s)))

    try:
        active = len(streams)
        while active > 0:
            # Round-robin through queues
            for q in queues:
                try:
                    chunk = q.get_nowait()
                except asyncio.QueueEmpty:
                    continue
                if chunk is None:
                    active -= 1
                    continue
                yield chunk
            if active > 0:
                await asyncio.sleep(0.001)
    finally:
        for t in tasks:
            if not t.done():
                t.cancel()
        for t in tasks:
            try:
                await t
            except asyncio.CancelledError:
                pass
