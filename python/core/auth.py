from __future__ import annotations

import hashlib
import hmac as hmac_mod
import ipaddress
import json
import logging
import time
from typing import Any, TYPE_CHECKING

from core.db import get_db
from core.teams import merge_allowlist, merge_limits, resolve_user_limits


_log = logging.getLogger("llm-pico.auth")


def hash_key(raw_key: str) -> str:
    return hashlib.sha256(raw_key.encode()).hexdigest()


def prefix_from_key(raw_key: str) -> str:
    if len(raw_key) > 12:
        return raw_key[:12] + "..."
    return raw_key


def extract_bearer(authorization: str | None) -> str | None:
    if not authorization:
        return None
    if not authorization.startswith("Bearer "):
        return None
    return authorization[7:].strip()


async def verify_api_key(
    raw_key: str,
    master_key: str | None = None,
) -> dict[str, Any] | None:
    if master_key and hmac_mod.compare_digest(raw_key, master_key):
        return {"role": "admin", "key_prefix": "master"}

    key_hash = hash_key(raw_key)
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT * FROM user_keys WHERE key_hash = ? AND is_active = 1",
            (key_hash,),
        )
        row = await cursor.fetchone()

    if row is None:
        return None

    if row["expires_at"] and row["expires_at"] < time.strftime("%Y-%m-%dT%H:%M:%S"):
        return None

    allowlist = None
    if row["model_allowlist"]:
        try:
            allowlist = json.loads(row["model_allowlist"])
        except (json.JSONDecodeError, TypeError):
            pass

    key_limits = {
        "rpm": row["rpm_limit"],
        "rpd": row["rpd_limit"],
        "tpm": row["tpm_limit"],
        "tpd": row["tpd_limit"],
        "ash": row["ash_limit"],
        "asd": row["asd_limit"],
    }

    user_id = row["user_id"]
    user_row = None
    team_row = None

    if user_id is not None:
        user_row, team_row = await resolve_user_limits(user_id)
        if user_row and not user_row.get("is_active"):
            return None
        if team_row and not team_row.get("is_active"):
            return None

    merged_limits = merge_limits(key_limits, user_row, team_row) if user_id else key_limits
    merged_allowlist = merge_allowlist(allowlist, user_row, team_row) if user_id else allowlist

    result: dict[str, Any] = {
        "role": "user",
        "key_hash": row["key_hash"],
        "key_prefix": row["key_prefix"],
        "model_allowlist": merged_allowlist,
        "rpm_limit": merged_limits.get("rpm"),
        "rpd_limit": merged_limits.get("rpd"),
        "tpm_limit": merged_limits.get("tpm"),
        "tpd_limit": merged_limits.get("tpd"),
        "ash_limit": merged_limits.get("ash"),
        "asd_limit": merged_limits.get("asd"),
        "user_id": user_id,
        "ip_allowlist": row["ip_allowlist"],
        "scopes": await get_key_scopes(row["id"]),
    }

    if user_id:
        result["user_row"] = user_row
        result["team_row"] = team_row

    return result


def check_model_access(user: dict[str, Any], model_name: str) -> bool:
    allowlist = user.get("model_allowlist")
    if allowlist is None:
        return True
    return model_name in allowlist


# ---- API Key Scopes ----

async def get_key_scopes(key_id: int) -> list[str]:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT scope FROM api_key_scopes WHERE key_id = ?", (key_id,)
        )
        return [row["scope"] for row in await cursor.fetchall()]


async def set_key_scopes(key_id: int, scopes: list[str]) -> None:
    async with get_db() as db:
        await db.execute("DELETE FROM api_key_scopes WHERE key_id = ?", (key_id,))
        for scope in scopes:
            await db.execute(
                "INSERT INTO api_key_scopes (key_id, scope) VALUES (?, ?)",
                (key_id, scope),
            )
        await db.commit()


async def get_key_id_by_hash(key_hash: str) -> int | None:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT id FROM user_keys WHERE key_hash = ? AND is_active = 1",
            (key_hash,),
        )
        row = await cursor.fetchone()
        return row["id"] if row else None


async def has_scope(key_hash: str, scope: str) -> bool:
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT 1 FROM api_key_scopes s
               JOIN user_keys k ON k.id = s.key_id
               WHERE k.key_hash = ? AND s.scope = ?""",
            (key_hash, scope),
        )
        return await cursor.fetchone() is not None


def require_scope(scope: str):
    """Dependency factory: returns a FastAPI dependency that checks for `scope`."""
    from fastapi import HTTPException

    async def _check(user: dict[str, Any]) -> dict[str, Any]:
        if user.get("role") == "admin":
            return user
        key_hash = user.get("key_hash")
        if key_hash and user.get("scopes"):
            if scope in user["scopes"]:
                return user
        if key_hash and await has_scope(key_hash, scope):
            return user
        raise HTTPException(status_code=403, detail={
            "error": {"message": f"Missing required scope: {scope}",
                       "type": "forbidden", "code": 403}
        })
    return _check


# ---- IP Allowlist ----

def check_ip_allowed(key_hash: str, client_ip: str, ip_allowlist: str | None) -> bool:
    if not ip_allowlist:
        return True
    try:
        allowed = json.loads(ip_allowlist)
    except (json.JSONDecodeError, TypeError):
        return True
    if not allowed:
        return True
    try:
        addr = ipaddress.ip_address(client_ip)
    except ValueError:
        return False
    for cidr in allowed:
        try:
            if addr in ipaddress.ip_network(cidr, strict=False):
                return True
        except ValueError:
            continue
    return False


# ---- HMAC Request Signing ----

def verify_hmac_signature(secret: str, body: bytes, signature: str) -> bool:
    expected = hmac_mod.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac_mod.compare_digest(f"sha256={expected}", signature)




async def seed_users(users: list[Any]) -> None:
    async with get_db() as db:
        for user in users:
            if hasattr(user, "key"):
                raw_key = user.key
                label = user.label
                models = user.models
                rpm = user.rpm
                rpd = user.rpd
                tpm = user.tpm
                tpd = user.tpd
            else:
                raw_key = user["key"]
                label = user.get("label")
                models = user.get("models")
                rpm = user.get("rpm")
                rpd = user.get("rpd")
                tpm = user.get("tpm")
                tpd = user.get("tpd")

            key_hash = hash_key(raw_key)
            key_prefix = prefix_from_key(raw_key)
            allowlist_json = json.dumps(models) if models else None
            now = time.strftime("%Y-%m-%dT%H:%M:%S")

            await db.execute(
                """INSERT OR IGNORE INTO user_keys
                   (key_hash, key_prefix, label, is_active, created_at,
                    model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit)
                   VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?)""",
                (key_hash, key_prefix, label, now, allowlist_json, rpm, rpd, tpm, tpd),
            )
        await db.commit()
    _log.info("seeded %d user keys", len(users))


# ponytail: JSONL audit log — one line per admin action, append-only
import os as _os
_audit_log_path: str | None = None


def init_audit_log(db_path: str) -> None:
    global _audit_log_path
    _audit_log_path = _os.path.join(_os.path.dirname(db_path), "audit.jsonl")


def audit_log(action: str, actor: str = "admin", **details: Any) -> None:
    """Append a structured audit entry. Best-effort, never raises."""
    if not _audit_log_path:
        return
    try:
        import json as _json
        from datetime import datetime, timezone
        entry = {"ts": datetime.now(timezone.utc).isoformat(), "action": action, "actor": actor, **details}
        with open(_audit_log_path, "a") as f:
            f.write(_json.dumps(entry) + "\n")
    except Exception:
        pass
