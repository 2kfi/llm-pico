from __future__ import annotations

import hashlib
import hmac
import json
import os
import time

import pytest
import pytest_asyncio

from core.auth import (
    check_ip_allowed,
    get_key_scopes,
    has_scope,
    hash_key,
    set_key_scopes,
    verify_hmac_signature,
    verify_api_key,
)
from core.db import close_db, get_db, init_db

pytestmark = [pytest.mark.asyncio, pytest.mark.usefixtures("db")]


@pytest_asyncio.fixture
async def db(tmp_path):
    db_path = os.path.join(tmp_path, "test.db")
    await init_db(db_path)
    yield
    await close_db()


async def _seed_key(key_hash: str, user_id=None, ip_allowlist=None):
    now = time.strftime("%Y-%m-%dT%H:%M:%S")
    async with get_db() as db:
        await db.execute(
            """INSERT INTO user_keys
               (key_hash, key_prefix, label, is_active, created_at, user_id, ip_allowlist)
               VALUES (?, ?, ?, 1, ?, ?, ?)""",
            (key_hash, key_hash[:12], "test", now, user_id, ip_allowlist),
        )
        await db.commit()


async def _get_key_id(key_hash: str) -> int:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT id FROM user_keys WHERE key_hash = ?", (key_hash,)
        )
        row = await cursor.fetchone()
        return row["id"]


# ---- API Key Scopes tests ----

@pytest.mark.asyncio
async def test_set_and_get_scopes():
    await _seed_key("hash-scope-test")
    key_id = await _get_key_id("hash-scope-test")

    await set_key_scopes(key_id, ["read", "write"])
    scopes = await get_key_scopes(key_id)
    assert sorted(scopes) == ["read", "write"]


@pytest.mark.asyncio
async def test_set_scopes_replaces():
    await _seed_key("hash-scope-replace")
    key_id = await _get_key_id("hash-scope-replace")

    await set_key_scopes(key_id, ["a", "b"])
    await set_key_scopes(key_id, ["c"])
    scopes = await get_key_scopes(key_id)
    assert scopes == ["c"]


@pytest.mark.asyncio
async def test_has_scope():
    await _seed_key("hash-scope-has")
    key_id = await _get_key_id("hash-scope-has")

    await set_key_scopes(key_id, ["deploy"])
    assert await has_scope("hash-scope-has", "deploy") is True
    assert await has_scope("hash-scope-has", "admin") is False


@pytest.mark.asyncio
async def test_scopes_loaded_into_verify():
    raw_key = "sk-pico-test-scopes"
    key_hash = hash_key(raw_key)
    await _seed_key(key_hash)

    key_id = await _get_key_id(key_hash)
    await set_key_scopes(key_id, ["chat", "embeddings"])

    user = await verify_api_key(raw_key)
    assert user is not None
    assert "chat" in user["scopes"]
    assert "embeddings" in user["scopes"]


@pytest.mark.asyncio
async def test_require_scope_admin_bypasses():
    from core.auth import require_scope
    check = require_scope("anything")
    result = await check({"role": "admin"})
    assert result["role"] == "admin"


# ---- IP Allowlist tests ----

@pytest.mark.asyncio
async def test_ip_allowlist_allows_matching_cidr():
    assert check_ip_allowed("h", "10.0.0.5", json.dumps(["10.0.0.0/8"])) is True


@pytest.mark.asyncio
async def test_ip_allowlist_blocks_non_matching():
    assert check_ip_allowed("h", "192.168.1.1", json.dumps(["10.0.0.0/8"])) is False


@pytest.mark.asyncio
async def test_ip_allowlist_none_is_open():
    assert check_ip_allowed("h", "1.2.3.4", None) is True


@pytest.mark.asyncio
async def test_ip_allowlist_empty_is_open():
    assert check_ip_allowed("h", "1.2.3.4", "[]") is True


@pytest.mark.asyncio
async def test_ip_allowlist_multiple_cidrs():
    allowlist = json.dumps(["10.0.0.0/8", "192.168.0.0/16"])
    assert check_ip_allowed("h", "192.168.5.5", allowlist) is True
    assert check_ip_allowed("h", "172.16.0.1", allowlist) is False


@pytest.mark.asyncio
async def test_ip_allowlist_invalid_ip():
    assert check_ip_allowed("h", "not-an-ip", json.dumps(["10.0.0.0/8"])) is False


@pytest.mark.asyncio
async def test_ip_loaded_into_verify():
    raw_key = "sk-pico-test-ip"
    key_hash = hash_key(raw_key)
    ip_list = json.dumps(["10.0.0.0/8"])
    await _seed_key(key_hash, ip_allowlist=ip_list)

    user = await verify_api_key(raw_key)
    assert user is not None
    assert user["ip_allowlist"] == ip_list


# ---- HMAC Request Signing tests ----

def test_hmac_signature_valid():
    secret = "test-secret"
    body = b'{"model": "gpt-4"}'
    sig = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    assert verify_hmac_signature(secret, body, sig) is True


def test_hmac_signature_invalid():
    secret = "test-secret"
    body = b'{"model": "gpt-4"}'
    assert verify_hmac_signature(secret, body, "sha256=deadbeef") is False


def test_hmac_signature_wrong_secret():
    body = b'{"model": "gpt-4"}'
    sig = "sha256=" + hmac.new(b"wrong", body, hashlib.sha256).hexdigest()
    assert verify_hmac_signature("correct", body, sig) is False


def test_hmac_signature_empty_body():
    secret = "s"
    body = b""
    sig = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    assert verify_hmac_signature(secret, body, sig) is True


# ---- Audit Log structured_details / client_ip ----

@pytest.mark.asyncio
async def test_admin_log_structured_and_ip():
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        await db.execute(
            "INSERT INTO admin_log (action, actor_hash, details, structured_details, client_ip, created_at) VALUES (?, ?, ?, ?, ?, ?)",
            ("test_action", "abc123", '{"k":"v"}', '{"k":"v"}', "10.0.0.1", now),
        )
        await db.commit()

        cursor = await db.execute(
            "SELECT structured_details, client_ip FROM admin_log WHERE action = 'test_action'"
        )
        row = await cursor.fetchone()
        assert row["structured_details"] == '{"k":"v"}'
        assert row["client_ip"] == "10.0.0.1"


@pytest.mark.asyncio
async def test_admin_log_nullable_columns():
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        await db.execute(
            "INSERT INTO admin_log (action, actor_hash, details, created_at) VALUES (?, ?, ?, ?)",
            ("nullable_test", "abc", None, now),
        )
        await db.commit()

        cursor = await db.execute(
            "SELECT structured_details, client_ip FROM admin_log WHERE action = 'nullable_test'"
        )
        row = await cursor.fetchone()
        assert row["structured_details"] is None
        assert row["client_ip"] is None
