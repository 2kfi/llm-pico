from __future__ import annotations

import asyncio
import json
import logging
import os
import time
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from starlette.responses import StreamingResponse

from core.auth import (
    check_model_access,
    hash_key,
    prefix_from_key,
    verify_api_key,
    verify_hmac_signature,
)
from api.dependencies import require_master_key
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
    get_model_chain,
    set_model_chain,
    get_chain_rewrites_response,
    set_chain_rewrites_response,
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
from core.usage import get_cost_stats, get_top_models, get_usage_stats, get_error_stats

_log = logging.getLogger("llm-pico.admin")

router = APIRouter()


async def _json_body(request: Request) -> dict:
    try:
        return await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })


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
#log { background:#111; border:1px solid #222; border-radius:6px; padding:12px; height:calc(100vh - 80px); overflow-y:auto; white-space:pre-wrap; word-break:break-all; }
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
const masterKey = localStorage.getItem('pico_master_key') || '';

fetch('/admin/logs/stream-token', {
  method: 'POST',
  headers: { 'Authorization': `Bearer ${masterKey}` }
})
.then(r => {
  if (!r.ok) throw new Error('Auth failed');
  return r.json();
})
.then(data => {
  const token = data.stream_token;
  const es = new EventSource('/admin/logs/stream?token=' + token);
  es.onmessage = e => {
    const d = JSON.parse(e.data);
    if (d.type === 'keepalive') return;
    const line = document.createElement('div');
    const ts = document.createElement('span'); ts.className = 'log-ts'; ts.textContent = d.ts || '';
    const model = document.createElement('span'); model.className = 'log-model'; model.textContent = d.model || '?';
    const status = document.createElement('span');
    status.className = d.status === 200 ? 'log-status-ok' : 'log-status-err';
    status.textContent = `[${d.status}]`;
    const tokens = document.createElement('span'); tokens.className = 'log-tokens'; tokens.textContent = `tokens:${d.total_tokens||0}`;
    const cost = document.createElement('span'); cost.className = 'log-cost'; cost.textContent = ` cost:$${(d.cost_usd||0).toFixed(6)}`;
    line.append(ts, ' ', model, ' ', status, ' ', tokens, ' ', cost, ' ', d.key_prefix || '');
    el.appendChild(line);
    el.scrollTop = el.scrollHeight;
    if (el.children.length > 1000) el.removeChild(el.children[0]);
  };
  es.onerror = () => {
    el.innerHTML = '<div style="color:#e55">Disconnected. <a href="" style="color:#6bf">Reload</a></div>';
  };
})
.catch(err => {
  el.innerHTML = '<div style="color:#e55">Failed to authenticate log stream.</div>';
});
</script>
</body>
</html>"""


async def _log_admin(action: str, actor_hash: str, details: dict[str, Any] | None = None,
                     client_ip: str | None = None) -> None:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    structured = json.dumps(details) if details else None
    async with get_db() as db:
        await db.execute(
            "INSERT INTO admin_log (action, actor_hash, details, structured_details, client_ip, created_at) VALUES (?, ?, ?, ?, ?, ?)",
            (action, actor_hash, structured, structured, client_ip, now),
        )
        await db.commit()
    # ponytail: also write JSONL for external tooling/compliance
    from core.auth import audit_log
    audit_log(action, actor=actor_hash[:16], ip=client_ip, **(details or {}))


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

    body = await _json_body(request)

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

    body = await _json_body(request)

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

    body = await _json_body(request)

    pattern = _prefix_pattern(prefix)
    async with get_db() as db:
        cursor = await db.execute(
            """UPDATE user_keys SET
               rpm_limit = COALESCE(?, rpm_limit),
               rpd_limit = COALESCE(?, rpd_limit),
               tpm_limit = COALESCE(?, tpm_limit),
               tpd_limit = COALESCE(?, tpd_limit),
               ash_limit = COALESCE(?, ash_limit),
               asd_limit = COALESCE(?, asd_limit)
               WHERE key_prefix LIKE ? AND is_active = 1""",
            (body.get("rpm"), body.get("rpd"), body.get("tpm"), body.get("tpd"),
             body.get("ash"), body.get("asd"), pattern),
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

    body = await _json_body(request)

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

    body = await _json_body(request)

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

    body = await _json_body(request)

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

    body = await _json_body(request)

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


@router.put("/teams/{team_id}/chain")
async def api_set_team_chain(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:
    body = await request.body()
    try:
        data = json.loads(body)
    except (json.JSONDecodeError, TypeError):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    model_chain = data.get("model_chain")
    if not isinstance(model_chain, list):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "model_chain must be a list of model names", "type": "bad_request", "code": 400}
        })

    team = await get_team(team_id)
    if not team:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Team not found", "type": "not_found", "code": 404}
        })

    await set_model_chain(team_id, model_chain)
    await _log_admin("set_model_chain", _actor, {"team_id": team_id, "model_chain": model_chain})

    return Response(
        content=json.dumps({"team_id": team_id, "model_chain": model_chain}),
        media_type="application/json",
    )


@router.get("/teams/{team_id}/chain")
async def api_get_team_chain(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:
    team = await get_team(team_id)
    if not team:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Team not found", "type": "not_found", "code": 404}
        })

    chain = await get_model_chain(team_id)
    return Response(
        content=json.dumps({"team_id": team_id, "model_chain": chain or []}),
        media_type="application/json",
    )


@router.put("/teams/{team_id}/chain/rewrites")
async def api_set_chain_rewrites(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:
    body = await request.body()
    try:
        data = json.loads(body)
    except (json.JSONDecodeError, TypeError):
        raise HTTPException(status_code=400, detail={"error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}})
    text = data.get("rewrites_response")
    if text is not None and not isinstance(text, str):
        raise HTTPException(status_code=400, detail={"error": {"message": "rewrites_response must be a string or null", "type": "bad_request", "code": 400}})
    team = await get_team(team_id)
    if not team:
        raise HTTPException(status_code=404, detail={"error": {"message": "Team not found", "type": "not_found", "code": 404}})
    await set_chain_rewrites_response(team_id, text)
    await _log_admin("set_chain_rewrites", _actor, {"team_id": team_id, "rewrites_response": text})
    return Response(content=json.dumps({"team_id": team_id, "chain_rewrites_response": text}), media_type="application/json")


@router.get("/teams/{team_id}/chain/rewrites")
async def api_get_chain_rewrites(request: Request, team_id: int, _actor: str = Depends(require_master_key)) -> Response:
    team = await get_team(team_id)
    if not team:
        raise HTTPException(status_code=404, detail={"error": {"message": "Team not found", "type": "not_found", "code": 404}})
    text = await get_chain_rewrites_response(team_id)
    return Response(content=json.dumps({"team_id": team_id, "chain_rewrites_response": text}), media_type="application/json")


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


@router.get("/traces/{request_id}")
async def api_get_trace(request_id: str, _actor: str = Depends(require_master_key)) -> Response:
    from core.db import get_traces
    traces = await get_traces(request_id)
    return Response(content=json.dumps(traces), media_type="application/json")


@router.get("/usage/recent")
async def api_recent_requests(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    params = dict(request.query_params)
    limit = _parse_limit(params)
    stats = await get_usage_stats(
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=limit,
    )
    return Response(content=json.dumps(stats), media_type="application/json")


# ---- User endpoints ----

@router.post("/teams/{team_id}/users")
async def api_create_user(request: Request, team_id: int, actor_hash: str = Depends(require_master_key)) -> Response:

    body = await _json_body(request)

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

    body = await _json_body(request)

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

    body = await _json_body(request)

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

    body = await _json_body(request)

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


@router.get("/stats/errors")
async def error_stats(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    params = dict(request.query_params)
    stats = await get_error_stats(
        provider=params.get("provider"),
        model=params.get("model"),
        from_date=params.get("from"),
        to_date=params.get("to"),
    )
    return Response(
        content=json.dumps({"errors": stats}),
        media_type="application/json",
    )


@router.get("/stats/metrics")
async def prometheus_metrics(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    lines: list[str] = []

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT model_name, provider, status_code, COUNT(*) as cnt
               FROM usage_log GROUP BY model_name, provider, status_code"""
        )
        rows = await cursor.fetchall()

    lines.append("# HELP llm_pico_requests_total Total requests by model, provider, status")
    lines.append("# TYPE llm_pico_requests_total counter")
    for row in rows:
        m = row["model_name"].replace("\\", "\\\\").replace('"', '\\"')
        p = row["provider"].replace("\\", "\\\\").replace('"', '\\"')
        lines.append(f'llm_pico_requests_total{{model="{m}",provider="{p}",status="{row["status_code"]}"}} {row["cnt"]}')

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT model_name, provider,
                      SUM(total_tokens) as tokens, SUM(cost_usd) as cost
               FROM usage_log GROUP BY model_name, provider"""
        )
        rows = await cursor.fetchall()

    lines.append("# HELP llm_pico_tokens_total Total tokens by model and provider")
    lines.append("# TYPE llm_pico_tokens_total counter")
    for row in rows:
        m = row["model_name"].replace("\\", "\\\\").replace('"', '\\"')
        p = row["provider"].replace("\\", "\\\\").replace('"', '\\"')
        lines.append(f'llm_pico_tokens_total{{model="{m}",provider="{p}"}} {row["tokens"] or 0}')

    lines.append("# HELP llm_pico_cost_usd_total Total cost by model and provider")
    lines.append("# TYPE llm_pico_cost_usd_total counter")
    for row in rows:
        m = row["model_name"].replace("\\", "\\\\").replace('"', '\\"')
        p = row["provider"].replace("\\", "\\\\").replace('"', '\\"')
        lines.append(f'llm_pico_cost_usd_total{{model="{m}",provider="{p}"}} {row["cost"] or 0}')

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT error_type, COUNT(*) as cnt
               FROM usage_log WHERE error_type IS NOT NULL
               GROUP BY error_type"""
        )
        rows = await cursor.fetchall()

    lines.append("# HELP llm_pico_errors_total Total errors by type")
    lines.append("# TYPE llm_pico_errors_total counter")
    for row in rows:
        lines.append(f'llm_pico_errors_total{{type="{row["error_type"]}"}} {row["cnt"]}')

    return Response(
        content="\n".join(lines) + "\n",
        media_type="text/plain; version=0.0.4; charset=utf-8",
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


@router.post("/logs/stream-token")
async def create_stream_token(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:
    import secrets
    import time
    
    if not hasattr(request.app.state, "active_stream_tokens"):
        request.app.state.active_stream_tokens = {}
    
    now = time.time()
    request.app.state.active_stream_tokens = {
        t: exp for t, exp in request.app.state.active_stream_tokens.items() if exp > now
    }
    
    token = secrets.token_hex(16)
    request.app.state.active_stream_tokens[token] = now + 300.0
    
    return Response(
        content=json.dumps({"stream_token": token}),
        media_type="application/json"
    )


@router.get("/logs/stream")
async def log_stream(request: Request, token: str | None = None) -> StreamingResponse:
    from fastapi import HTTPException
    import hmac
    import time
    
    if not token:
        auth_header = request.headers.get("Authorization")
        if auth_header and auth_header.startswith("Bearer "):
            raw_key = auth_header[7:].strip()
            config = request.app.state.config
            master_key = config.general_settings.master_key
            if not hmac.compare_digest(raw_key, master_key):
                raise HTTPException(status_code=401, detail={
                    "error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}
                })
        else:
            raise HTTPException(status_code=401, detail={
                "error": {"message": "Missing or invalid token", "type": "unauthorized", "code": 401}
            })
    else:
        active_tokens = getattr(request.app.state, "active_stream_tokens", {})
        now = time.time()
        if token not in active_tokens or active_tokens[token] < now:
            raise HTTPException(status_code=401, detail={
                "error": {"message": "Invalid or expired stream token", "type": "unauthorized", "code": 401}
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
    return Response(content=HTML_DASHBOARD, media_type="text/html")


# ---- First-boot init ----

@router.post("/init")
async def init_instance(request: Request) -> Response:
    from core.config import save_settings
    import secrets

    config = request.app.state.config
    if config and config.general_settings.master_key:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Instance already initialized", "type": "bad_request", "code": 400}
        })

    master_key = "sk-pico-" + secrets.token_hex(32)
    await save_settings({"master_key": master_key})
    await _log_admin("init_instance", hash_key(master_key))

    # ponytail: reload config so in-memory master_key matches DB
    from core.config import load_config_from_db
    request.app.state.config = await load_config_from_db()

    return Response(
        content=json.dumps({"master_key": master_key}),
        status_code=201,
        media_type="application/json",
    )


# ---- Init status (no auth) ----

@router.get("/init/status")
async def init_status(request: Request) -> Response:
    config = request.app.state.config
    configured = bool(config and config.general_settings.master_key)
    return Response(
        content=json.dumps({"initialized": configured}),
        media_type="application/json",
    )


# ---- Auth: hash-based master key (no raw key in transit) ----

@router.post("/auth/init-master-key")
async def auth_init_master_key(request: Request) -> Response:
    from core.config import save_settings

    config = request.app.state.config
    if config and config.general_settings.master_key:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Instance already initialized", "type": "bad_request", "code": 400}
        })

    body = await _json_body(request)
    key_hash = body.get("keyHash")
    if not key_hash:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'keyHash' is required", "type": "bad_request", "code": 400}
        })

    # Store the hash — master_key field now holds the hash
    await save_settings({"master_key": key_hash})
    await _log_admin("auth_init_master_key", key_hash[:16])

    # Reload config so in-memory master_key matches
    from core.config import load_config_from_db
    request.app.state.config = await load_config_from_db()

    return Response(
        content=json.dumps({"ok": True}),
        status_code=201,
        media_type="application/json",
    )


