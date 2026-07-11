from __future__ import annotations

import asyncio
import time

import pytest

from core.ratelimit import RateLimiter, _global_limiter


@pytest.fixture(autouse=True)
def reset_limiter():
    global _global_limiter
    _global_limiter = None
    yield


@pytest.mark.asyncio
async def test_rpm_allows_within_limit():
    limiter = RateLimiter()
    limits = {"_level": "user", "rpm": 5}

    for _ in range(5):
        rejected = await limiter.check_and_reserve("hash-a", "model-x", limits, reservation=1)
        assert rejected is None, f"unexpected rejection at iteration {_}"


@pytest.mark.asyncio
async def test_rpm_rejects_over_limit():
    limiter = RateLimiter()
    limits = {"_level": "user", "rpm": 3}

    for _ in range(3):
        rejected = await limiter.check_and_reserve("hash-b", "model-x", limits, reservation=1)
        assert rejected is None

    rejected = await limiter.check_and_reserve("hash-b", "model-x", limits, reservation=1)
    assert rejected is not None
    assert rejected["exceeded"] == "rpm"


@pytest.mark.asyncio
async def test_rpm_separate_keys():
    limiter = RateLimiter()
    limits = {"_level": "user", "rpm": 2}

    for _ in range(2):
        assert await limiter.check_and_reserve("hash-a", "model-x", limits, 1) is None
        assert await limiter.check_and_reserve("hash-b", "model-x", limits, 1) is None

    # hash-a should be at limit
    assert await limiter.check_and_reserve("hash-a", "model-x", limits, 1) is not None
    # hash-b should also be at limit (separate counter)
    assert await limiter.check_and_reserve("hash-b", "model-x", limits, 1) is not None
    # hash-c should still have capacity
    assert await limiter.check_and_reserve("hash-c", "model-x", limits, 1) is None


@pytest.mark.asyncio
async def test_reservation_consumes_multiple():
    limiter = RateLimiter()
    limits = {"_level": "user", "rpm": 10}

    rejected = await limiter.check_and_reserve("hash-c", "model-x", limits, reservation=7)
    assert rejected is None

    rejected = await limiter.check_and_reserve("hash-c", "model-x", limits, reservation=7)
    assert rejected is not None
    assert rejected["exceeded"] == "rpm"


@pytest.mark.asyncio
async def test_reconcile_adjusts_tpm(tmp_path):
    import core.db as db
    await db.init_db(str(tmp_path / "test.db"))
    try:
        limiter = RateLimiter()
        limits = {"_level": "user", "tpm": 100}

        rejected = await limiter.check_and_reserve("hash-d", "model-x", limits, reservation=50)
        assert rejected is None

        # Reconcile down to 30 tokens used (saved 20)
        await limiter.reconcile("hash-d", "model-x", limits, actual_tokens=30, reserved_tokens=50)

        # Should have room for 80 more (100 - 50 + 20 = 70, so 70 available now)
        # After reconcile: count = 30. 30 + 70 = 100 = limit. So 70 should fit.
        rejected = await limiter.check_and_reserve("hash-d", "model-x", limits, reservation=70)
        assert rejected is None

        # 71 should exceed (30+71=101>100)
        rejected = await limiter.check_and_reserve("hash-d", "model-x", limits, reservation=71)
        assert rejected is not None
    finally:
        await db.close_db()


@pytest.mark.asyncio
async def test_user_and_model_levels_separate():
    limiter = RateLimiter()
    user_limits = {"_level": "user", "rpm": 3}
    model_limits = {"_level": "model", "rpm": 2}

    # Check both levels
    for l in (user_limits, model_limits):
        rej = await limiter.check_and_reserve("hash-e", "model-x", l, reservation=1)
        assert rej is None

    for l in (user_limits, model_limits):
        rej = await limiter.check_and_reserve("hash-e", "model-x", l, reservation=1)
        assert rej is None

    # model limit should be hit (2 of 2 used)
    for l in (user_limits, model_limits):
        rej = await limiter.check_and_reserve("hash-e", "model-x", l, reservation=1)
        if l["_level"] == "model":
            assert rej is not None
        else:
            assert rej is None


@pytest.mark.asyncio
async def test_ash_allows_within_limit():
    limiter = RateLimiter()
    limits = {"_level": "user", "ash": 5}

    for _ in range(5):
        rejected = await limiter.check_and_reserve("hash-ash-a", "model-x", limits, reservation=1)
        assert rejected is None


@pytest.mark.asyncio
async def test_ash_rejects_over_limit():
    limiter = RateLimiter()
    limits = {"_level": "user", "ash": 3}

    for _ in range(3):
        assert await limiter.check_and_reserve("hash-ash-b", "model-x", limits, 1) is None

    rejected = await limiter.check_and_reserve("hash-ash-b", "model-x", limits, 1)
    assert rejected is not None
    assert rejected["exceeded"] == "ash"
    assert rejected["retry_after"] == 3600  # ash is per-hour


@pytest.mark.asyncio
async def test_asd_allows_within_limit(tmp_path):
    import core.db as db
    await db.init_db(str(tmp_path / "test_asd.db"))
    try:
        limiter = RateLimiter()
        limits = {"_level": "user", "asd": 5}

        for _ in range(5):
            rejected = await limiter.check_and_reserve("hash-asd-a", "model-x", limits, reservation=1)
            assert rejected is None
    finally:
        await db.close_db()


@pytest.mark.asyncio
async def test_asd_rejects_over_limit(tmp_path):
    import core.db as db
    await db.init_db(str(tmp_path / "test_asd.db2"))
    try:
        limiter = RateLimiter()
        limits = {"_level": "user", "asd": 3}

        for _ in range(3):
            assert await limiter.check_and_reserve("hash-asd-b", "model-x", limits, 1) is None

        rejected = await limiter.check_and_reserve("hash-asd-b", "model-x", limits, 1)
        assert rejected is not None
        assert rejected["exceeded"] == "asd"
    finally:
        await db.close_db()
