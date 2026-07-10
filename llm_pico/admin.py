from __future__ import annotations

import asyncio
import json
import logging
import os
import time
from typing import Any

from fastapi import APIRouter, HTTPException, Request, Response

from .auth import (
    check_model_access,
    extract_bearer,
    hash_key,
    prefix_from_key,
    verify_api_key,
)
from .config import Config
from .db import get_db
from .usage import get_top_models, get_usage_stats

_log = logging.getLogger("llm-pico.admin")

router = APIRouter()


async def _require_master(request: Request) -> str:
    config: Config = getattr(request.app.state, "config")
    master_key = config.general_settings.master_key

    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key or raw_key != master_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid or missing master API key", "type": "unauthorized", "code": 401}
        })

    return raw_key


async def _log_admin(action: str, actor_hash: str, details: dict[str, Any] | None = None) -> None:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        await db.execute(
            "INSERT INTO admin_log (action, actor_hash, details, created_at) VALUES (?, ?, ?, ?)",
            (action, actor_hash, json.dumps(details) if details else None, now),
        )
        await db.commit()


@router.get("/keys")
async def list_keys(request: Request) -> Response:
    actor_hash = await _require_master(request)

    async with get_db() as db:
        cursor = await db.execute(
            "SELECT key_prefix, label, is_active, created_at, expires_at, model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit FROM user_keys ORDER BY created_at DESC"
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
        })

    return Response(
        content=json.dumps({"keys": keys, "total": len(keys)}),
        media_type="application/json",
    )


@router.post("/keys")
async def create_key(request: Request) -> Response:
    actor_hash = await _require_master(request)

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
                model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit)
               VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)""",
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
async def delete_key(request: Request, prefix: str) -> Response:
    actor_hash = await _require_master(request)

    pattern = prefix.rstrip("...") + "%"

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
async def set_key_models(request: Request, prefix: str) -> Response:
    actor_hash = await _require_master(request)

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
    pattern = prefix.rstrip("...") + "%"

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
async def set_key_limits(request: Request, prefix: str) -> Response:
    actor_hash = await _require_master(request)

    try:
        body = await request.json()
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    pattern = prefix.rstrip("...") + "%"
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


@router.get("/usage")
async def usage(request: Request) -> Response:
    actor_hash = await _require_master(request)

    params = dict(request.query_params)
    stats = await get_usage_stats(
        key_hash=params.get("key_hash"),
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=int(params.get("limit", 100)),
    )

    return Response(
        content=json.dumps(stats),
        media_type="application/json",
    )


@router.get("/usage/top-models")
async def top_models(request: Request) -> Response:
    actor_hash = await _require_master(request)

    params = dict(request.query_params)
    models = await get_top_models(
        from_date=params.get("from"),
        to_date=params.get("to"),
        limit=int(params.get("limit", 10)),
    )

    return Response(
        content=json.dumps({"models": models}),
        media_type="application/json",
    )


@router.get("/log")
async def admin_log(request: Request) -> Response:
    actor_hash = await _require_master(request)

    limit = int(request.query_params.get("limit", 50))

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


@router.post("/config/reload")
async def reload_config(request: Request) -> Response:
    actor_hash = await _require_master(request)

    import llm_pico.server as srv

    srv._is_draining = True
    _log.warning("config reload initiated, draining in-flight requests")

    drained = await srv._wait_for_drain(timeout=120.0)
    await _log_admin("config_reload", actor_hash, {"drained": drained})

    _log.info("restarting process for config reload")
    os.execve(sys.executable, [sys.executable] + sys.argv, os.environ)