@router.post("/auth/verify-master-key")
async def auth_verify_master_key(request: Request) -> Response:
    body = await _json_body(request)
    key_hash = body.get("keyHash")
    if not key_hash:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'keyHash' is required", "type": "bad_request", "code": 400}
        })

    config = request.app.state.config
    stored_hash = config.general_settings.master_key if config else None
    if not stored_hash:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Instance not initialized", "type": "unauthorized", "code": 401}
        })

    import hmac as hmac_mod
    if not hmac_mod.compare_digest(key_hash, stored_hash):
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid master key", "type": "unauthorized", "code": 401}
        })

    return Response(
        content=json.dumps({"ok": True}),
        media_type="application/json",
    )


# ---- Provider probe (fetch models from provider) ----

@router.post("/providers/probe")
async def probe_provider(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    body = await _json_body(request)
    provider = body.get("provider", "")
    api_key = body.get("api_key", "")
    base_url = body.get("base_url", "")
    account_id = body.get("account_id", "")

    if not api_key:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'api_key' is required", "type": "bad_request", "code": 400}
        })

    try:
        models = await _fetch_provider_models(provider, api_key, base_url, account_id)
        return Response(
            content=json.dumps({"models": models}),
            media_type="application/json",
        )
    except HTTPException:
        raise
    except Exception as e:
        _log.exception("provider probe failed: %s", provider)
        raise HTTPException(status_code=502, detail={
            "error": {"message": f"Provider probe failed: {e}", "type": "upstream_error", "code": 502}
        })


