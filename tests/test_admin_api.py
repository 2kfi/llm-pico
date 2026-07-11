from __future__ import annotations

import json
import os
from unittest.mock import patch, AsyncMock

import httpx
import pytest
import pytest_asyncio

from api.server import create_app
from core.config import Config, GeneralSettings, LitellmParams, ModelEntry, RouterSettings
from core.db import close_db, init_db, get_db

MASTER_KEY = "sk-pico-master-test"


@pytest_asyncio.fixture
async def app(tmp_path):
    """Create a fully initialized FastAPI app with temp DB for admin API testing."""
    db_path = str(tmp_path / "test.db")
    await init_db(db_path)

    config = Config(
        general_settings=GeneralSettings(master_key=MASTER_KEY),
        router_settings=RouterSettings(num_retries=2),
        model_list=[
            ModelEntry(
                model_name="test-model",
                litellm_params=LitellmParams(
                    model="openai/gpt-4",
                    api_key="sk-test-key-1",
                    api_base="https://api.openai.com/v1",
                ),
            ),
        ],
    )

    application = create_app({"config": config})

    yield application

    await close_db()


@pytest_asyncio.fixture
async def client(app):
    """Async HTTP client bound to the test app (no lifespan)."""
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://testserver") as c:
        yield c


def _auth() -> dict[str, str]:
    return {"Authorization": f"Bearer {MASTER_KEY}"}


def _no_auth() -> dict[str, str]:
    return {}


# ---------------------------------------------------------------------------
# 1. Health check (no auth needed)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_health_check(client):
    resp = await client.get("/health")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok"
    assert "version" in body


# ---------------------------------------------------------------------------
# 2. List keys — with and without auth
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_list_keys_with_auth(client):
    resp = await client.get("/admin/keys", headers=_auth())
    assert resp.status_code == 200
    body = resp.json()
    assert "keys" in body
    assert "total" in body
    assert isinstance(body["keys"], list)


@pytest.mark.asyncio
async def test_list_keys_without_auth(client):
    resp = await client.get("/admin/keys", headers=_no_auth())
    assert resp.status_code == 401
    body = resp.json()
    assert body["error"]["code"] == 401


# ---------------------------------------------------------------------------
# 3. Create key — verify response and storage
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_create_key(client):
    resp = await client.post(
        "/admin/keys",
        headers=_auth(),
        json={"label": "test-key", "rpm_limit": 100},
    )
    assert resp.status_code == 201
    body = resp.json()
    assert body["key"].startswith("sk-pico-")
    assert body["label"] == "test-key"
    assert "key_prefix" in body

    list_resp = await client.get("/admin/keys", headers=_auth())
    keys = list_resp.json()["keys"]
    assert any(k["label"] == "test-key" for k in keys)


# ---------------------------------------------------------------------------
# 4. Revoke key (DELETE /admin/keys/{prefix})
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_revoke_key(client):
    create_resp = await client.post(
        "/admin/keys",
        headers=_auth(),
        json={"label": "revoke-me"},
    )
    prefix = create_resp.json()["key_prefix"]

    resp = await client.delete(f"/admin/keys/{prefix}", headers=_auth())
    assert resp.status_code == 200
    assert resp.json()["revoked"] == 1

    list_resp = await client.get("/admin/keys", headers=_auth())
    keys = list_resp.json()["keys"]
    revoked = [k for k in keys if k["key_prefix"] == prefix]
    assert len(revoked) == 1
    assert not revoked[0]["is_active"]


@pytest.mark.asyncio
async def test_revoke_nonexistent_key(client):
    resp = await client.delete("/admin/keys/noexist...", headers=_auth())
    assert resp.status_code == 404


