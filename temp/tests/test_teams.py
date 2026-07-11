from __future__ import annotations

import os
import time

import pytest

from core.auth import verify_api_key
from core.config import Config, GeneralSettings, ModelEntry, LitellmParams, RouterSettings
import pytest_asyncio

from core.db import close_db, init_db, get_db
pytestmark = [pytest.mark.asyncio, pytest.mark.usefixtures("db")]

from core.teams import (
    check_user_budget,
    create_team,
    create_user,
    deactivate_team,
    get_team,
    get_team_month_spend,
    get_user_month_spend,
    get_teams,
    get_user,
    get_users,
    merge_allowlist,
    merge_limits,
    update_team_limits,
    update_user_budget,
    update_user_limits,
)
from core.usage import log_usage


@pytest_asyncio.fixture
async def db(tmp_path):
    db_path = os.path.join(tmp_path, "test.db")
    await init_db(db_path)
    yield
    await close_db()


async def _seed_key(key_hash: str, user_id: int | None = None):
    now = time.strftime("%Y-%m-%dT%H:%M:%S")
    async with get_db() as db:
        await db.execute(
            """INSERT INTO user_keys
               (key_hash, key_prefix, label, is_active, created_at, user_id)
               VALUES (?, ?, ?, 1, ?, ?)""",
            (key_hash, key_hash[:12], "test", now, user_id),
        )
        await db.commit()


@pytest.mark.asyncio
async def test_create_team():
    team = await create_team("Engineering", "Engineering team")
    assert team["name"] == "Engineering"
    assert team["description"] == "Engineering team"
    assert team["is_active"] is True

    teams = await get_teams()
    assert len(teams) == 1


@pytest.mark.asyncio
async def test_create_user():
    team = await create_team("Eng")
    user = await create_user(team["id"], "alice@example.com", "Alice")
    assert user["email"] == "alice@example.com"
    assert user["team_id"] == team["id"]
    assert user["is_active"] is True

    users = await get_users(team["id"])
    assert len(users) == 1


@pytest.mark.asyncio
async def test_update_team_limits():
    team = await create_team("Eng")
    updated = await update_team_limits(team["id"], {"rpm": 100, "rpd": 5000})
    assert updated

    t = await get_team(team["id"])
    assert t["rpm_limit"] == 100
    assert t["rpd_limit"] == 5000


@pytest.mark.asyncio
async def test_update_user_limits_and_budget():
    team = await create_team("Eng")
    user = await create_user(team["id"], "bob@example.com", "Bob")
    await update_user_limits(user["id"], {"rpm": 50})
    await update_user_budget(user["id"], 100.0)

    u = await get_user(user["id"])
    assert u["rpm_limit"] == 50
    assert u["monthly_budget_usd"] == 100.0


@pytest.mark.asyncio
async def test_check_user_budget():
    team = await create_team("Eng")
    user = await create_user(team["id"], "carol@example.com", "Carol")
    await update_user_budget(user["id"], 10.0)

    err = await check_user_budget(user["id"], 5.0)
    assert err is None

    await _seed_key("hash-carol", user["id"])
    await log_usage(
        key_hash="hash-carol", key_prefix="carol",
        model_name="gpt-4", provider="openai",
        prompt_tokens=0, completion_tokens=0, total_tokens=0,
        latency_ms=0, status_code=200, cost_usd=8.0,
    )

    err = await check_user_budget(user["id"], 5.0)
    assert err is not None
    assert "budget" in err.lower()


@pytest.mark.asyncio
async def test_check_user_budget_no_budget():
    team = await create_team("Eng")
    user = await create_user(team["id"], "dave@example.com", "Dave")

    err = await check_user_budget(user["id"], 5.0)
    assert err is None


@pytest.mark.asyncio
async def test_merge_limits():
    key_limits = {"rpm": 100, "rpd": None, "tpm": 50000, "tpd": None}
    user_row = {"rpm_limit": 50, "rpd_limit": 1000, "tpm_limit": None, "tpd_limit": None}
    team_row = {"rpm_limit": None, "rpd_limit": 2000, "tpm_limit": None, "tpd_limit": 500000}

    merged = merge_limits(key_limits, user_row, team_row)
    assert merged["rpm"] == 50
    assert merged["rpd"] == 1000
    assert merged["tpm"] == 50000
    assert merged["tpd"] == 500000


@pytest.mark.asyncio
async def test_merge_allowlist():
    merged = merge_allowlist(["gpt-4", "claude"], {"model_allowlist": '["gpt-4", "gemini"]'}, None)
    assert merged == ["gpt-4"]

    merged = merge_allowlist(None, {"model_allowlist": '["gpt-4"]'}, None)
    assert merged == ["gpt-4"]

    merged = merge_allowlist(None, None, None)
    assert merged is None


@pytest.mark.asyncio
async def test_deactivate_team_cascades():
    team = await create_team("Eng")
    user = await create_user(team["id"], "eve@example.com", "Eve")
    await _seed_key("hash-eve", user["id"])

    await deactivate_team(team["id"])

    t = await get_team(team["id"])
    assert t["is_active"] == 0

    u = await get_user(user["id"])
    assert u["is_active"] == 0

    async with get_db() as db:
        cursor = await db.execute("SELECT is_active FROM user_keys WHERE key_hash = ?", ("hash-eve",))
        row = await cursor.fetchone()
        assert row["is_active"] == 0


@pytest.mark.asyncio
async def test_month_spend():
    team = await create_team("Eng")
    user = await create_user(team["id"], "frank@example.com", "Frank")
    await _seed_key("hash-frank", user["id"])

    await log_usage(
        key_hash="hash-frank", key_prefix="frank",
        model_name="gpt-4", provider="openai",
        prompt_tokens=100, completion_tokens=200, total_tokens=300,
        latency_ms=50, status_code=200, cost_usd=0.05,
    )
    await log_usage(
        key_hash="hash-frank", key_prefix="frank",
        model_name="gpt-4", provider="openai",
        prompt_tokens=100, completion_tokens=200, total_tokens=300,
        latency_ms=50, status_code=200, cost_usd=0.03,
    )

    user_spend = await get_user_month_spend(user["id"])
    assert abs(user_spend - 0.08) < 0.001

    team_spend = await get_team_month_spend(team["id"])
    assert abs(team_spend - 0.08) < 0.001


@pytest.mark.asyncio
async def test_auth_resolves_hierarchy():
    from core.auth import hash_key, verify_api_key
    from core.config import Config, GeneralSettings

    team = await create_team("Eng")
    await update_team_limits(team["id"], {"rpm": 200})

    user = await create_user(team["id"], "grace@example.com", "Grace")
    await update_user_limits(user["id"], {"rpm": 100})

    raw_key = "sk-pico-test-hierarchy-key"
    key_hash = hash_key(raw_key)
    await _seed_key(key_hash, user["id"])

    cfg = Config(general_settings=GeneralSettings(master_key="sk-pico-master-test"))
    user_info = await verify_api_key(raw_key, "sk-pico-master-test")
    assert user_info is not None
    assert user_info["role"] == "user"
    assert user_info["user_id"] == user["id"]
    assert user_info["rpm_limit"] == 100