async def _fetch_provider_models(provider: str, api_key: str, base_url: str, account_id: str) -> list[dict]:
    """Fetch model list from a provider's API."""
    import httpx

    # Build URL and headers based on provider
    if provider == "cloudflare":
        if not account_id:
            raise HTTPException(status_code=400, detail={
                "error": {"message": "Cloudflare requires account_id", "type": "bad_request", "code": 400}
            })
        url = f"https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/models/search"
        headers = {"Authorization": f"Bearer {api_key}"}
    elif provider == "google":
        url = f"https://generativelanguage.googleapis.com/v1beta/models?key={api_key}"
        headers = {}
    elif provider == "anthropic":
        # Anthropic has no models endpoint — return hardcoded list
        return [
            {"id": "claude-opus-4-20250514", "name": "Claude Opus 4"},
            {"id": "claude-sonnet-4-20250514", "name": "Claude Sonnet 4"},
            {"id": "claude-3-5-haiku-20241022", "name": "Claude 3.5 Haiku"},
            {"id": "claude-3-5-sonnet-20241022", "name": "Claude 3.5 Sonnet"},
            {"id": "claude-3-opus-20240229", "name": "Claude 3 Opus"},
        ]
    else:
        # OpenAI-compatible: base_url may already include /v1
        if base_url:
            root = base_url.rstrip("/")
            url = root + "/models" if root.endswith("/v1") else root + "/v1/models"
        else:
            url = "https://api.openai.com/v1/models"
        headers = {"Authorization": f"Bearer {api_key}"}

    async with httpx.AsyncClient(timeout=15) as client:
        resp = await client.get(url, headers=headers)

    if resp.status_code == 401:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}
        })
    if resp.status_code == 403:
        raise HTTPException(status_code=403, detail={
            "error": {"message": "Account suspended or access denied", "type": "forbidden", "code": 403}
        })
    if resp.status_code == 402:
        raise HTTPException(status_code=402, detail={
            "error": {"message": "Quota exceeded", "type": "payment_required", "code": 402}
        })
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail={
            "error": {"message": f"Provider returned {resp.status_code}", "type": "upstream_error", "code": 502}
        })

    try:
        data = resp.json()
    except Exception:
        # Provider returned non-JSON (e.g. "Not Found" text) — no models available
        return []

    # Parse response based on provider format
    if provider == "google":
        # Google returns {models: [{name: "models/xxx", displayName: "..."}]}
        raw = data.get("models", [])
        return [{"id": m.get("name", "").replace("models/", ""), "name": m.get("displayName", m.get("name", ""))} for m in raw]
    elif provider == "cloudflare":
        # Cloudflare returns {result: [{id: "...", name: "..."}]}
        raw = data.get("result", [])
        return [{"id": m.get("id", ""), "name": m.get("name", m.get("id", ""))} for m in raw]
    else:
        # OpenAI-compatible: {data: [{id: "...", owned_by: "..."}]}
        raw = data.get("data", [])
        return [{"id": m.get("id", ""), "name": m.get("id", "")} for m in raw]