# ---------------------------------------------------------------------------
# 5. Set key limits (PUT /admin/keys/{prefix}/limits)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_set_key_limits(client):
    create_resp = await client.post(
        "/admin/keys",
        headers=_auth(),
        json={"label": "limit-test"},
    )
    prefix = create_resp.json()["key_prefix"]

    resp = await client.put(
        f"/admin/keys/{prefix}/limits",
        headers=_auth(),
        json={"rpm": 50, "rpd": 500, "tpm": 100000, "tpd": 1000000},
    )
    assert resp.status_code == 200
    assert resp.json()["updated"] == 1

    list_resp = await client.get("/admin/keys", headers=_auth())
    keys = list_resp.json()["keys"]
    key = [k for k in keys if k["key_prefix"] == prefix][0]
    assert key["rpm_limit"] == 50
    assert key["rpd_limit"] == 500
    assert key["tpm_limit"] == 100000
    assert key["tpd_limit"] == 1000000


# ---------------------------------------------------------------------------
# 6. List teams (GET /admin/teams)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_list_teams(client):
    resp = await client.get("/admin/teams", headers=_auth())
    assert resp.status_code == 200
    body = resp.json()
    assert "teams" in body
    assert "total" in body
    assert isinstance(body["teams"], list)


# ---------------------------------------------------------------------------
# 7. Create team (POST /admin/teams)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_create_team(client):
    resp = await client.post(
        "/admin/teams",
        headers=_auth(),
        json={"name": "Engineering", "description": "Eng team"},
    )
    assert resp.status_code == 201
    body = resp.json()
    assert body["name"] == "Engineering"
    assert body["description"] == "Eng team"
    assert body["is_active"] is True
    assert "id" in body

    list_resp = await client.get("/admin/teams", headers=_auth())
    teams = list_resp.json()["teams"]
    assert any(t["name"] == "Engineering" for t in teams)


@pytest.mark.asyncio
async def test_create_team_missing_name(client):
    resp = await client.post(
        "/admin/teams",
        headers=_auth(),
        json={"description": "no name"},
    )
    assert resp.status_code == 400


# ---------------------------------------------------------------------------
# 8. Deactivate team (DELETE /admin/teams/{id}) — cascade check
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_deactivate_team_cascade(client):
    team_resp = await client.post(
        "/admin/teams",
        headers=_auth(),
        json={"name": "DeactivateMe"},
    )
    team_id = team_resp.json()["id"]

    user_resp = await client.post(
        f"/admin/teams/{team_id}/users",
        headers=_auth(),
        json={"email": "cascade@example.com", "name": "Cascade"},
    )
    assert user_resp.status_code == 201
    user_id = user_resp.json()["id"]

    key_resp = await client.post(
        "/admin/keys",
        headers=_auth(),
        json={"label": "cascade-key", "user_id": user_id},
    )
    assert key_resp.status_code == 201
    key_prefix = key_resp.json()["key_prefix"]

    del_resp = await client.delete(f"/admin/teams/{team_id}", headers=_auth())
    assert del_resp.status_code == 200
    assert del_resp.json()["deactivated"] is True

    keys_resp = await client.get("/admin/keys", headers=_auth())
    key_info = [k for k in keys_resp.json()["keys"] if k["key_prefix"] == key_prefix]
    assert len(key_info) == 1
    assert not key_info[0]["is_active"]

    user_detail_resp = await client.get(f"/admin/users/{user_id}", headers=_auth())
    assert user_detail_resp.status_code == 200
    assert not user_detail_resp.json()["is_active"]


@pytest.mark.asyncio
async def test_deactivate_nonexistent_team(client):
    resp = await client.delete("/admin/teams/99999", headers=_auth())
    assert resp.status_code == 404


