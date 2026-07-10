from __future__ import annotations

import logging
import time
import uuid
from typing import Any

from .db import get_db

_log = logging.getLogger("llm-pico.usage")


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
) -> str:
    request_id = uuid.uuid4().hex[:16]
    created_at = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())

    async with get_db() as db:
        await db.execute(
            """INSERT INTO usage_log
               (key_hash, key_prefix, model_name, provider, request_id,
                prompt_tokens, completion_tokens, total_tokens,
                latency_ms, status_code, error, created_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                key_hash, key_prefix, model_name, provider, request_id,
                prompt_tokens, completion_tokens, total_tokens,
                latency_ms, status_code, error, created_at,
            ),
        )
        await db.commit()

    return request_id


async def get_usage_stats(
    key_hash: str | None = None,
    from_date: str | None = None,
    to_date: str | None = None,
    limit: int = 100,
) -> dict[str, Any]:
    conditions = []
    params: list[Any] = []

    if key_hash:
        conditions.append("key_hash = ?")
        params.append(key_hash)
    if from_date:
        conditions.append("created_at >= ?")
        params.append(from_date)
    if to_date:
        conditions.append("created_at <= ?")
        params.append(to_date)

    where = " WHERE " + " AND ".join(conditions) if conditions else ""

    async with get_db() as db:
        cursor = await db.execute(
            f"""SELECT
                   COUNT(*) as total_requests,
                   COALESCE(SUM(prompt_tokens), 0) as total_prompt_tokens,
                   COALESCE(SUM(completion_tokens), 0) as total_completion_tokens,
                   COALESCE(SUM(total_tokens), 0) as total_tokens
                FROM usage_log{where}""",
            params,
        )
        summary = dict(await cursor.fetchone())

        cursor = await db.execute(
            f"""SELECT key_prefix, model_name,
                       COUNT(*) as requests,
                       SUM(prompt_tokens) as prompt_tokens,
                       SUM(completion_tokens) as completion_tokens,
                       SUM(total_tokens) as total_tokens
                FROM usage_log{where}
                GROUP BY key_prefix, model_name
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
                       SUM(total_tokens) as total_tokens
                FROM usage_log{where}
                GROUP BY model_name, provider
                ORDER BY total_tokens DESC
                LIMIT ?""",
            params,
        )
        return [dict(r) for r in await cursor.fetchall()]