@router.post("/providers/sync")
async def sync_provider_models(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    """Sync model lists from all configured providers into the database."""
    from core.db import get_db
    body = await request.body()
    data = json.loads(body) if body else {}
    provider_filter = data.get("provider")  # optional: sync only one provider

    async with get_db() as db:
        cursor = await db.execute("SELECT DISTINCT provider FROM models")
        providers = [row[0] for row in await cursor.fetchall()]

    synced = 0
    skipped = 0
    errors = []
    for prov in providers:
        if provider_filter and prov != provider_filter:
            continue
        try:
            models = await _fetch_provider_models(prov, "")
            async with get_db() as db:
                for m in models:
                    model_id = m.get("id", "")
                    if not model_id:
                        continue
                    cursor = await db.execute("SELECT id FROM models WHERE model = ?", (model_id,))
                    if await cursor.fetchone():
                        skipped += 1
                        continue
                    await db.execute(
                        "INSERT INTO models (model_name, model, is_active, created_at) VALUES (?, ?, 1, ?)",
                        (model_id, model_id, time.strftime("%Y-%m-%dT%H:%M:%SZ")),
                    )
                    synced += 1
                await db.commit()
        except Exception as e:
            errors.append({"provider": prov, "error": str(e)[:200]})

    await _log_admin("sync_providers", _actor, {"synced": synced, "skipped": skipped, "errors": len(errors)})
    return Response(content=json.dumps({"synced": synced, "skipped": skipped, "errors": errors}), media_type="application/json")


# ---- Degradation mode ----

@router.post("/degradation")
async def set_degradation(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    from core.degradation import DegradationMode
    body = await request.body()
    data = json.loads(body) if body else {}
    mode_str = data.get("mode", "normal")
    try:
        mode = DegradationMode(mode_str)
    except ValueError:
        raise HTTPException(status_code=400, detail={"error": {"message": f"Invalid mode: {mode_str}. Use normal/reject/queue/fallback_only", "type": "bad_request", "code": 400}})
    deg = getattr(request.app.state, "degradation", None)
    if deg:
        deg.set_mode(mode)
    await _log_admin("set_degradation", _actor, {"mode": mode_str})
    return Response(content=json.dumps({"mode": mode_str}), media_type="application/json")


@router.get("/degradation")
async def get_degradation(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    deg = getattr(request.app.state, "degradation", None)
    mode = deg.mode.value if deg else "normal"
    queue = deg.queue_depth if deg else 0
    return Response(content=json.dumps({"mode": mode, "queue_depth": queue}), media_type="application/json")


# ---- Config CRUD ----

@router.get("/config")
async def get_config(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    from core.config import load_config_from_db
    cfg = await load_config_from_db()
    return Response(
        content=json.dumps({
            "general_settings": {
                "master_key": cfg.general_settings.master_key,
                "usage_log_retention_days": cfg.general_settings.usage_log_retention_days,
                "admin_log_retention_days": cfg.general_settings.admin_log_retention_days,
            },
            "router_settings": {
                "num_retries": cfg.router_settings.num_retries,
                "cooldown_time": cfg.router_settings.cooldown_time,
                "circuit_breaker": {
                    "enabled": cfg.router_settings.circuit_breaker.enabled,
                    "failure_threshold": cfg.router_settings.circuit_breaker.failure_threshold,
                    "recovery_timeout": cfg.router_settings.circuit_breaker.recovery_timeout,
                },
            },
        }),
        media_type="application/json",
    )


@router.put("/config/settings")
async def update_settings(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:
    body = await _json_body(request)

    from core.config import save_settings
    await save_settings(body)
    await _log_admin("update_settings", actor_hash, {"keys": list(body.keys())})

    return Response(
        content=json.dumps({"updated": True}),
        media_type="application/json",
    )


@router.get("/config/models")
async def list_models(request: Request, _actor: str = Depends(require_master_key)) -> Response:
    from core.config import get_provider_keys
    async with get_db() as db:
        cursor = await db.execute(
            """SELECT id, model_name, model, api_base, images, embeddings, stt, tts,
                      failover_model, can_cache, cost_per_1m_input, cost_per_1m_output,
                      rpm, rpd, tpm, tpd, ash, asd, is_active
               FROM models ORDER BY id"""
        )
        rows = await cursor.fetchall()

    models = []
    for row in rows:
        pk = await get_provider_keys(row["id"])
        models.append({
            "id": row["id"],
            "model_name": row["model_name"],
            "model": row["model"],
            "api_base": row["api_base"],
            "images": bool(row["images"]),
            "embeddings": bool(row["embeddings"]),
            "stt": bool(row["stt"]),
            "tts": bool(row["tts"]),
            "failover_model": row["failover_model"],
            "can_cache": bool(row["can_cache"]),
            "cost_per_1m_input": row["cost_per_1m_input"],
            "cost_per_1m_output": row["cost_per_1m_output"],
            "rpm": row["rpm"],
            "rpd": row["rpd"],
            "tpm": row["tpm"],
            "tpd": row["tpd"],
            "ash": row["ash"],
            "asd": row["asd"],
            "is_active": bool(row["is_active"]),
            "provider_keys": [{"id": k["id"], "priority": k["priority"], "is_active": bool(k["is_active"])} for k in pk],
        })

    return Response(
        content=json.dumps({"models": models, "total": len(models)}),
        media_type="application/json",
    )


@router.post("/config/models")
async def create_model(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:
    body = await _json_body(request)

    if not body.get("model_name") or not body.get("model"):
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Fields 'model_name' and 'model' are required", "type": "bad_request", "code": 400}
        })

    from core.config import save_model, save_provider_key
    model_id = await save_model(None, body)

    api_keys = body.get("api_keys") or []
    if isinstance(api_keys, str):
        api_keys = [api_keys]
    for i, key in enumerate(api_keys):
        if key:
            await save_provider_key(model_id, key, priority=i)

    await _log_admin("create_model", actor_hash, {"model_id": model_id, "model_name": body["model_name"]})

    return Response(
        content=json.dumps({"id": model_id, "model_name": body["model_name"]}),
        status_code=201,
        media_type="application/json",
    )


@router.put("/config/models/{model_id}")
async def update_model(request: Request, model_id: int, actor_hash: str = Depends(require_master_key)) -> Response:
    body = await _json_body(request)

    from core.config import save_model
    updated_id = await save_model(model_id, body)
    if not updated_id:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Model not found", "type": "not_found", "code": 404}
        })

    await _log_admin("update_model", actor_hash, {"model_id": model_id})

    return Response(
        content=json.dumps({"updated": True}),
        media_type="application/json",
    )


@router.delete("/config/models/{model_id}")
async def api_delete_model(request: Request, model_id: int, actor_hash: str = Depends(require_master_key)) -> Response:
    from core.config import delete_model
    deleted = await delete_model(model_id)
    if not deleted:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Model not found", "type": "not_found", "code": 404}
        })

    await _log_admin("delete_model", actor_hash, {"model_id": model_id})

    return Response(
        content=json.dumps({"deleted": True}),
        media_type="application/json",
    )


