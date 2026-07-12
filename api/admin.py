from __future__ import annotations

import asyncio
import json
import logging
import os
import sys
import time
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from starlette.responses import StreamingResponse

from core.auth import (
    check_model_access,
    hash_key,
    prefix_from_key,
    require_master_key,
    verify_api_key,
)
from core.config import Config
from core.db import get_db
from core.events import subscribe, unsubscribe
from core.teams import (
    assign_key_to_user,
    check_user_budget,
    create_team,
    create_user,
    deactivate_team,
    get_team,
    get_team_month_spend,
    get_teams,
    get_user,
    get_user_month_spend,
    get_users,
    update_team_allowlist,
    update_team_limits,
    update_user_allowlist,
    update_user_budget,
    update_user_limits,
)
from core.usage import get_cost_stats, get_top_models, get_usage_stats

_log = logging.getLogger("llm-pico.admin")

router = APIRouter()


def _parse_limit(params: dict, default: int = 100) -> int:
    try:
        return int(params.get("limit", default))
    except (ValueError, TypeError):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "limit must be an integer", "type": "bad_request", "code": 400}
        })


def _prefix_pattern(prefix: str) -> str:
    suffix_idx = prefix.rfind("...")
    base = prefix[:suffix_idx] if suffix_idx != -1 else prefix
    return base + "...%"

