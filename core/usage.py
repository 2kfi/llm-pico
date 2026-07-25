from __future__ import annotations

import calendar
import logging
import time
import uuid
from datetime import datetime, timezone
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


def classify_error(status_code: int, body: dict | str = "") -> str:
    if status_code == 429:
        return "rate_limit"
    if status_code == 401:
        return "invalid_key"
    if status_code == 402:
        return "quota_exceeded"
    if status_code == 503:
        return "model_overloaded"
    if status_code == 504:
        return "timeout"
    if "content_filter" in str(body):
        return "content_filter"
    return "unknown"


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
    error_type: str | None = None,
) -> str:
    request_id = uuid.uuid4().hex[:16]
    created_at = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())

    async with get_db() as db:
        await db.execute(
            """INSERT INTO usage_log
               (key_hash, key_prefix, model_name, provider, request_id,
                prompt_tokens, completion_tokens, total_tokens,
                latency_ms, status_code, error, cost_usd, error_type, created_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                key_hash, key_prefix, model_name, provider, request_id,
                prompt_tokens, completion_tokens, total_tokens,
                latency_ms, status_code, error, cost_usd, error_type, created_at,
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


async def get_error_stats(
    provider: str | None = None,
    model: str | None = None,
    from_date: str | None = None,
    to_date: str | None = None,
) -> list[dict[str, Any]]:
    conditions = ["error_type IS NOT NULL", "error_type != 'unknown'"]
    params: list[Any] = []

    if provider:
        conditions.append("provider = ?")
        params.append(provider)
    if model:
        conditions.append("model_name = ?")
        params.append(model)
    if from_date:
        conditions.append("created_at >= ?")
        params.append(from_date)
    if to_date:
        conditions.append("created_at <= ?")
        params.append(to_date)

    where = " WHERE " + " AND ".join(conditions)

    async with get_db() as db:
        cursor = await db.execute(
            f"""SELECT error_type, COUNT(*) as count,
                       model_name, provider
                FROM usage_log{where}
                GROUP BY error_type, model_name, provider
                ORDER BY count DESC""",
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


def _month_start() -> str:
    return time.strftime("%Y-%m-01T00:00:00", time.gmtime())


async def get_key_month_spend(key_hash: str) -> float:
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT COALESCE(SUM(cost_usd), 0) as total
               FROM usage_log WHERE key_hash = ? AND created_at >= ? AND cost_usd IS NOT NULL""",
            (key_hash, _month_start()),
        )
        row = await cursor.fetchone()
        return row["total"] if row else 0.0


async def check_key_budget(key_hash: str, estimated_cost: float | None) -> str | None:
    if estimated_cost is None:
        return None
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT monthly_budget_usd, hard_limit FROM user_keys WHERE key_hash = ?",
            (key_hash,),
        )
        row = await cursor.fetchone()
    if row is None or row["monthly_budget_usd"] is None:
        return None
    budget = row["monthly_budget_usd"]
    spend = await get_key_month_spend(key_hash)
    if spend + estimated_cost > budget:
        return f"Key budget exceeded: ${spend:.4f} + ${estimated_cost:.4f} > ${budget:.2f}"
    return None


async def get_cost_projection() -> dict[str, float]:
    now = datetime.now(timezone.utc)
    today = now.day
    days_in_month = calendar.monthrange(now.year, now.month)[1]
    month_start = _month_start()

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT COALESCE(SUM(cost_usd), 0) as total
               FROM usage_log WHERE created_at >= ? AND cost_usd IS NOT NULL""",
            (month_start,),
        )
        row = await cursor.fetchone()
        current_spend = row["total"] if row else 0.0

    daily_rate = current_spend / max(1, today)
    return {
        "current_spend": current_spend,
        "days_elapsed": today,
        "daily_rate": daily_rate,
        "projected_total": daily_rate * days_in_month,
        "days_remaining": days_in_month - today,
    }


async def get_provider_cost_comparison() -> list[dict[str, Any]]:
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT model_name, provider,
                      AVG(cost_usd * 1000000.0 / NULLIF(total_tokens, 0)) as cost_per_m,
                      AVG(latency_ms) as avg_latency_ms,
                      COUNT(*) as request_count
               FROM usage_log
               GROUP BY model_name, provider
               ORDER BY model_name, cost_per_m"""
        )
        return [dict(r) for r in await cursor.fetchall()]


async def reserve_tokens(
    key_hash: str,
    model_name: str,
    user_id: int | None,
    team_id: int | None,
    reservation: int,
) -> str | None:
    # Key-level
    spend = await get_key_month_spend(key_hash)
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT monthly_budget_usd FROM user_keys WHERE key_hash = ?",
            (key_hash,),
        )
        row = await cursor.fetchone()
    if row and row["monthly_budget_usd"] is not None:
        if spend + reservation / 1_000_000.0 > row["monthly_budget_usd"]:
            return "Key budget exceeded"

    # User-level (reuse existing check_user_budget from teams)
    if user_id:
        from core.teams import check_user_budget
        err = await check_user_budget(user_id, reservation / 1_000_000.0)
        if err:
            return err

    # Team-level
    if team_id:
        from core.teams import get_team, get_team_month_spend
        team = await get_team(team_id)
        if team and team.get("monthly_budget_usd") is not None:
            ts = await get_team_month_spend(team_id)
            if ts + reservation / 1_000_000.0 > team["monthly_budget_usd"]:
                return "Team budget exceeded"

    return None


async def reconcile_tokens(
    key_hash: str,
    model_name: str,
    user_id: int | None,
    team_id: int | None,
    actual: int,
    reserved: int,
) -> None:
    delta = actual - reserved
    if delta == 0:
        return
    # No persistent counters to update for budgets — usage_log entries are the source of truth.
    # Token reservations are checked against usage_log at request time.
    pass


# ponytail: budget alert webhook — fire-and-forget HTTP POST on threshold breach
_budget_alert_config: dict[str, Any] = {}


def configure_budget_alerts(webhook_url: str, thresholds: list[float] | None = None) -> None:
    global _budget_alert_config
    _budget_alert_config = {
        "webhook_url": webhook_url,
        "thresholds": sorted(thresholds or [0.8, 0.9, 1.0]),
    }


async def check_budget_alert(entity_type: str, entity_id: str, spend: float, budget: float) -> None:
    """Check spend/budget ratio and fire webhook if threshold crossed."""
    if not _budget_alert_config.get("webhook_url") or budget <= 0:
        return
    ratio = spend / budget
    for t in _budget_alert_config["thresholds"]:
        if ratio >= t:
            try:
                import httpx as _httpx
                async with _httpx.AsyncClient(timeout=5) as client:
                    await client.post(_budget_alert_config["webhook_url"], json={
                        "event": "budget_alert",
                        "entity_type": entity_type,
                        "entity_id": entity_id,
                        "spend_usd": round(spend, 4),
                        "budget_usd": round(budget, 2),
                        "threshold": t,
                        "ratio": round(ratio, 4),
                    })
            except Exception:
                _log.debug("budget webhook failed for %s/%s", entity_type, entity_id)
            break  # only fire highest crossed threshold