@router.post("/config/models/{model_id}/keys")
async def add_provider_key(request: Request, model_id: int, actor_hash: str = Depends(require_master_key)) -> Response:
    body = await _json_body(request)

    api_key = body.get("api_key")
    if not api_key:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'api_key' is required", "type": "bad_request", "code": 400}
        })

    from core.config import save_provider_key
    key_id = await save_provider_key(model_id, api_key, priority=body.get("priority", 0))
    await _log_admin("add_provider_key", actor_hash, {"model_id": model_id, "key_id": key_id})

    return Response(
        content=json.dumps({"id": key_id}),
        status_code=201,
        media_type="application/json",
    )


@router.delete("/config/keys/{key_id}")
async def api_delete_provider_key(request: Request, key_id: int, actor_hash: str = Depends(require_master_key)) -> Response:
    from core.config import delete_provider_key
    deleted = await delete_provider_key(key_id)
    if not deleted:
        raise HTTPException(status_code=404, detail={
            "error": {"message": "Key not found", "type": "not_found", "code": 404}
        })

    await _log_admin("delete_provider_key", actor_hash, {"key_id": key_id})

    return Response(
        content=json.dumps({"deleted": True}),
        media_type="application/json",
    )


# ---- Config reload (hot swap) ----

@router.post("/config/reload")
async def reload_config(request: Request, actor_hash: str = Depends(require_master_key)) -> Response:
    from core.config import reload_config as _reload_config

    success = await _reload_config(request.app.state)
    if not success:
        import os
        os.execve(os.sys.executable, [os.sys.executable, "-m", "api.cli"] + os.sys.argv[1:], os.environ)

    cfg = request.app.state.config
    await _log_admin("config_reload", actor_hash, {"models": len(cfg.model_list)})

    return Response(
        content=json.dumps({"reloaded": True, "models": len(cfg.model_list)}),
        media_type="application/json",
    )
