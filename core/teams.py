from __future__ import annotations

import json
import time
from typing import Any

from core.db import get_db


async def create_team(name: str, description: str | None = None) -> dict[str, Any]:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        cursor = await db.execute(
            """INSERT INTO teams (name, description, is_active, created_at)
               VALUES (?, ?, 1, ?)""",
            (name, description, now),
        )
        await db.commit()
        team_id = cursor.lastrowid
    return {"id": team_id, "name": name, "description": description, "is_active": True, "created_at": now}


async def get_teams() -> list[dict[str, Any]]:
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT id, name, description, is_active, created_at,
                      rpm_limit, rpd_limit, tpm_limit, tpd_limit,
                      ash_limit, asd_limit,
                      monthly_budget_usd, model_allowlist
               FROM teams ORDER BY created_at DESC"""
        )
        rows = await cursor.fetchall()
    return [dict(r) for r in rows]


async def get_team(team_id: int) -> dict[str, Any] | None:
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT id, name, description, is_active, created_at,
                      rpm_limit, rpd_limit, tpm_limit, tpd_limit,
                      ash_limit, asd_limit,
                      monthly_budget_usd, model_allowlist
               FROM teams WHERE id = ?""",
            (team_id,),
        )
        row = await cursor.fetchone()
    return dict(row) if row else None


async def update_team_limits(team_id: int, limits: dict[str, Any]) -> bool:
    async with get_db() as db:
        cursor = await db.execute(
            """UPDATE teams SET
               rpm_limit = COALESCE(?, rpm_limit),
               rpd_limit = COALESCE(?, rpd_limit),
               tpm_limit = COALESCE(?, tpm_limit),
               tpd_limit = COALESCE(?, tpd_limit),
               ash_limit = COALESCE(?, ash_limit),
               asd_limit = COALESCE(?, asd_limit)
               WHERE id = ?""",
            (limits.get("rpm"), limits.get("rpd"), limits.get("tpm"), limits.get("tpd"),
             limits.get("ash"), limits.get("asd"), team_id),
        )
        await db.commit()
        return cursor.rowcount > 0


async def update_team_allowlist(team_id: int, models: list[str] | None) -> bool:
    allowlist_json = json.dumps(models) if models is not None else None
    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE teams SET model_allowlist = ? WHERE id = ?",
            (allowlist_json, team_id),
        )
        await db.commit()
        return cursor.rowcount > 0


async def create_user(team_id: int, email: str, name: str) -> dict[str, Any]:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        cursor = await db.execute(
            """INSERT INTO users (team_id, email, name, is_active, created_at)
               VALUES (?, ?, ?, 1, ?)""",
            (team_id, email, name, now),
        )
        await db.commit()
        user_id = cursor.lastrowid
    return {"id": user_id, "team_id": team_id, "email": email, "name": name, "is_active": True, "created_at": now}


async def get_users(team_id: int | None = None) -> list[dict[str, Any]]:
    async with get_db() as db:
        if team_id is not None:
            cursor = await db.execute(
                """SELECT id, team_id, email, name, is_active, created_at,
                          rpm_limit, rpd_limit, tpm_limit, tpd_limit,
                          ash_limit, asd_limit,
                          monthly_budget_usd, model_allowlist
                   FROM users WHERE team_id = ? ORDER BY created_at DESC""",
                (team_id,),
            )
        else:
            cursor = await db.execute(
                """SELECT id, team_id, email, name, is_active, created_at,
                          rpm_limit, rpd_limit, tpm_limit, tpd_limit,
                          ash_limit, asd_limit,
                          monthly_budget_usd, model_allowlist
                   FROM users ORDER BY created_at DESC"""
            )
        rows = await cursor.fetchall()
    return [dict(r) for r in rows]


