from __future__ import annotations

import logging
import time
import uuid
from typing import Any

from core.db import get_db

_log = logging.getLogger("llm-pico.usage")


def compute_cost(
    prompt_tokens: int,
    completion_tokens: int,
    cost_in: float | None,
    cost_out: float | None,
) -> float | None:
    if cost_in is None and cost_out is None:
        return None
    total = prompt_tokens + completion_tokens
    if cost_in is None:
        return completion_tokens / 1_000_000 * cost_out
    if cost_out is None:
        return prompt_tokens / 1_000_000 * cost_in
    return (prompt_tokens / 1_000_000 * cost_in) + (completion_tokens / 1_000_000 * cost_out)


async def log_usage(
    key_hash: str,
    key_prefix: str,
    model_name: str,
    provider: str,
    prompt_tokens: int,
    completion_tokens: int,
    total_tokens: int,
    latency_ms: int,
    status_code: int,
    error: str | None = None,
    cost_usd: float | None = None,
) -> str:
    request_id = uuid.uuid4().hex[:16]
    created_at = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())

    async with get_db() as db:
        await db.execute(
            """INSERT INTO usage_log
               (key_hash, key_prefix, model_name, provider, request_id,
                prompt_tokens, completion_tokens, total_tokens,
                latency_ms, status_code, error, cost_usd, created_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                key_hash, key_prefix, model_name, provider, request_id,
                prompt_tokens, completion_tokens, total_tokens,
                latency_ms, status_code, error, cost_usd, created_at,
            ),
        )
        await db.commit()

    return request_id


async def get_usage_stats(
    key_hash: str | None = None,
    from_date: str | None = None,
    to_date: str | None = None,
    limit: int = 100,
    user_id: int | None = None,
    team_id: int | None = None,
) -> dict[str, Any]:
    conditions = []
    params: list[Any] = []

    if key_hash:
        conditions.append("ul.key_hash = ?")
        params.append(key_hash)
    if from_date:
        conditions.append("ul.created_at >= ?")
        params.append(from_date)
    if to_date:
        conditions.append("ul.created_at <= ?")
        params.append(to_date)
    if user_id is not None:
        conditions.append("uk.user_id = ?")
        params.append(user_id)
    if team_id is not None:
        conditions.append("u.team_id = ?")
        params.append(team_id)

    joins = ""
    if user_id is not None or team_id is not None:
        joins = " JOIN user_keys uk ON ul.key_hash = uk.key_hash"
        if team_id is not None:
            joins += " JOIN users u ON uk.user_id = u.id"

    where = " WHERE " + " AND ".join(conditions) if conditions else ""

    async with get_db() as db:
        cursor = await db.execute(
            f"""SELECT
                   COUNT(*) as total_requests,
                   COALESCE(SUM(ul.prompt_tokens), 0) as total_prompt_tokens,
                   COALESCE(SUM(ul.completion_tokens), 0) as total_completion_tokens,
                   COALESCE(SUM(ul.total_tokens), 0) as total_tokens,
                   COALESCE(SUM(ul.cost_usd), 0) as total_cost_usd
                FROM usage_log ul{joins}{where}""",
            params,
        )
        summary = dict(await cursor.fetchone())

        cursor = await db.execute(
            f"""SELECT ul.key_prefix, ul.model_name,
                       COUNT(*) as requests,
                       SUM(ul.prompt_tokens) as prompt_tokens,
                       SUM(ul.completion_tokens) as completion_tokens,
                       SUM(ul.total_tokens) as total_tokens,
                       SUM(ul.cost_usd) as cost_usd
                FROM usage_log ul{joins}{where}
                GROUP BY ul.key_prefix, ul.model_name
                ORDER BY total_tokens DESC
                LIMIT ?""",
            params + [limit],
        )
        rows = await cursor.fetchall()
        per_key_model = [dict(r) for r in rows]

    return {
        "summary": summary,
        "details": per_key_model,
    }


async def get_top_models(
    from_date: str | None = None,
    to_date: str | None = None,
    limit: int = 10,
) -> list[dict[str, Any]]:
    conditions = []
    params: list[Any] = [limit]

    if from_date:
        conditions.append("created_at >= ?")
        params.insert(0, from_date)
    if to_date:
        conditions.append("created_at <= ?")
        params.insert(0 if not from_date else 1, to_date)

    where = " WHERE " + " AND ".join(conditions) if conditions else ""

    async with get_db() as db:
        cursor = await db.execute(
            f"""SELECT model_name, provider,
                       COUNT(*) as requests,
                       SUM(prompt_tokens) as prompt_tokens,
                       SUM(completion_tokens) as completion_tokens,
                       SUM(total_tokens) as total_tokens,
                       COALESCE(SUM(cost_usd), 0) as total_cost_usd
                FROM usage_log{where}
                GROUP BY model_name, provider
                ORDER BY total_tokens DESC
                LIMIT ?""",
            params,
        )
        return [dict(r) for r in await cursor.fetchall()]


async def get_cost_stats(
    group_by: str = "user",
    from_date: str | None = None,
    to_date: str | None = None,
) -> list[dict[str, Any]]:
    conditions = []
    params: list[Any] = []

    if from_date:
        conditions.append("ul.created_at >= ?")
        params.append(from_date)
    if to_date:
        conditions.append("ul.created_at <= ?")
        params.append(to_date)

    where = " WHERE " + " AND ".join(conditions) if conditions else ""

    if group_by == "user":
        select = """SELECT ul.key_prefix,
                           COUNT(*) as requests,
                           COALESCE(SUM(ul.cost_usd), 0) as total_cost_usd,
                           COALESCE(SUM(ul.total_tokens), 0) as total_tokens"""
        group = " GROUP BY ul.key_prefix"
        order = " ORDER BY total_cost_usd DESC"
    elif group_by == "model":
        select = """SELECT ul.model_name,
                           COUNT(*) as requests,
                           COALESCE(SUM(ul.cost_usd), 0) as total_cost_usd,
                           COALESCE(SUM(ul.total_tokens), 0) as total_tokens"""
        group = " GROUP BY ul.model_name"
        order = " ORDER BY total_cost_usd DESC"
    elif group_by == "day":
        select = """SELECT DATE(ul.created_at) as day,
                           COUNT(*) as requests,
                           COALESCE(SUM(ul.cost_usd), 0) as total_cost_usd,
                           COALESCE(SUM(ul.total_tokens), 0) as total_tokens"""
        group = " GROUP BY day"
        order = " ORDER BY day DESC"
    else:
        return []

    async with get_db() as db:
        cursor = await db.execute(
            f"{select} FROM usage_log ul{where}{group}{order}",
            params,
        )
        return [dict(r) for r in await cursor.fetchall()]