# ---------------------------------------------------------------------------
# 9. Get user details (GET /admin/users/{id})
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_get_user(client):
    team_resp = await client.post(
        "/admin/teams",
        headers=_auth(),
        json={"name": "UserTeam"},
    )
    team_id = team_resp.json()["id"]

    user_resp = await client.post(
        f"/admin/teams/{team_id}/users",
        headers=_auth(),
        json={"email": "detail@example.com", "name": "Detail"},
    )
    user_id = user_resp.json()["id"]

    resp = await client.get(f"/admin/users/{user_id}", headers=_auth())
    assert resp.status_code == 200
    body = resp.json()
    assert body["email"] == "detail@example.com"
    assert body["name"] == "Detail"
    assert body["team_id"] == team_id
    assert "month_spend_usd" in body


@pytest.mark.asyncio
async def test_get_nonexistent_user(client):
    resp = await client.get("/admin/users/99999", headers=_auth())
    assert resp.status_code == 404


# ---------------------------------------------------------------------------
# 10. Set user budget (PUT /admin/users/{id}/budget)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_set_user_budget(client):
    team_resp = await client.post(
        "/admin/teams",
        headers=_auth(),
        json={"name": "BudgetTeam"},
    )
    team_id = team_resp.json()["id"]

    user_resp = await client.post(
        f"/admin/teams/{team_id}/users",
        headers=_auth(),
        json={"email": "budget@example.com", "name": "Budget"},
    )
    user_id = user_resp.json()["id"]

    resp = await client.put(
        f"/admin/users/{user_id}/budget",
        headers=_auth(),
        json={"monthly_budget_usd": 150.0},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["updated"] is True
    assert body["monthly_budget_usd"] == 150.0

    detail = await client.get(f"/admin/users/{user_id}", headers=_auth())
    assert detail.json()["monthly_budget_usd"] == 150.0


@pytest.mark.asyncio
async def test_set_user_budget_nonexistent(client):
    resp = await client.put(
        "/admin/users/99999/budget",
        headers=_auth(),
        json={"monthly_budget_usd": 50.0},
    )
    assert resp.status_code == 404


# ---------------------------------------------------------------------------
# 11. Global usage (GET /admin/usage)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_global_usage(client):
    resp = await client.get("/admin/usage", headers=_auth())
    assert resp.status_code == 200
    body = resp.json()
    assert "summary" in body
    assert "details" in body
    assert "total_requests" in body["summary"]
    assert "total_cost_usd" in body["summary"]


@pytest.mark.asyncio
async def test_usage_without_auth(client):
    resp = await client.get("/admin/usage", headers=_no_auth())
    assert resp.status_code == 401


# ---------------------------------------------------------------------------
# 12. Config reload (POST /admin/config/reload)
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_config_reload(client):
    with patch("api.server._is_draining", False), \
         patch("api.server._wait_for_drain", new_callable=AsyncMock, return_value=True), \
         patch("os.execve") as mock_exec:
        resp = await client.post("/admin/config/reload", headers=_auth())
        assert resp.status_code == 200
        mock_exec.assert_called_once()


# ---------------------------------------------------------------------------
# Bonus: auth edge cases
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_wrong_master_key(client):
    resp = await client.get(
        "/admin/keys",
        headers={"Authorization": "Bearer sk-wrong-key"},
    )
    assert resp.status_code == 401


@pytest.mark.asyncio
async def test_malformed_auth_header(client):
    resp = await client.get(
        "/admin/keys",
        headers={"Authorization": "NotBearer sk-test"},
    )
    assert resp.status_code == 401


# ---------------------------------------------------------------------------
# Bonus: key models endpoint
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_set_key_models(client):
    create_resp = await client.post(
        "/admin/keys",
        headers=_auth(),
        json={"label": "model-test"},
    )
    prefix = create_resp.json()["key_prefix"]

    resp = await client.put(
        f"/admin/keys/{prefix}/models",
        headers=_auth(),
        json={"models": ["gpt-4", "claude-3"]},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["models"] == ["gpt-4", "claude-3"]

    list_resp = await client.get("/admin/keys", headers=_auth())
    key = [k for k in list_resp.json()["keys"] if k["key_prefix"] == prefix][0]
    assert key["model_allowlist"] == ["gpt-4", "claude-3"]
