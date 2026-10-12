from __future__ import annotations

import pytest

from core.cache import clear_cache, get_cached, set_cached


pytestmark = [pytest.mark.asyncio]


async def test_cache_set_and_get():
    body = b'{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'

    cached = await get_cached(body)
    assert cached is None

    await set_cached(body, b'{"choices":[{"message":{"content":"Hello!"}}]}')
    result = await get_cached(body)
    assert result is not None
    resp_body, content_type = result
    assert resp_body == b'{"choices":[{"message":{"content":"Hello!"}}]}'
    assert content_type == "application/json"


async def test_cache_key_different_bodies():
    b1 = b'{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'
    b2 = b'{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}'
    await set_cached(b1, b"r1")
    r1 = await get_cached(b1)
    r2 = await get_cached(b2)
    assert r1 is not None
    assert r2 is None


async def test_cache_expired():
    body = b'{"model":"gpt-4"}'
    await set_cached(body, b'response', ttl=-1)

    result = await get_cached(body)
    assert result is None


async def test_clear_cache():
    body = b'{"model":"gpt-4"}'
    await set_cached(body, b'response')

    await clear_cache()

    result = await get_cached(body)
    assert result is None