async def get_user(user_id: int) -> dict[str, Any] | None:
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT id, team_id, email, name, is_active, created_at,
                      rpm_limit, rpd_limit, tpm_limit, tpd_limit,
                      ash_limit, asd_limit,
                      monthly_budget_usd, model_allowlist
               FROM users WHERE id = ?""",
            (user_id,),
        )
        row = await cursor.fetchone()
    return dict(row) if row else None


async def update_user_limits(user_id: int, limits: dict[str, Any]) -> bool:
    async with get_db() as db:
        cursor = await db.execute(
            """UPDATE users SET
               rpm_limit = COALESCE(?, rpm_limit),
               rpd_limit = COALESCE(?, rpd_limit),
               tpm_limit = COALESCE(?, tpm_limit),
               tpd_limit = COALESCE(?, tpd_limit),
               ash_limit = COALESCE(?, ash_limit),
               asd_limit = COALESCE(?, asd_limit)
               WHERE id = ?""",
            (limits.get("rpm"), limits.get("rpd"), limits.get("tpm"), limits.get("tpd"),
             limits.get("ash"), limits.get("asd"), user_id),
        )
        await db.commit()
        return cursor.rowcount > 0


async def update_user_budget(user_id: int, monthly_budget_usd: float | None) -> bool:
    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE users SET monthly_budget_usd = ? WHERE id = ?",
            (monthly_budget_usd, user_id),
        )
        await db.commit()
        return cursor.rowcount > 0


async def update_user_allowlist(user_id: int, models: list[str] | None) -> bool:
    allowlist_json = json.dumps(models) if models is not None else None
    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE users SET model_allowlist = ? WHERE id = ?",
            (allowlist_json, user_id),
        )
        await db.commit()
        return cursor.rowcount > 0


async def assign_key_to_user(key_hash: str, user_id: int | None) -> bool:
    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE user_keys SET user_id = ? WHERE key_hash = ?",
            (user_id, key_hash),
        )
        await db.commit()
        return cursor.rowcount > 0


async def resolve_user_limits(user_id: int) -> tuple[dict[str, Any] | None, dict[str, Any] | None]:
    """Resolve user row and parent team row. Returns (user, team) or (None, None)."""
    user_row = await get_user(user_id)
    if user_row is None:
        return None, None
    team_row = await get_team(user_row["team_id"]) if user_row["team_id"] else None
    return user_row, team_row


def merge_limits(
    key_limits: dict[str, Any],
    user_row: dict[str, Any] | None,
    team_row: dict[str, Any] | None,
) -> dict[str, Any]:
    """Most restrictive wins: min of (key, user, team) for each window type."""
    result: dict[str, Any] = {"_level": "user"}
    for wt in ("rpm", "rpd", "tpm", "tpd", "ash", "asd"):
        vals = [key_limits.get(wt)]
        if user_row:
            vals.append(user_row.get(f"{wt}_limit"))
        if team_row:
            vals.append(team_row.get(f"{wt}_limit"))
        non_null = [v for v in vals if v is not None]
        result[wt] = min(non_null) if non_null else None
    return result


def merge_allowlist(
    key_allowlist: list[str] | None,
    user_row: dict[str, Any] | None,
    team_row: dict[str, Any] | None,
) -> list[str] | None:
    """Intersection of allowlists. None = unrestricted."""
    lists = [key_allowlist]
    if user_row and user_row.get("model_allowlist"):
        try:
            lists.append(json.loads(user_row["model_allowlist"]))
        except (json.JSONDecodeError, TypeError):
            pass
    if team_row and team_row.get("model_allowlist"):
        try:
            lists.append(json.loads(team_row["model_allowlist"]))
        except (json.JSONDecodeError, TypeError):
            pass

    non_null = [l for l in lists if l is not None]
    if not non_null:
        return None
    result = set(non_null[0])
    for l in non_null[1:]:
        result &= set(l)
    return sorted(result) if result else []


async def check_user_budget(user_id: int, estimated_cost: float | None) -> str | None:
    """Check if user has monthly budget remaining. Returns None if OK, or error message."""
    user_row = await get_user(user_id)
    if user_row is None:
        return None
    budget = user_row.get("monthly_budget_usd")
    if budget is None:
        return None
    if estimated_cost is None:
        return None

    now = time.gmtime()
    month_start = time.strftime("%Y-%m-01T00:00:00", now)

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT COALESCE(SUM(cost_usd), 0) as total_spend
               FROM usage_log
               WHERE key_hash IN (
                   SELECT key_hash FROM user_keys WHERE user_id = ?
               ) AND created_at >= ? AND cost_usd IS NOT NULL""",
            (user_id, month_start),
        )
        row = await cursor.fetchone()
        total_spend = row["total_spend"] if row else 0.0

    if total_spend + estimated_cost > budget:
        return f"Monthly budget exceeded: ${total_spend:.4f} + ${estimated_cost:.4f} > ${budget:.2f}"
    return None