HTML_DASHBOARD = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>llm-pico Live Logs</title>
<style>
body { background:#0a0a0a; color:#e0e0e0; font:14px/1.4 'SF Mono','Fira Code','Consolas',monospace; margin:0; padding:20px; }
h1 { color:#888; font-size:16px; font-weight:400; margin:0 0 12px 0; }
h1 span { color:#3a8; }
#log { background:#111; border:1px solid #222; border-radius:6px; padding:12px; height:calc(100vh-80px); overflow-y:auto; white-space:pre-wrap; word-break:break-all; }
#log div:hover { background:#1a1a1a; }
.log-ts { color:#555; }
.log-model { color:#6bf; }
.log-tokens { color:#8a8; }
.log-cost { color:#fa8; }
.log-status { color:#888; }
.log-status-ok { color:#3a8; }
.log-status-err { color:#e55; }
</style>
</head>
<body>
<h1>llm-pico <span>live log stream</span></h1>
<div id="log"><div style="color:#555">Connecting...</div></div>
<script>
const el=document.getElementById('log');
const es=new EventSource('/admin/logs/stream?token=__MASTER_KEY__');
es.onmessage=e=>{
  const d=JSON.parse(e.data);
  if(d.type==='keepalive') return;
  const line=document.createElement('div');
  const ts=document.createElement('span'); ts.className='log-ts'; ts.textContent=d.ts||'';
  const model=document.createElement('span'); model.className='log-model'; model.textContent=d.model||'?';
  const status=document.createElement('span');
  status.className=d.status===200?'log-status-ok':'log-status-err';
  status.textContent=`[${d.status}]`;
  const tokens=document.createElement('span'); tokens.className='log-tokens'; tokens.textContent=`tokens:${d.total_tokens||0}`;
  const cost=document.createElement('span'); cost.className='log-cost'; cost.textContent=` cost:$${(d.cost_usd||0).toFixed(6)}`;
  line.append(ts,' ',model,' ',status,' ',tokens,' ',cost,' ',d.key_prefix||'');
  el.appendChild(line);
  el.scrollTop=el.scrollHeight;
  if(el.children.length>1000) el.removeChild(el.children[0]);
};
es.onerror=()=>{el.innerHTML='<div style="color:#e55">Disconnected. <a href="" style="color:#6bf">Reload</a></div>'};
</script>
</body>
</html>"""


async def _log_admin(action: str, actor_hash: str, details: dict[str, Any] | None = None) -> None:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        await db.execute(
            "INSERT INTO admin_log (action, actor_hash, details, created_at) VALUES (?, ?, ?, ?)",
            (action, actor_hash, json.dumps(details) if details else None, now),
        )
        await db.commit()


# ---- Key endpoints ----

@router.get("/keys")
async def list_keys(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT key_prefix, label, is_active, created_at, expires_at,
                      model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit, user_id
               FROM user_keys ORDER BY created_at DESC"""
        )
        rows = await cursor.fetchall()

    keys = []
    for row in rows:
        allowlist = None
        if row["model_allowlist"]:
            try:
                allowlist = json.loads(row["model_allowlist"])
            except (json.JSONDecodeError, TypeError):
                pass
        keys.append({
            "key_prefix": row["key_prefix"],
            "label": row["label"],
            "is_active": bool(row["is_active"]),
            "created_at": row["created_at"],
            "expires_at": row["expires_at"],
            "model_allowlist": allowlist,
            "rpm_limit": row["rpm_limit"],
            "rpd_limit": row["rpd_limit"],
            "tpm_limit": row["tpm_limit"],
            "tpd_limit": row["tpd_limit"],
            "user_id": row["user_id"],
        })

    return Response(
        content=json.dumps({"keys": keys, "total": len(keys)}),
        media_type="application/json",
    )


@router.post("/keys")
async def create_key(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    raw_key = "sk-pico-" + os.urandom(32).hex()
    key_hash = hash_key(raw_key)
    key_prefix = prefix_from_key(raw_key)
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())

    allowlist_json = json.dumps(body.get("models")) if body.get("models") else None

    async with get_db() as db:
        await db.execute(
            """INSERT INTO user_keys
               (key_hash, key_prefix, label, is_active, created_at, expires_at,
                model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit, user_id)
               VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                key_hash,
                key_prefix,
                body.get("label"),
                now,
                body.get("expires_at"),
                allowlist_json,
                body.get("rpm_limit"),
                body.get("rpd_limit"),
                body.get("tpm_limit"),
                body.get("tpd_limit"),
                body.get("user_id"),
            ),
        )
        await db.commit()

    await _log_admin("create_key", actor_hash, {"key_prefix": key_prefix, "label": body.get("label")})

    return Response(
        content=json.dumps({
            "key": raw_key,
            "key_prefix": key_prefix,
            "label": body.get("label"),
        }),
        status_code=201,
        media_type="application/json",
    )


@router.delete("/keys/{prefix}")
async def delete_key(request: Request, prefix: str, actor_hash: str = Depends(require_master_key)) -> Response:

    pattern = _prefix_pattern(prefix)

    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE user_keys SET is_active = 0 WHERE key_prefix LIKE ? AND is_active = 1",
            (pattern,),
        )
        affected = cursor.rowcount
        await db.commit()

    if affected == 0:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "No active key found with that prefix", "type": "not_found", "code": 404}
        })

    await _log_admin("revoke_key", actor_hash, {"key_prefix": prefix})

    return Response(
        content=json.dumps({"revoked": affected}),
        media_type="application/json",
    )


@router.put("/keys/{prefix}/models")
async def set_key_models(request: Request, prefix: str, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    models = body.get("models")
    if models is not None and not isinstance(models, list):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "'models' must be a list or null", "type": "bad_request", "code": 400}
        })

    allowlist_json = json.dumps(models) if models is not None else None
    pattern = _prefix_pattern(prefix)

    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE user_keys SET model_allowlist = ? WHERE key_prefix LIKE ? AND is_active = 1",
            (allowlist_json, pattern),
        )
        affected = cursor.rowcount
        await db.commit()

    if affected == 0:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "No active key found with that prefix", "type": "not_found", "code": 404}
        })

    await _log_admin("set_key_models", actor_hash, {"key_prefix": prefix, "models": models})

    return Response(
        content=json.dumps({"updated": affected, "models": models}),
        media_type="application/json",
    )


@router.put("/keys/{prefix}/limits")
async def set_key_limits(request: Request, prefix: str, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    pattern = _prefix_pattern(prefix)
    async with get_db() as db:
        cursor = await db.execute(
            """UPDATE user_keys SET
               rpm_limit = COALESCE(?, rpm_limit),
               rpd_limit = COALESCE(?, rpd_limit),
               tpm_limit = COALESCE(?, tpm_limit),
               tpd_limit = COALESCE(?, tpd_limit)
               WHERE key_prefix LIKE ? AND is_active = 1""",
            (body.get("rpm"), body.get("rpd"), body.get("tpm"), body.get("tpd"), pattern),
        )
        affected = cursor.rowcount
        await db.commit()

    if affected == 0:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "No active key found with that prefix", "type": "not_found", "code": 404}
        })

    await _log_admin("set_key_limits", actor_hash, {"key_prefix": prefix, "limits": body})

    return Response(
        content=json.dumps({"updated": affected}),
        media_type="application/json",
    )


@router.put("/keys/{prefix}/user")
async def assign_key_user(request: Request, prefix: str, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    user_id = body.get("user_id")
    pattern = _prefix_pattern(prefix)

    async with get_db() as db:
        cursor = await db.execute(
            "UPDATE user_keys SET user_id = ? WHERE key_prefix LIKE ? AND is_active = 1",
            (user_id, pattern),
        )
        affected = cursor.rowcount
        await db.commit()

    if affected == 0:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "No active key found with that prefix", "type": "not_found", "code": 404}
        })

    await _log_admin("assign_key_user", actor_hash, {"key_prefix": prefix, "user_id": user_id})

    return Response(
        content=json.dumps({"updated": affected, "user_id": user_id}),
        media_type="application/json",
    )


# ---- Team endpoints ----

@router.post("/teams")
async def api_create_team(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    name = body.get("name")
    if not name:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'name' is required", "type": "bad_request", "code": 400}
        })

    team = await create_team(name, body.get("description"))
    await _log_admin("create_team", actor_hash, {"team_id": team["id"], "name": name})

    return Response(
        content=json.dumps(team),
        status_code=201,
        media_type="application/json",
    )


@router.get("/teams")
async def api_list_teams(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    teams = await get_teams()
    return Response(
        content=json.dumps({"teams": teams, "total": len(teams)}),
        media_type="application/json",
    )


@router.get("/teams/{team_id}")
async def api_get_team(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:

    team = await get_team(team_id)
    if team is None:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Team not found", "type": "not_found", "code": 404}
        })

    month_spend = await get_team_month_spend(team_id)
    team["month_spend_usd"] = month_spend

    return Response(
        content=json.dumps(team),
        media_type="application/json",
    )


@router.put("/teams/{team_id}/limits")
async def api_update_team_limits(request: Request, team_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    updated = await update_team_limits(team_id, body)
    if not updated:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Team not found", "type": "not_found", "code": 404}
        })

    await _log_admin("update_team_limits", actor_hash, {"team_id": team_id, "limits": body})

    return Response(
        content=json.dumps({"updated": True}),
        media_type="application/json",
    )


@router.put("/teams/{team_id}/models")
async def api_update_team_models(request: Request, team_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    models = body.get("models")
    if models is not None and not isinstance(models, list):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "'models' must be a list or null", "type": "bad_request", "code": 400}
        })

    updated = await update_team_allowlist(team_id, models)
    if not updated:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Team not found", "type": "not_found", "code": 404}
        })

    await _log_admin("update_team_models", actor_hash, {"team_id": team_id, "models": models})

    return Response(
        content=json.dumps({"updated": True, "models": models}),
        media_type="application/json",
    )


@router.delete("/teams/{team_id}")
async def api_deactivate_team(request: Request, team_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    updated = await deactivate_team(team_id)
    if not updated:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Team not found", "type": "not_found", "code": 404}
        })

    await _log_admin("deactivate_team", actor_hash, {"team_id": team_id})

    return Response(
        content=json.dumps({"deactivated": True}),
        media_type="application/json",
    )


@router.get("/teams/{team_id}/usage")
async def api_team_usage(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:

    params = dict(request.query_params)
    stats = await get_usage_stats(
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=_parse_limit(params),
        team_id=team_id,
    )

    return Response(
        content=json.dumps(stats),
        media_type="application/json",
    )


# ---- User endpoints ----

@router.post("/teams/{team_id}/users")
async def api_create_user(request: Request, team_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    email = body.get("email")
    name = body.get("name")
    if not email or not name:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Fields 'email' and 'name' are required", "type": "bad_request", "code": 400}
        })

    user = await create_user(team_id, email, name)
    await _log_admin("create_user", actor_hash, {"team_id": team_id, "user_id": user["id"], "email": email})

    return Response(
        content=json.dumps(user),
        status_code=201,
        media_type="application/json",
    )


@router.get("/teams/{team_id}/users")
async def api_list_users(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:
    users = await get_users(team_id)
    return Response(
        content=json.dumps({"users": users, "total": len(users)}),
        media_type="application/json",
    )


@router.get("/users/{user_id}")
async def api_get_user(request: Request, user_id: int, _actor: str = Depends(require_master_key)) -> Response:

    user = await get_user(user_id)
    if user is None:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "User not found", "type": "not_found", "code": 404}
        })

    month_spend = await get_user_month_spend(user_id)
    user["month_spend_usd"] = month_spend

    return Response(
        content=json.dumps(user),
        media_type="application/json",
    )


@router.put("/users/{user_id}/limits")
async def api_update_user_limits(request: Request, user_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    updated = await update_user_limits(user_id, body)
    if not updated:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "User not found", "type": "not_found", "code": 404}
        })

    await _log_admin("update_user_limits", actor_hash, {"user_id": user_id, "limits": body})

    return Response(
        content=json.dumps({"updated": True}),
        media_type="application/json",
    )


@router.put("/users/{user_id}/budget")
async def api_update_user_budget(request: Request, user_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    monthly_budget_usd = body.get("monthly_budget_usd")
    if monthly_budget_usd is not None:
        monthly_budget_usd = float(monthly_budget_usd)

    updated = await update_user_budget(user_id, monthly_budget_usd)
    if not updated:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "User not found", "type": "not_found", "code": 404}
        })

    await _log_admin("update_user_budget", actor_hash, {"user_id": user_id, "budget": monthly_budget_usd})

    return Response(
        content=json.dumps({"updated": True, "monthly_budget_usd": monthly_budget_usd}),
        media_type="application/json",
    )


@router.put("/users/{user_id}/models")
async def api_update_user_models(request: Request, user_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    models = body.get("models")
    if models is not None and not isinstance(models, list):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "'models' must be a list or null", "type": "bad_request", "code": 400}
        })

    updated = await update_user_allowlist(user_id, models)
    if not updated:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "User not found", "type": "not_found", "code": 404}
        })

    await _log_admin("update_user_models", actor_hash, {"user_id": user_id, "models": models})

    return Response(
        content=json.dumps({"updated": True, "models": models}),
        media_type="application/json",
    )


@router.get("/users/{user_id}/usage")
async def api_user_usage(request: Request, user_id: int, _actor: str = Depends(require_master_key)) -> Response:

    params = dict(request.query_params)
    stats = await get_usage_stats(
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=_parse_limit(params),
        user_id=user_id,
    )

    return Response(
        content=json.dumps(stats),
        media_type="application/json",
    )


# ---- Budgets aggregate endpoint ----

@router.get("/budgets")
async def budgets_summary(
    request: Request,
    actor_hash: str = Depends(require_master_key),
) -> Response:
    """Return all users across all teams with their budget info and current month spend."""
    async with get_db() as conn:
        cursor = await conn.execute("""
            SELECT u.id, u.name, u.email, u.monthly_budget_usd,
                   t.name as team_name, t.id as team_id,
                   COALESCE(SUM(ul.cost_usd), 0) as current_spend
            FROM users u
            JOIN teams t ON u.team_id = t.id
            LEFT JOIN user_keys uk ON uk.user_id = u.id
            LEFT JOIN usage_log ul ON ul.key_hash = uk.key_hash
                AND ul.created_at >= date('now', 'start of month')
            WHERE u.is_active = 1
            GROUP BY u.id
            ORDER BY t.name, u.name
        """)
        rows = await cursor.fetchall()

    return Response(
        content=json.dumps({
            "users": [
                {
                    "id": row["id"],
                    "name": row["name"],
                    "email": row["email"],
                    "team_id": row["team_id"],
                    "team_name": row["team_name"],
                    "monthly_budget_usd": row["monthly_budget_usd"],
                    "current_spend": row["current_spend"],
                }
                for row in rows
            ]
        }),
        media_type="application/json",
    )


# ---- Stats endpoints ----

@router.get("/usage")
async def usage(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    params = dict(request.query_params)
    stats = await get_usage_stats(
        key_hash=params.get("key_hash"),
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=_parse_limit(params),
    )

    return Response(
        content=json.dumps(stats),
        media_type="application/json",
    )


@router.get("/usage/top-models")
async def top_models(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    params = dict(request.query_params)
    models = await get_top_models(
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=_parse_limit(params, default=10),
    )

    return Response(
        content=json.dumps({"models": models}),
        media_type="application/json",
    )


@router.get("/stats/costs")
async def cost_stats(request: Request, _actor: str = Depends(require_master_key)) -> Response:

    params = dict(request.query_params)
    group_by = params.get("group_by", "user")
    if group_by not in ("user", "model", "day"):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "group_by must be 'user', 'model', or 'day'", "type": "bad_request", "code": 400}
        })

    stats = await get_cost_stats(
        group_by=group_by,
        from_date=params.get("from"),
        to_date=params.get("to"),
    )

    return Response(
        content=json.dumps({f"costs_by_{group_by}": stats}),
        media_type="application/json",
    )


# ---- Log endpoints ----

@router.get("/log")
async def admin_log(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    limit = _parse_limit(dict(request.query_params), default=50)

    async with get_db() as db:
        cursor = await db.execute(
            "SELECT action, actor_hash, details, created_at FROM admin_log ORDER BY created_at DESC LIMIT ?",
            (limit,),
        )
        rows = await cursor.fetchall()

    entries = []
    for row in rows:
        details = None
        if row["details"]:
            try:
                details = json.loads(row["details"])
            except (json.JSONDecodeError, TypeError):
                details = row["details"]
        entries.append({
            "action": row["action"],
            "actor_hash": row["actor_hash"],
            "details": details,
            "created_at": row["created_at"],
        })

    return Response(
        content=json.dumps({"entries": entries, "total": len(entries)}),
        media_type="application/json",
    )


@router.get("/logs/stream")
async def log_stream(request: Request, token: str | None = None) -> StreamingResponse:
    from fastapi import HTTPException
    import hmac
    config = request.app.state.config
    master_key = config.general_settings.master_key
    raw_key = token
    if not raw_key:
        auth_header = request.headers.get("Authorization")
        if auth_header and auth_header.startswith("Bearer "):
            raw_key = auth_header[7:].strip()
    if not raw_key or not hmac.compare_digest(raw_key, master_key):
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid or missing master API key",
                       "type": "unauthorized", "code": 401}
        })
    q = subscribe()

    async def generate():
        try:
            while True:
                payload = await asyncio.wait_for(q.get(), timeout=30)
                yield f"data: {payload}\n\n"
        except asyncio.TimeoutError:
            yield f"data: {json.dumps({'type': 'keepalive'})}\n\n"
        finally:
            unsubscribe(q)

    return StreamingResponse(generate(), media_type="text/event-stream")


@router.get("/logs", include_in_schema=False)
async def log_dashboard(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    config = request.app.state.config
    master_key = config.general_settings.master_key
    html = HTML_DASHBOARD.replace("__MASTER_KEY__", master_key)
    return Response(content=html, media_type="text/html")


# ---- Config reload ----

@router.post("/config/reload")
async def reload_config(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:

    import api.server as srv

    srv._is_draining = True
    _log.warning("config reload initiated, draining in-flight requests")

    drained = await srv._wait_for_drain(timeout=120.0)
    await _log_admin("config_reload", actor_hash, {"drained": drained})

    _log.info("restarting process for config reload")
    os.execve(sys.executable, [sys.executable] + sys.argv, os.environ)
