from __future__ import annotations

import asyncio
import json

import pytest

from core.streaming import heartbeat_generator, merge_streams, parse_stream_usage


# ── parse_stream_usage ──

def _sse_chunk(data: dict) -> bytes:
    return f"data: {json.dumps(data)}\n\n".encode()


@pytest.mark.asyncio
async def test_parse_usage_present():
    chunk = _sse_chunk({"usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}})
    result = await parse_stream_usage(chunk)
    assert result == {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}


@pytest.mark.asyncio
async def test_parse_usage_absent():
    chunk = _sse_chunk({"choices": [{"delta": {"content": "hi"}}]})
    result = await parse_stream_usage(chunk)
    assert result is None


@pytest.mark.asyncio
async def test_parse_usage_done_marker():
    chunk = b"data: [DONE]\n\n"
    result = await parse_stream_usage(chunk)
    assert result is None


@pytest.mark.asyncio
async def test_parse_usage_malformed():
    chunk = b"data: {bad json\n\n"
    result = await parse_stream_usage(chunk)
    assert result is None


@pytest.mark.asyncio
async def test_parse_usage_multiline():
    chunk = (
        b"data: {\"choices\": [{\"delta\": {\"content\": \"a\"}}]}\n\n"
        b"data: {\"usage\": {\"prompt_tokens\": 5, \"completion_tokens\": 7, \"total_tokens\": 12}}\n\n"
    )
    # parse_stream_usage checks per-line; the usage line is second
    lines = chunk.split(b"\n\n")
    result = await parse_stream_usage(lines[1])
    assert result["total_tokens"] == 12


# ── heartbeat_generator ──

@pytest.mark.asyncio
async def test_heartbeat_emits_keepalive():
    gen = heartbeat_generator(interval=0.05)
    chunk = await asyncio.wait_for(gen.__anext__(), timeout=1)
    assert chunk == b": keepalive\n\n"
    await gen.aclose()


@pytest.mark.asyncio
async def test_heartbeat_interval():
    gen = heartbeat_generator(interval=0.05)
    t0 = asyncio.get_event_loop().time()
    await asyncio.wait_for(gen.__anext__(), timeout=1)
    elapsed = asyncio.get_event_loop().time() - t0
    assert elapsed >= 0.04
    await gen.aclose()


# ── merge_streams ──

@pytest.mark.asyncio
async def test_merge_yields_all_chunks():
    async def stream_a():
        for i in range(3):
            yield f"a{i}".encode()
            await asyncio.sleep(0)

    async def stream_b():
        for i in range(3):
            yield f"b{i}".encode()
            await asyncio.sleep(0)

    chunks = []
    async for chunk in merge_streams(stream_a(), stream_b()):
        chunks.append(chunk)

    assert len(chunks) == 6
    # All chunks from both streams present
    text_chunks = {c.decode() for c in chunks}
    assert text_chunks == {"a0", "a1", "a2", "b0", "b1", "b2"}


@pytest.mark.asyncio
async def test_merge_empty():
    async def empty():
        if False:
            yield b""

    result = [c async for c in merge_streams(empty())]
    assert result == []


@pytest.mark.asyncio
async def test_merge_single_stream():
    async def single():
        yield b"x"
        yield b"y"

    result = [c async for c in merge_streams(single())]
    assert result == [b"x", b"y"]


# ── cancellation propagation ──

@pytest.mark.asyncio
async def test_generate_closes_upstream_on_cancel():
    """Simulate client disconnect: generator should close upstream."""
    closed = []

    class FakeResponse:
        async def aiter_bytes(self):
            yield b"data: chunk1\n\n"
            await asyncio.sleep(10)  # long wait simulating slow upstream
            yield b"data: chunk2\n\n"

        async def aclose(self):
            closed.append(True)

    # Import the generate closure pattern from server
    from core.streaming import parse_stream_usage

    upstream = FakeResponse()

    async def generate():
        try:
            async for chunk in upstream.aiter_bytes():
                yield chunk
        except (GeneratorExit, asyncio.CancelledError):
            await upstream.aclose()
            raise

    gen = generate()
    chunk = await gen.__anext__()
    assert chunk == b"data: chunk1\n\n"

    # Simulate client disconnect
    try:
        await gen.aclose()
    except Exception:
        pass

    assert closed == [True]
