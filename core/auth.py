from __future__ import annotations

import hashlib
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
    if master_key and raw_key == master_key:
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
        "user_id": user_id,
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
