from __future__ import annotations

import os

import pytest
import pytest_asyncio

from core.cache import clear_cache, get_cached, make_cache_key, set_cached
from core.db import close_db, init_db


pytestmark = [pytest.mark.asyncio, pytest.mark.usefixtures("db")]


@pytest_asyncio.fixture
async def db(tmp_path):
    db_path = os.path.join(tmp_path, "test.db")
    await init_db(db_path)
    yield
    await close_db()


async def test_cache_set_and_get():
    body = b'{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'
    ck = make_cache_key(body)

    cached = await get_cached(ck)
    assert cached is None

    await set_cached(ck, b'{"choices":[{"message":{"content":"Hello!"}}]}')
    result = await get_cached(ck)
    assert result is not None
    resp_body, content_type = result
    assert resp_body == b'{"choices":[{"message":{"content":"Hello!"}}]}'
    assert content_type == "application/json"


async def test_cache_key_different_bodies():
    ck1 = make_cache_key(b'{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}')
    ck2 = make_cache_key(b'{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}')
    assert ck1 != ck2


async def test_cache_expired():
    body = b'{"model":"gpt-4"}'
    ck = make_cache_key(body)
    await set_cached(ck, b'response', ttl=-1)

    result = await get_cached(ck)
    assert result is None


async def test_clear_cache():
    body = b'{"model":"gpt-4"}'
    ck = make_cache_key(body)
    await set_cached(ck, b'response')

    cleared = await clear_cache()
    assert cleared > 0

    result = await get_cached(ck)
    assert result is None
