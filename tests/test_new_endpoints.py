"""Tests for new init/auth/probe endpoints and round-robin routing."""
from __future__ import annotations

import hashlib
import json
import secrets
import time

import pytest
import pytest_asyncio
from httpx import AsyncClient, ASGITransport

from api.server import create_app
from core.config import Config, GeneralSettings, ModelParams, ModelEntry, RouterSettings, save_settings
from core.db import init_db

MASTER_KEY = "sk-pico-master-test"
MASTER_HASH = hashlib.sha256(MASTER_KEY.encode()).hexdigest()


@pytest_asyncio.fixture
async def app(tmp_path):
    db_path = str(tmp_path / "test.db")
    await init_db(db_path)
    # Save master key hash to DB (simulates completed init)
    await save_settings({"master_key": MASTER_HASH})
    config = Config(
        general_settings=GeneralSettings(master_key=MASTER_HASH),
        router_settings=RouterSettings(),
    )
    application = create_app({"config": config, "router": None})
    application.state.config = config
    return application


@pytest.fixture
def client(app):
    transport = ASGITransport(app=app)
    return AsyncClient(transport=transport, base_url="http://testserver")


# ---- Init status ----

@pytest.mark.asyncio
async def test_init_status_configured(client):
    resp = await client.get("/admin/init/status")
    assert resp.status_code == 200
    assert resp.json() == {"initialized": True}


@pytest.mark.asyncio
async def test_init_status_not_configured(client, tmp_path):
    db_path = str(tmp_path / "fresh.db")
    await init_db(db_path)
    config = Config(general_settings=GeneralSettings(master_key=""))
    app = create_app({"config": config, "router": None})
    app.state.config = config
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://testserver") as c:
        resp = await c.get("/admin/init/status")
        assert resp.json() == {"initialized": False}


# ---- Auth init master key ----

@pytest.mark.asyncio
async def test_auth_init_master_key(client, tmp_path):
    db_path = str(tmp_path / "fresh2.db")
    await init_db(db_path)
    config = Config(general_settings=GeneralSettings(master_key=""))
    app = create_app({"config": config, "router": None})
    app.state.config = config
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://testserver") as c:
        key_hash = hashlib.sha256(b"new-key").hexdigest()
        resp = await c.post("/admin/auth/init-master-key", json={"keyHash": key_hash})
        assert resp.status_code == 201
        assert resp.json()["ok"] is True


@pytest.mark.asyncio
async def test_auth_init_already_configured(client):
    key_hash = hashlib.sha256(b"key").hexdigest()
    resp = await client.post("/admin/auth/init-master-key", json={"keyHash": key_hash})
    assert resp.status_code == 400


@pytest.mark.asyncio
async def test_auth_init_missing_hash(client):
    resp = await client.post("/admin/auth/init-master-key", json={})
    assert resp.status_code == 400


# ---- Auth verify master key ----

@pytest.mark.asyncio
async def test_auth_verify_valid(client):
    # Send the hash of MASTER_KEY (which is what's stored in DB)
    resp = await client.post("/admin/auth/verify-master-key", json={"keyHash": MASTER_HASH})
    assert resp.status_code == 200
    assert resp.json()["ok"] is True


@pytest.mark.asyncio
async def test_auth_verify_invalid(client):
    wrong = hashlib.sha256(b"wrong").hexdigest()
    resp = await client.post("/admin/auth/verify-master-key", json={"keyHash": wrong})
    assert resp.status_code == 401


@pytest.mark.asyncio
async def test_auth_verify_not_initialized(client, tmp_path):
    db_path = str(tmp_path / "fresh3.db")
    await init_db(db_path)
    config = Config(general_settings=GeneralSettings(master_key=""))
    app = create_app({"config": config, "router": None})
    app.state.config = config
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://testserver") as c:
        resp = await c.post("/admin/auth/verify-master-key", json={"keyHash": "abc"})
        assert resp.status_code == 401


# ---- Require master key accepts hash and raw key ----

@pytest.mark.asyncio
async def test_master_key_accepts_hash(client):
    resp = await client.get("/admin/config", headers={"Authorization": f"Bearer {MASTER_HASH}"})
    assert resp.status_code == 200


@pytest.mark.asyncio
async def test_master_key_rejects_wrong(client):
    resp = await client.get("/admin/config", headers={"Authorization": "Bearer wrong"})
    assert resp.status_code == 401


# ---- Round-robin routing ----

def test_round_robin_distribution():
    from core.router import Router
    config = Config(model_list=[
        ModelEntry(model_name="m", model_params=ModelParams(model="openai/m", api_key=["k1", "k2", "k3"])),
    ])
    router = Router(config)
    counts = {"k1": 0, "k2": 0, "k3": 0}
    for _ in range(30):
        _, key, _ = router.resolve("m")
        counts[key.api_key] += 1
    assert counts == {"k1": 10, "k2": 10, "k3": 10}


def test_round_robin_skips_cooldown():
    from core.router import Router
    config = Config(model_list=[
        ModelEntry(model_name="m", model_params=ModelParams(model="openai/m", api_key=["k1", "k2", "k3"])),
    ])
    router = Router(config)
    router._model_map["m"][0].keys[1].cooldown_until = time.monotonic() + 60
    counts = {"k1": 0, "k2": 0, "k3": 0}
    for _ in range(20):
        _, key, _ = router.resolve("m")
        counts[key.api_key] += 1
    assert counts["k2"] == 0
    assert counts["k1"] == 10
    assert counts["k3"] == 10
