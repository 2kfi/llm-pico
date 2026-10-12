from __future__ import annotations

import asyncio
import hashlib
import json
import os
import time

import pytest

os.environ.setdefault("LLM_PICO_DB", ":memory:")

from core.db import get_db, init_db, close_db
from core.usage import classify_error, log_usage, get_error_stats
from core.profiler import LatencyTracker
from core.sampling import maybe_sample


@pytest.fixture
async def db_setup(tmp_path):
    db_path = str(tmp_path / "test.db")
    await init_db(db_path)
    yield
    await close_db()


# ---- classify_error (sync) ----

def test_classify_rate_limit():
    assert classify_error(429, {}) == "rate_limit"

def test_classify_invalid_key():
    assert classify_error(401, {}) == "invalid_key"

def test_classify_quota_exceeded():
    assert classify_error(402, {}) == "quota_exceeded"

def test_classify_model_overloaded():
    assert classify_error(503, {}) == "model_overloaded"

def test_classify_timeout():
    assert classify_error(504, {}) == "timeout"

def test_classify_content_filter():
    assert classify_error(500, {"error": {"code": "content_filter"}}) == "content_filter"

def test_classify_content_filter_in_str():
    assert classify_error(400, "content_filter blocked") == "content_filter"

def test_classify_unknown():
    assert classify_error(500, {}) == "unknown"

def test_classify_success_not_error():
    assert classify_error(200, {}) == "unknown"


# ---- LatencyTracker (sync) ----

def test_latency_tracker_basic():
    t = LatencyTracker(window_size=10)
    t.record("gpt-4", "openai", 100)
    t.record("gpt-4", "openai", 200)
    t.record("gpt-4", "openai", 300)
    assert t.get_p50("gpt-4", "openai") == 200

def test_latency_tracker_p99():
    t = LatencyTracker(window_size=100)
    for i in range(100):
        t.record("gpt-4", "openai", i)
    p99 = t.get_p99("gpt-4", "openai")
    assert p99 >= 97

def test_latency_tracker_empty():
    t = LatencyTracker()
    assert t.get_p50("x", "y") == 0
    assert t.get_p99("x", "y") == 0
    assert t.is_slow("x", "y", 100) is False

def test_latency_tracker_is_slow():
    t = LatencyTracker(window_size=10)
    for _ in range(10):
        t.record("gpt-4", "openai", 100)
    assert t.is_slow("gpt-4", "openai", 200) is True
    assert t.is_slow("gpt-4", "openai", 100) is False

def test_latency_tracker_window_eviction():
    t = LatencyTracker(window_size=3)
    t.record("m", "p", 10)
    t.record("m", "p", 20)
    t.record("m", "p", 30)
    t.record("m", "p", 40)
    buf = t._samples["m:p"]
    assert len(buf) == 3
    assert buf[0] == 20

def test_latency_tracker_clear():
    t = LatencyTracker()
    t.record("m", "p", 100)
    t.clear()
    assert t.get_p50("m", "p") == 0


# ---- Sampling (async, needs db) ----

async def test_sampling_disabled_by_default(db_setup):
    await maybe_sample("req1", "gpt-4", "hello", "world", sampling_rate=0.0)
    async with get_db() as db:
        cursor = await db.execute("SELECT COUNT(*) as cnt FROM request_samples")
        row = await cursor.fetchone()
        assert row["cnt"] == 0

async def test_sampling_stores_when_rate_1(db_setup):
    await maybe_sample("req2", "gpt-4", "hello world", "response text", sampling_rate=1.0)
    async with get_db() as db:
        cursor = await db.execute("SELECT * FROM request_samples WHERE request_id = 'req2'")
        row = await cursor.fetchone()
        assert row is not None
        assert row["model"] == "gpt-4"
        assert row["prompt_preview"] == "hello world"
        assert row["response_preview"] == "response text"
        expected_hash = hashlib.sha256(b"hello world").hexdigest()[:16]
        assert row["prompt_hash"] == expected_hash

async def test_sampling_truncates_long_text(db_setup):
    long_prompt = "x" * 500
    long_response = "y" * 500
    await maybe_sample("req3", "gpt-4", long_prompt, long_response, sampling_rate=1.0)
    async with get_db() as db:
        cursor = await db.execute("SELECT * FROM request_samples WHERE request_id = 'req3'")
        row = await cursor.fetchone()
        assert len(row["prompt_preview"]) == 200
        assert len(row["response_preview"]) == 200


# ---- log_usage with error_type (async, needs db) ----

async def test_log_usage_with_error_type(db_setup):
    rid = await log_usage(
        key_hash="abc123",
        key_prefix="sk-tes",
        model_name="gpt-4",
        provider="openai",
        prompt_tokens=10,
        completion_tokens=5,
        total_tokens=15,
        latency_ms=200,
        status_code=429,
        error="rate limited",
        cost_usd=None,
        error_type="rate_limit",
    )
    assert len(rid) == 16

    async with get_db() as db:
        cursor = await db.execute(
            "SELECT error_type FROM usage_log WHERE request_id = ?", (rid,)
        )
        row = await cursor.fetchone()
        assert row["error_type"] == "rate_limit"


# ---- get_error_stats (async, needs db) ----

async def test_get_error_stats(db_setup):
    await log_usage("h1", "sk-t1", "gpt-4", "openai", 10, 5, 15, 100, 429, error_type="rate_limit")
    await log_usage("h1", "sk-t1", "gpt-4", "openai", 10, 5, 15, 100, 429, error_type="rate_limit")
    await log_usage("h1", "sk-t1", "claude-3", "anthropic", 10, 5, 15, 100, 401, error_type="invalid_key")

    stats = await get_error_stats()
    assert len(stats) >= 2
    types = {s["error_type"] for s in stats}
    assert "rate_limit" in types
    assert "invalid_key" in types

async def test_get_error_stats_by_provider(db_setup):
    await log_usage("h2", "sk-t2", "gpt-4", "openai", 10, 5, 15, 100, 429, error_type="rate_limit")
    await log_usage("h2", "sk-t2", "claude-3", "anthropic", 10, 5, 15, 100, 429, error_type="rate_limit")

    stats = await get_error_stats(provider="openai")
    assert all(s["provider"] == "openai" for s in stats)

async def test_get_error_stats_excludes_unknown(db_setup):
    await log_usage("h3", "sk-t3", "gpt-4", "openai", 10, 5, 15, 100, 500)
    stats = await get_error_stats()
    assert len(stats) == 0
