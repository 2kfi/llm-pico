from __future__ import annotations

import os

import pytest
import pytest_asyncio

from core.db import close_db, init_db
from core.usage import (
    check_key_budget,
    get_cost_projection,
    get_key_month_spend,
    get_provider_cost_comparison,
    log_usage,
    reconcile_tokens,
    reserve_tokens,
)

pytestmark = [pytest.mark.asyncio, pytest.mark.usefixtures("db")]


@pytest_asyncio.fixture
async def db(tmp_path):
    db_path = os.path.join(tmp_path, "test.db")
    await init_db(db_path)
    yield
    await close_db()


async def _seed_usage(key_hash: str, model: str, provider: str, cost: float, tokens: int = 100):
    await log_usage(
        key_hash=key_hash,
        key_prefix="sk-...",
        model_name=model,
        provider=provider,
        prompt_tokens=tokens // 2,
        completion_tokens=tokens // 2,
        total_tokens=tokens,
        latency_ms=100,
        status_code=200,
        cost_usd=cost,
    )


async def _create_key_with_budget(key_hash: str, budget: float | None = None):
    """Insert a user_keys row with optional budget."""
    from core.db import get_db
    import time
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        await db.execute(
            """INSERT INTO user_keys (key_hash, key_prefix, is_active, created_at, monthly_budget_usd)
               VALUES (?, 'sk-...', 1, ?, ?)""",
            (key_hash, now, budget),
        )
        await db.commit()


# ---- Per-Key Budget Tests (4.1) ----


async def test_get_key_month_spend_empty():
    spend = await get_key_month_spend("nonexistent")
    assert spend == 0.0


async def test_get_key_month_spend_with_usage():
    await _seed_usage("key1", "gpt-4", "openai", 0.50)
    spend = await get_key_month_spend("key1")
    assert spend == pytest.approx(0.50)


async def test_check_key_budget_no_key():
    result = await check_key_budget("nonexistent", 1.0)
    assert result is None


async def test_check_key_budget_no_budget_set():
    await _create_key_with_budget("kb1", None)
    await _seed_usage("kb1", "gpt-4", "openai", 10.0)
    result = await check_key_budget("kb1", 5.0)
    assert result is None


async def test_check_key_budget_under_limit():
    await _create_key_with_budget("kb2", 20.0)
    await _seed_usage("kb2", "gpt-4", "openai", 10.0)
    result = await check_key_budget("kb2", 5.0)
    assert result is None


async def test_check_key_budget_exceeded():
    await _create_key_with_budget("kb3", 10.0)
    await _seed_usage("kb3", "gpt-4", "openai", 8.0)
    result = await check_key_budget("kb3", 5.0)
    assert result is not None
    "budget exceeded" in result.lower() or "Key budget exceeded" in result


async def test_check_key_budget_estimated_cost_none():
    await _create_key_with_budget("kb4", 5.0)
    result = await check_key_budget("kb4", None)
    assert result is None


# ---- Cost Projection Tests (4.2) ----


async def test_cost_projection_empty():
    proj = await get_cost_projection()
    assert proj["current_spend"] == 0.0
    assert proj["daily_rate"] == 0.0
    assert proj["projected_total"] == 0.0
    assert proj["days_elapsed"] > 0


async def test_cost_projection_with_spend():
    await _seed_usage("proj1", "gpt-4", "openai", 30.0)
    proj = await get_cost_projection()
    assert proj["current_spend"] == pytest.approx(30.0)
    assert proj["daily_rate"] > 0
    assert proj["projected_total"] > 0


# ---- Provider Cost Comparison Tests (4.3) ----


async def test_provider_cost_comparison_empty():
    result = await get_provider_cost_comparison()
    assert result == []


async def test_provider_cost_comparison():
    await _seed_usage("c1", "gpt-4", "openai", 0.50, 1000)
    await _seed_usage("c2", "gpt-4", "groq", 0.20, 1000)
    result = await get_provider_cost_comparison()
    assert len(result) == 2
    providers = {r["provider"] for r in result}
    assert "openai" in providers
    assert "groq" in providers


# ---- Token Budget Reservations Tests (4.5) ----


async def test_reserve_tokens_no_budget():
    result = await reserve_tokens("kh1", "gpt-4", None, None, 1000)
    assert result is None


async def test_reserve_tokens_key_budget_ok():
    await _create_key_with_budget("rb1", 100.0)
    result = await reserve_tokens("rb1", "gpt-4", None, None, 1000)
    assert result is None


async def test_reserve_tokens_key_budget_exceeded():
    await _create_key_with_budget("rb2", 0.0001)
    # Spend something to push over tiny budget
    await _seed_usage("rb2", "gpt-4", "openai", 0.00005, 100)
    # reservation is in tokens, converted to cost via /1M — tiny budget should fail
    result = await reserve_tokens("rb2", "gpt-4", None, None, 1000)
    assert result is not None
    assert "Key budget" in result


async def test_reconcile_tokens_noop():
    await reconcile_tokens("kh2", "gpt-4", None, None, 500, 500)
    # Should not raise