async def get_user_month_spend(user_id: int) -> float:
    now = time.gmtime()
    month_start = time.strftime("%Y-%m-01T00:00:00", now)
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT COALESCE(SUM(cost_usd), 0) as total_spend
               FROM usage_log
               WHERE key_hash IN (
                   SELECT key_hash FROM user_keys WHERE user_id = ?
               ) AND created_at >= ? AND cost_usd IS NOT NULL""",
            (user_id, month_start),
        )
        row = await cursor.fetchone()
        return row["total_spend"] if row else 0.0


async def get_team_month_spend(team_id: int) -> float:
    now = time.gmtime()
    month_start = time.strftime("%Y-%m-01T00:00:00", now)
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT COALESCE(SUM(cost_usd), 0) as total_spend
               FROM usage_log
               WHERE key_hash IN (
                   SELECT uk.key_hash FROM user_keys uk
                   JOIN users u ON uk.user_id = u.id
                   WHERE u.team_id = ?
               ) AND created_at >= ? AND cost_usd IS NOT NULL""",
            (team_id, month_start),
        )
        row = await cursor.fetchone()
        return row["total_spend"] if row else 0.0


async def deactivate_team(team_id: int) -> bool:
    async with get_db() as db:
        cursor = await db.execute("UPDATE teams SET is_active = 0 WHERE id = ?", (team_id,))
        await db.execute("UPDATE users SET is_active = 0 WHERE team_id = ? AND is_active = 1", (team_id,))
        await db.execute(
            """UPDATE user_keys SET is_active = 0
               WHERE user_id IN (SELECT id FROM users WHERE team_id = ?) AND is_active = 1""",
            (team_id,),
        )
        await db.commit()
        return cursor.rowcount > 0


async def get_model_chain(team_id: int) -> list[str] | None:
    """Get the model chain for a team. Returns None if no chain set."""
    async with get_db() as db:
        cursor = await db.execute("SELECT model_chain FROM teams WHERE id = ?", (team_id,))
        row = await cursor.fetchone()
        if row and row[0]:
            return json.loads(row[0])
        return None


async def set_model_chain(team_id: int, model_chain: list[str]) -> None:
    """Set the model chain for a team (replaces entire array)."""
    async with get_db() as db:
        await db.execute(
            "UPDATE teams SET model_chain = ? WHERE id = ?",
            (json.dumps(model_chain), team_id),
        )
        await db.commit()


async def get_chain_rewrites_response(team_id: int) -> str | None:
    async with get_db() as db:
        cursor = await db.execute("SELECT chain_rewrites_response FROM teams WHERE id = ?", (team_id,))
        row = await cursor.fetchone()
        return row[0] if row else None


async def set_chain_rewrites_response(team_id: int, text: str | None) -> None:
    async with get_db() as db:
        await db.execute(
            "UPDATE teams SET chain_rewrites_response = ? WHERE id = ?",
            (text, team_id),
        )
        await db.commit()
