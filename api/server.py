from __future__ import annotations

import asyncio
import json
import logging
import os
import sys
import time
from contextlib import asynccontextmanager
from typing import Any

import httpx
from fastapi import Depends, FastAPI, Form, HTTPException, Request, Response, UploadFile
from fastapi.middleware.cors import CORSMiddleware
from starlette.responses import StreamingResponse

from providers import get_adapter
from providers.base import BaseAdapter, close_all_clients
from providers.openai import OpenAIAdapter
from api.admin import router as admin_router
from core.auth import (
    check_model_access,
    hash_key,
    prefix_from_key,
    require_api_key,
    seed_users,
)
from core.cache import get_cached, set_cached
from core.config import Config
from core.db import close_db, init_db, prune_logs
from core.events import emit
from core.placeholder import router as placeholder_router
from core.ratelimit import get_limiter
from core.router import Router
from core.teams import check_user_budget
from core.usage import compute_cost, log_usage
from website.routes import router as website_router

_log = logging.getLogger("llm-pico.server")

_ALL_WINDOWS = ("rpm", "rpd", "tpm", "tpd", "ash", "asd")


async def _build_rate_limit_headers(
    limiter,
    key_hash: str,
    model_name: str,
    user_limits: dict[str, int | None],
    model_limits: dict[str, int | None],
) -> dict[str, str]:
    """Build X-RateLimit-* headers from current limiter usage."""
    headers: dict[str, str] = {}
    now = time.time()
    for level, limits in [("user", user_limits), ("model", model_limits)]:
        for window in _ALL_WINDOWS:
            limit = limits.get(window)
            if limit is None:
                continue
            count = await limiter.get_usage(key_hash, model_name, level, window) or 0
            remaining = max(0, limit - count)
            reset_ts = limiter._window_end(window, now)
            prefix = f"X-RateLimit-{window.upper()}"
            headers[f"{prefix}-Limit"] = str(limit)
            headers[f"{prefix}-Remaining"] = str(remaining)
            headers[f"{prefix}-Reset"] = str(reset_ts)
    return headers


_in_flight: set[str] = set()
_in_flight_lock = asyncio.Lock()
_is_draining = False


async def _track_request(request_id: str):
    async with _in_flight_lock:
        _in_flight.add(request_id)


async def _untrack_request(request_id: str):
    async with _in_flight_lock:
        _in_flight.discard(request_id)


async def _wait_for_drain(timeout: float = 120.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        async with _in_flight_lock:
            if not _in_flight:
                return True
        await asyncio.sleep(0.5)
    remaining = 0
    async with _in_flight_lock:
        remaining = len(_in_flight)
    _log.warning("drain timeout: %d requests still in flight", remaining)
    return False


@asynccontextmanager
async def lifespan(app: FastAPI):
    s = app.state
    config: Config = s.config
    db_path: str = s.db_path
    users: list[dict] = getattr(s, "users", [])
    verbose: bool = getattr(s, "verbose", False)

    await init_db(db_path)
    await seed_users(users)

    router = Router(config)
    s.router = router

    limiter = get_limiter()
    limiter.start()
    s.limiter = limiter

    _log.info("llm-pico ready: %d models, %d adapters", len(config.model_list), len(router.get_model_names()))

    async def _log_pruner(cfg: Config):
        while True:
            try:
                usg, adm = await prune_logs(
                    cfg.general_settings.usage_log_retention_days,
                    cfg.general_settings.admin_log_retention_days,
                )
                if usg > 0 or adm > 0:
                    _log.info("Pruned %d usage rows and %d admin rows", usg, adm)
            except Exception:
                _log.exception("Log pruning failed")
            await asyncio.sleep(3600)

    pruner_task = asyncio.create_task(_log_pruner(config))

    yield

    pruner_task.cancel()
    try:
        await pruner_task
    except asyncio.CancelledError:
        pass
    await limiter.stop()
    await close_all_clients()
    await close_db()

    _log.info("shutdown complete")


def _make_model_list_response(model_names: list[str]) -> list[dict]:
    created = int(time.time())
    return [
        {"id": name, "object": "model", "created": created, "owned_by": "llm-pico"}
        for name in model_names
    ]


def _rewrite_model_field(body_bytes: bytes, new_model: str) -> bytes:
    """Replace the 'model' field in a JSON body with the provider's actual model string."""
    obj = json.loads(body_bytes)
    obj["model"] = new_model
    return json.dumps(obj, ensure_ascii=False).encode()


async def _proxy_request(
    body_bytes: bytes,
    model_name: str,
    stream: bool,
    max_tokens: int,
    user_key: dict[str, Any] | None,
    master_key: str | None,
    app_state: dict[str, Any],
    route_type: str = "chat",
    _is_failover: bool = False,
):
    request_id = os.urandom(8).hex()
    await _track_request(request_id)

    try:
        config: Config = getattr(app_state, "config")
        router: Router = getattr(app_state, "router")
        limiter = getattr(app_state, "limiter")

        num_retries = config.router_settings.num_retries
        prompt_tokens = max(1, len(body_bytes) // 4)
        reservation = prompt_tokens + max_tokens

        # Pre-compute limits (same across all retries)
        limits = {
            "_level": "user",
            "rpm": user_key.get("rpm_limit") if user_key else None,
            "rpd": user_key.get("rpd_limit") if user_key else None,
            "tpm": user_key.get("tpm_limit") if user_key else None,
            "tpd": user_key.get("tpd_limit") if user_key else None,
        } if user_key else {}

        model_limits = { "_level": "model" }
        # model_limits are populated per-retry from the resolved ModelEntry
        # model-level rate limits are enforced per-cell below

        last_error: HTTPException | None = None

        for attempt in range(num_retries + 1):
            result = router.resolve(model_name)
            if result is None:
                if last_error:
                    raise last_error
                raise HTTPException(status_code=404, detail={
                    "error": {"message": f"Model '{model_name}' not available", "type": "model_not_found", "code": 404}
                })

            provider_group, key_state, model_entry = result
            slug = provider_group.provider_slug
            adapter_cls = get_adapter(slug) or OpenAIAdapter

            # ---- capability checks (same outcome every attempt, but need model_entry) ----
            if route_type == "embeddings" and not model_entry.embeddings:
                raise HTTPException(status_code=400, detail={
                    "error": {"message": f"Model '{model_name}' does not support embeddings", "type": "bad_request", "code": 400}
                })
            if route_type == "stt" and not model_entry.stt:
                raise HTTPException(status_code=400, detail={
                    "error": {"message": f"Model '{model_name}' does not support speech-to-text", "type": "bad_request", "code": 400}
                })
            if route_type == "tts" and not model_entry.tts:
                raise HTTPException(status_code=400, detail={
                    "error": {"message": f"Model '{model_name}' does not support text-to-speech", "type": "bad_request", "code": 400}
                })
            if route_type == "embeddings" and not adapter_cls.supports_embeddings:
                raise HTTPException(status_code=501, detail={
                    "error": {"message": f"Adapter '{slug}' does not support embeddings yet", "type": "not_implemented", "code": 501}
                })

            adapter = adapter_cls(provider_slug=slug, api_key=key_state.api_key, api_base=provider_group.api_base)

            # Image check
            if route_type == "chat" and adapter.has_image_input(body_bytes):
                if not model_entry.images:
                    await adapter.close()
                    raise HTTPException(status_code=400, detail={
                        "error": {"message": f"Model '{model_name}' does not support image inputs", "type": "bad_request", "code": 400}
                    })
                if not adapter_cls.supports_images:
                    await adapter.close()
                    raise HTTPException(status_code=501, detail={
                        "error": {"message": f"Adapter '{slug}' does not support image inputs yet", "type": "not_implemented", "code": 501}
                    })

            # ---- Cache check (only on first attempt, non-streaming) ----
            if attempt == 0 and model_entry.can_cache and not stream:
                cached = await get_cached(body_bytes)
                if cached:
                    resp_body, content_type = cached
                    _log.debug("cache hit for %s", model_name)
                    return Response(content=resp_body, media_type=content_type)

            # ---- Budget check (user-level, only on first attempt) ----
            if attempt == 0 and user_key and user_key.get("user_id"):
                est_cost = compute_cost(
                    prompt_tokens, max_tokens,
                    model_entry.cost_per_1m_input, model_entry.cost_per_1m_output,
                )
                budget_err = await check_user_budget(user_key["user_id"], est_cost)
                if budget_err:
                    raise HTTPException(status_code=429, detail={
                        "error": {
                            "message": budget_err,
                            "type": "budget_exceeded",
                            "code": 429,
                        }
                    })

            # ---- Rate limit reservation (only on first attempt) ----
            rl_headers: dict[str, str] = {}
            if attempt == 0:
                model_limits = {
                    "_level": "model",
                    "rpm": model_entry.rpm,
                    "rpd": model_entry.rpd,
                    "tpm": model_entry.tpm,
                    "tpd": model_entry.tpd,
                }
                for l in (limits, model_limits):
                    has_any = any(v is not None for k, v in l.items() if k != "_level")
                    if has_any:
                        rejected = await limiter.check_and_reserve(
                            key_hash=user_key["key_hash"] if user_key else master_key or "admin",
                            model_name=model_name,
                            limits=l,
                            reservation=reservation,
                        )
                        if rejected:
                            await adapter.close()
                            raise HTTPException(status_code=429, detail={
                                "error": {
                                    "message": f"Rate limit exceeded: {rejected['exceeded']}",
                                    "type": "rate_limit_exceeded",
                                    "code": 429,
                                    "retry_after": rejected["retry_after"],
                                }
                            })
                rl_headers = await _build_rate_limit_headers(
                    limiter,
                    key_hash=user_key["key_hash"] if user_key else master_key or "admin",
                    model_name=model_name,
                    user_limits=limits,
                    model_limits=model_limits,
                )

            adapter_model = model_entry.model_params.model
            # Strip provider prefix (e.g. "openai/gpt-4" → "gpt-4")
            model_for_api = adapter_model.split("/", 1)[1] if "/" in adapter_model else adapter_model
            rewritten_body = _rewrite_model_field(body_bytes, model_for_api)

            try:
                cin = model_entry.cost_per_1m_input
                cout = model_entry.cost_per_1m_output
                if stream:
                    response = await _handle_streaming(
                        adapter=adapter,
                        body_bytes=rewritten_body,
                        model_string=model_for_api,
                        user_key=user_key,
                        master_key=master_key or "",
                        model_name=model_name,
                        provider_slug=slug,
                        reservation=reservation,
                        limiter=limiter,
                        limits=limits,
                        request_id=request_id,
                        cost_in=cin, cost_out=cout,
                        rate_limit_headers=rl_headers,
                    )
                else:
                    response = await _handle_buffered(
                        adapter=adapter,
                        body_bytes=rewritten_body,
                        model_string=model_for_api,
                        user_key=user_key,
                        master_key=master_key or "",
                        model_name=model_name,
                        provider_slug=slug,
                        reservation=reservation,
                        limiter=limiter,
                        limits=limits,
                        request_id=request_id,
                        cost_in=cin, cost_out=cout,
                        rate_limit_headers=rl_headers,
                        route_type=route_type,
                    )

                router.record_success(provider_group)

                if model_entry.can_cache and not stream:
                    resp_body = response.body if hasattr(response, 'body') else None
                    if resp_body:
                        await set_cached(body_bytes, resp_body)

                return response

            except HTTPException as e:
                if e.status_code in (400, 401, 403, 404, 501):
                    raise
                router.record_failure(provider_group, key_state, e.status_code)
                last_error = e
            except httpx.HTTPError as e:
                router.record_failure(provider_group, key_state, 502)
                last_error = HTTPException(status_code=502, detail={
                    "error": {"message": f"Upstream request failed: {e}", "type": "upstream_error", "code": 502}
                })
            finally:
                await adapter.close()

        # All retries exhausted — try failover model (one level, no chain)
        if not _is_failover and model_entry and model_entry.failover_model:
            return await _proxy_request(
                body_bytes=body_bytes,
                model_name=model_entry.failover_model,
                stream=stream,
                max_tokens=max_tokens,
                user_key=user_key,
                master_key=master_key,
                app_state=app_state,
                route_type=route_type,
                _is_failover=True,
            )
        if last_error:
            raise last_error
        raise HTTPException(status_code=502, detail={
            "error": {"message": "All upstream providers failed", "type": "upstream_error", "code": 502}
        })

    finally:
        await _untrack_request(request_id)


async def _handle_streaming(
    adapter: OpenAIAdapter,
    body_bytes: bytes,
    model_string: str,
    user_key: dict[str, Any] | None,
    master_key: str,
    model_name: str,
    provider_slug: str,
    reservation: int,
    limiter: Any,
    limits: dict[str, Any],
    request_id: str,
    cost_in: float | None = None,
    cost_out: float | None = None,
    rate_limit_headers: dict[str, str] | None = None,
) -> StreamingResponse:
    t0 = time.monotonic()

    try:
        upstream = await adapter.proxy_request(body_bytes, model_string)
    except httpx.ConnectError as e:
        raise HTTPException(status_code=502, detail={
            "error": {"message": f"Upstream connection failed: {e}", "type": "upstream_error", "code": 502}
        })
    except httpx.TimeoutException as e:
        raise HTTPException(status_code=504, detail={
            "error": {"message": f"Upstream timeout: {e}", "type": "upstream_timeout", "code": 504}
        })

    if upstream.status_code >= 400:
        body = await upstream.aread()
        raise HTTPException(status_code=upstream.status_code, detail=json.loads(body or b"{}"))

    actual_tokens = 0

    has_custom_stream = type(adapter).proxy_stream is not BaseAdapter.proxy_stream

    if has_custom_stream:
        stream_chunks, stream_usage = await adapter.proxy_stream(upstream)

        async def generate():
            nonlocal actual_tokens
            for chunk in stream_chunks:
                if b"usage" in chunk:
                    try:
                        text = chunk.decode("utf-8", errors="replace")
                        for line in text.split("\n"):
                            if line.startswith("data: ") and "[DONE]" not in line:
                                data = json.loads(line[6:])
                                if "usage" in data:
                                    actual_tokens = data["usage"].get("total_tokens", 0)
                    except (json.JSONDecodeError, UnicodeDecodeError):
                        pass
                yield chunk

        if stream_usage:
            actual_tokens = stream_usage.get("total_tokens", 0)
    else:
        async def generate():
            nonlocal actual_tokens
            async for chunk in upstream.aiter_bytes():
                if b"usage" in chunk:
                    try:
                        text = chunk.decode("utf-8", errors="replace")
                        for line in text.split("\n"):
                            if line.startswith("data: ") and "[DONE]" not in line:
                                data = json.loads(line[6:])
                                if "usage" in data:
                                    actual_tokens = data["usage"].get("total_tokens", 0)
                    except (json.JSONDecodeError, UnicodeDecodeError):
                        pass
                yield chunk

    response = StreamingResponse(generate(), media_type="text/event-stream")
    response.headers["X-Request-Id"] = request_id
    if rate_limit_headers:
        for k, v in rate_limit_headers.items():
            response.headers[k] = v

    async def _log_and_reconcile():
        try:
            nonlocal actual_tokens
            while True:
                try:
                    await asyncio.sleep(0.1)
                    if actual_tokens > 0 or response.headers.get("x-llm-pico-done"):
                        break
                except (asyncio.CancelledError, GeneratorExit):
                    break

            latency = int((time.monotonic() - t0) * 1000)

            if actual_tokens == 0:
                actual_tokens = reservation

            pt = 0
            ct = 0
            cost = compute_cost(pt, ct, cost_in, cost_out)
            if cost is None and actual_tokens:
                cost = compute_cost(actual_tokens, 0, cost_in, cost_out)

            await log_usage(
                key_hash=user_key["key_hash"] if user_key else master_key,
                key_prefix=user_key["key_prefix"] if user_key else "master",
                model_name=model_name,
                provider=provider_slug,
                prompt_tokens=pt,
                completion_tokens=ct,
                total_tokens=actual_tokens,
                latency_ms=latency,
                status_code=200,
                cost_usd=cost,
            )

            emit({
                "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
                "key_prefix": user_key["key_prefix"] if user_key else "master",
                "model": model_name,
                "provider": provider_slug,
                "prompt_tokens": pt,
                "completion_tokens": ct,
                "total_tokens": actual_tokens,
                "latency_ms": latency,
                "status": 200,
                "cost_usd": cost,
            })

            if user_key:
                await limiter.reconcile(
                    key_hash=user_key["key_hash"],
                    model_name=model_name,
                    limits=limits,
                    actual_tokens=actual_tokens,
                    reserved_tokens=reservation,
                )
        except Exception:
            _log.exception("background logging/reconciliation failed")

    _log.debug("streaming request %s: model=%s provider=%s", request_id, model_name, provider_slug)
    asyncio.create_task(_log_and_reconcile())
    return response


async def _handle_buffered(
    adapter: OpenAIAdapter,
    body_bytes: bytes,
    model_string: str,
    user_key: dict[str, Any] | None,
    master_key: str,
    model_name: str,
    provider_slug: str,
    reservation: int,
    limiter: Any,
    limits: dict[str, Any],
    request_id: str,
    cost_in: float | None = None,
    cost_out: float | None = None,
    rate_limit_headers: dict[str, str] | None = None,
    route_type: str = "chat",
) -> Response:
    t0 = time.monotonic()

    try:
        if route_type == "embeddings":
            upstream = await adapter.proxy_embeddings(body_bytes)
        else:
            upstream = await adapter.proxy_request(body_bytes, model_string)
    except httpx.ConnectError as e:
        raise HTTPException(status_code=502, detail={
            "error": {"message": f"Upstream connection failed: {e}", "type": "upstream_error", "code": 502}
        })
    except httpx.TimeoutException as e:
        raise HTTPException(status_code=504, detail={
            "error": {"message": f"Upstream timeout: {e}", "type": "upstream_timeout", "code": 504}
        })

    body = await upstream.aread()
    latency = int((time.monotonic() - t0) * 1000)

    if upstream.status_code >= 400:
        raise HTTPException(status_code=upstream.status_code, detail=json.loads(body or b"{}"))

    try:
        resp_data = json.loads(body)
        usage_data = resp_data.get("usage", {})
        pt = usage_data.get("prompt_tokens", 0)
        ct = usage_data.get("completion_tokens", 0)
        actual_tokens = usage_data.get("total_tokens", reservation)
    except (json.JSONDecodeError, KeyError):
        pt, ct, actual_tokens = 0, 0, reservation

    cost = compute_cost(pt, ct, cost_in, cost_out)

    await log_usage(
        key_hash=user_key["key_hash"] if user_key else master_key,
        key_prefix=user_key["key_prefix"] if user_key else "master",
        model_name=model_name,
        provider=provider_slug,
        prompt_tokens=pt,
        completion_tokens=ct,
        total_tokens=actual_tokens,
        latency_ms=latency,
        status_code=upstream.status_code,
        cost_usd=cost,
    )

    emit({
        "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
        "key_prefix": user_key["key_prefix"] if user_key else "master",
        "model": model_name,
        "provider": provider_slug,
        "prompt_tokens": pt,
        "completion_tokens": ct,
        "total_tokens": actual_tokens,
        "latency_ms": latency,
        "status": upstream.status_code,
        "cost_usd": cost,
    })

    if user_key:
        await limiter.reconcile(
            key_hash=user_key["key_hash"],
            model_name=model_name,
            limits=limits,
            actual_tokens=actual_tokens,
            reserved_tokens=reservation,
        )

    _log.debug("buffered request %s: model=%s provider=%s tokens=%d latency=%dms",
               request_id, model_name, provider_slug, actual_tokens, latency)

    resp = Response(content=body, media_type="application/json", status_code=upstream.status_code)
    resp.headers["X-Request-Id"] = request_id
    if rate_limit_headers:
        for k, v in rate_limit_headers.items():
            resp.headers[k] = v
    return resp


async def _route_chat_completions(request: Request, user_key: dict = Depends(require_api_key)) -> Response:
    if _is_draining:
        return Response(
            content=json.dumps({"error": {"message": "Server is shutting down", "type": "draining", "code": 503}}),
            status_code=503,
            media_type="application/json",
            headers={"Retry-After": "30"},
        )

    app_state = request.app.state
    config: Config = getattr(app_state, "config")
    master_key = config.general_settings.master_key

    body_bytes = await request.body()
    try:
        # Peek only 3 fields from raw JSON body
        peek = json.loads(body_bytes)
        model_name = peek.get("model", "")
        stream = peek.get("stream", False)
        max_tokens = peek.get("max_tokens", 4096) or 4096
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    if not model_name:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'model' is required", "type": "bad_request", "code": 400}
        })

    if user_key.get("role") == "user":
        if not check_model_access(user_key, model_name):
            raise HTTPException(status_code=403, detail={
                "error": {"message": f"Model '{model_name}' not allowed for this key", "type": "forbidden", "code": 403}
            })

    return await _proxy_request(
        body_bytes=body_bytes,
        model_name=model_name,
        stream=stream,
        max_tokens=max_tokens,
        user_key=user_key if user_key.get("role") == "user" else None,
        master_key=master_key,
        app_state=app_state,
    )


async def _route_models(request: Request, user_key: dict = Depends(require_api_key)) -> Response:
    app_state = request.app.state
    router: Router = getattr(app_state, "router")

    all_models = router.get_model_names()

    if user_key.get("role") == "user" and user_key.get("model_allowlist"):
        allowed = user_key["model_allowlist"]
        all_models = [m for m in all_models if m in allowed]

    models = _make_model_list_response(all_models)
    return Response(
        content=json.dumps({"object": "list", "data": models}),
        media_type="application/json",
    )


async def _route_single_model(request: Request, model_id: str, user_key: dict = Depends(require_api_key)) -> Response:
    app_state = request.app.state
    router: Router = getattr(app_state, "router")

    if model_id not in router.get_model_names():
        raise HTTPException(status_code=404, detail={
            "error": {"message": f"Model '{model_id}' not found", "type": "model_not_found", "code": 404}
        })

    if user_key.get("role") == "user" and user_key.get("model_allowlist"):
        if model_id not in user_key["model_allowlist"]:
            raise HTTPException(status_code=403, detail={
                "error": {"message": f"Model '{model_id}' not allowed", "type": "forbidden", "code": 403}
            })

    model_obj = _make_model_list_response([model_id])[0]
    return Response(
        content=json.dumps(model_obj),
        media_type="application/json",
    )


async def _health_check() -> Response:
    return Response(
        content=json.dumps({"status": "ok", "version": "0.1.0"}),
        media_type="application/json",
    )


async def _route_completions(request: Request, user_key: dict = Depends(require_api_key)) -> Response:
    return await _route_chat_completions(request)


async def _proxy_audio_request(
    audio_bytes: bytes,
    filename: str,
    content_type: str,
    model_name: str,
    extra_data: dict[str, str] | None,
    user_key: dict[str, Any] | None,
    master_key: str | None,
    app_state: dict[str, Any],
    _is_failover: bool = False,
) -> Response:
    request_id = os.urandom(8).hex()
    await _track_request(request_id)

    try:
        config: Config = getattr(app_state, "config")
        router: Router = getattr(app_state, "router")
        limiter = getattr(app_state, "limiter")
        num_retries = config.router_settings.num_retries

        limits = {
            "_level": "user",
            "rpm": user_key.get("rpm_limit") if user_key else None,
            "rpd": user_key.get("rpd_limit") if user_key else None,
            "tpm": user_key.get("tpm_limit") if user_key else None,
            "tpd": user_key.get("tpd_limit") if user_key else None,
            "ash": user_key.get("ash_limit") if user_key else None,
            "asd": user_key.get("asd_limit") if user_key else None,
        } if user_key else {}

        last_error: HTTPException | None = None

        for attempt in range(num_retries + 1):
            result = router.resolve(model_name)
            if result is None:
                if last_error:
                    raise last_error
                raise HTTPException(status_code=404, detail={
                    "error": {"message": f"Model '{model_name}' not available", "type": "model_not_found", "code": 404}
                })

            provider_group, key_state, model_entry = result
            slug = provider_group.provider_slug
            adapter_cls = get_adapter(slug) or OpenAIAdapter

            if not model_entry.stt:
                raise HTTPException(status_code=400, detail={
                    "error": {"message": f"Model '{model_name}' does not support speech-to-text", "type": "bad_request", "code": 400}
                })

            adapter = adapter_cls(provider_slug=slug, api_key=key_state.api_key, api_base=provider_group.api_base)

            if attempt == 0 and user_key and user_key.get("user_id"):
                est_cost = compute_cost(0, 0, model_entry.cost_per_1m_input, model_entry.cost_per_1m_output)
                budget_err = await check_user_budget(user_key["user_id"], est_cost)
                if budget_err:
                    raise HTTPException(status_code=429, detail={
                        "error": {"message": budget_err, "type": "budget_exceeded", "code": 429}
                    })

            if attempt == 0:
                model_limits = {
                    "_level": "model",
                    "rpm": model_entry.rpm,
                    "rpd": model_entry.rpd,
                    "tpm": model_entry.tpm,
                    "tpd": model_entry.tpd,
                    "ash": model_entry.ash,
                    "asd": model_entry.asd,
                }
                rl_headers: dict[str, str] = {}
                for l in (limits, model_limits):
                    has_any = any(v is not None for k, v in l.items() if k != "_level")
                    if has_any:
                        rejected = await limiter.check_and_reserve(
                            key_hash=user_key["key_hash"] if user_key else master_key or "admin",
                            model_name=model_name,
                            limits=l,
                            reservation=1,
                        )
                        if rejected:
                            await adapter.close()
                            raise HTTPException(status_code=429, detail={
                                "error": {
                                    "message": f"Rate limit exceeded: {rejected['exceeded']}",
                                    "type": "rate_limit_exceeded",
                                    "code": 429,
                                    "retry_after": rejected["retry_after"],
                                }
                            })
                rl_headers = await _build_rate_limit_headers(
                    limiter,
                    key_hash=user_key["key_hash"] if user_key else master_key or "admin",
                    model_name=model_name,
                    user_limits=limits,
                    model_limits=model_limits,
                )

            adapter_model = model_entry.model_params.model
            t0 = time.monotonic()

            try:
                upstream = await adapter.proxy_audio_transcriptions(
                    audio_bytes=audio_bytes,
                    filename=filename,
                    content_type=content_type,
                    model_string=adapter_model,
                    extra_data=extra_data,
                )
            except httpx.ConnectError as e:
                router.record_failure(provider_group, key_state, 502)
                last_error = HTTPException(status_code=502, detail={
                    "error": {"message": f"Upstream connection failed: {e}", "type": "upstream_error", "code": 502}
                })
                continue
            except httpx.TimeoutException as e:
                router.record_failure(provider_group, key_state, 504)
                last_error = HTTPException(status_code=504, detail={
                    "error": {"message": f"Upstream timeout: {e}", "type": "upstream_timeout", "code": 504}
                })
                continue
            except httpx.HTTPError as e:
                router.record_failure(provider_group, key_state, 502)
                last_error = HTTPException(status_code=502, detail={
                    "error": {"message": f"Upstream request failed: {e}", "type": "upstream_error", "code": 502}
                })
                continue

            body = await upstream.aread()
            latency = int((time.monotonic() - t0) * 1000)

            if upstream.status_code >= 400:
                try:
                    detail = json.loads(body)
                except (json.JSONDecodeError, TypeError):
                    detail = {"error": {"message": body.decode(errors="replace"), "code": upstream.status_code}}
                cost = compute_cost(0, 0, model_entry.cost_per_1m_input, model_entry.cost_per_1m_output)
                await log_usage(
                    key_hash=user_key["key_hash"] if user_key else master_key or "",
                    key_prefix=user_key["key_prefix"] if user_key else "master",
                    model_name=model_name, provider=slug,
                    prompt_tokens=0, completion_tokens=0, total_tokens=0,
                    latency_ms=latency, status_code=upstream.status_code,
                    error=str(detail), cost_usd=cost,
                )
                emit({
                    "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
                    "key_prefix": user_key["key_prefix"] if user_key else "master",
                    "model": model_name, "provider": slug,
                    "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
                    "latency_ms": latency, "status": upstream.status_code,
                    "cost_usd": cost,
                })
                if upstream.status_code in (400, 401, 403, 404, 501):
                    raise HTTPException(status_code=upstream.status_code, detail=detail)
                router.record_failure(provider_group, key_state, upstream.status_code)
                last_error = HTTPException(status_code=upstream.status_code, detail=detail)
                continue

            router.record_success(provider_group)
            cost = compute_cost(0, 0, model_entry.cost_per_1m_input, model_entry.cost_per_1m_output)
            await log_usage(
                key_hash=user_key["key_hash"] if user_key else master_key or "",
                key_prefix=user_key["key_prefix"] if user_key else "master",
                model_name=model_name, provider=slug,
                prompt_tokens=0, completion_tokens=0, total_tokens=0,
                latency_ms=latency, status_code=upstream.status_code,
                cost_usd=cost,
            )
            emit({
                "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
                "key_prefix": user_key["key_prefix"] if user_key else "master",
                "model": model_name, "provider": slug,
                "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
                "latency_ms": latency, "status": upstream.status_code,
                "cost_usd": cost,
            })

            resp = Response(content=body, media_type="application/json")
            resp.headers["X-Request-Id"] = request_id
            for k, v in rl_headers.items():
                resp.headers[k] = v
            return resp

        # All retries exhausted — try failover model (one level, no chain)
        if not _is_failover and model_entry and model_entry.failover_model:
            return await _proxy_audio_request(
                audio_bytes=audio_bytes,
                filename=filename,
                content_type=content_type,
                model_name=model_entry.failover_model,
                extra_data=extra_data,
                user_key=user_key,
                master_key=master_key,
                app_state=app_state,
                _is_failover=True,
            )
        raise last_error or HTTPException(status_code=502, detail={
            "error": {"message": "All upstream providers failed", "type": "upstream_error", "code": 502}
        })

    finally:
        await _untrack_request(request_id)


async def _route_audio_transcriptions(request: Request, user_key: dict = Depends(require_api_key)) -> Response:
    if _is_draining:
        return Response(
            content=json.dumps({"error": {"message": "Server is shutting down", "type": "draining", "code": 503}}),
            status_code=503, media_type="application/json", headers={"Retry-After": "30"},
        )

    app_state = request.app.state
    config: Config = getattr(app_state, "config")
    master_key = config.general_settings.master_key

    form = await request.form()
    audio_file: UploadFile | None = form.get("file")
    model_name = form.get("model", "")

    if not audio_file or not audio_file.filename:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'file' is required", "type": "bad_request", "code": 400}
        })
    if not model_name:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'model' is required", "type": "bad_request", "code": 400}
        })

    if user_key.get("role") == "user":
        if not check_model_access(user_key, model_name):
            raise HTTPException(status_code=403, detail={
                "error": {"message": f"Model '{model_name}' not allowed for this key", "type": "forbidden", "code": 403}
            })

    audio_bytes = await audio_file.read()
    content_type = audio_file.content_type or "audio/mpeg"

    extra_data = {}
    for key in ("response_format", "language", "temperature", "timestamp_granularities"):
        val = form.get(key)
        if val is not None:
            extra_data[key] = val

    return await _proxy_audio_request(
        audio_bytes=audio_bytes,
        filename=audio_file.filename,
        content_type=content_type,
        model_name=model_name,
        extra_data=extra_data or None,
        user_key=user_key if user_key.get("role") == "user" else None,
        master_key=master_key,
        app_state=app_state,
    )


async def _proxy_audio_speech(
    body_bytes: bytes,
    model_name: str,
    user_key: dict[str, Any] | None,
    master_key: str | None,
    app_state: dict[str, Any],
    _is_failover: bool = False,
) -> Response:
    request_id = os.urandom(8).hex()
    await _track_request(request_id)

    try:
        config: Config = getattr(app_state, "config")
        router: Router = getattr(app_state, "router")
        num_retries = config.router_settings.num_retries

        last_error: HTTPException | None = None

        for attempt in range(num_retries + 1):
            result = router.resolve(model_name)
            if result is None:
                if last_error:
                    raise last_error
                raise HTTPException(status_code=404, detail={
                    "error": {"message": f"Model '{model_name}' not available", "type": "model_not_found", "code": 404}
                })

            _, key_state, model_entry = result
            slug = result[0].provider_slug
            adapter_cls = get_adapter(slug) or OpenAIAdapter

            if not model_entry.tts:
                raise HTTPException(status_code=400, detail={
                    "error": {"message": f"Model '{model_name}' does not support text-to-speech", "type": "bad_request", "code": 400}
                })

            if attempt == 0 and user_key and user_key.get("user_id"):
                est_cost = compute_cost(0, 0, model_entry.cost_per_1m_input, model_entry.cost_per_1m_output)
                budget_err = await check_user_budget(user_key["user_id"], est_cost)
                if budget_err:
                    raise HTTPException(status_code=429, detail={
                        "error": {"message": budget_err, "type": "budget_exceeded", "code": 429}
                    })

            adapter = adapter_cls(provider_slug=slug, api_key=key_state.api_key, api_base=result[0].api_base)
            adapter_model = model_entry.model_params.model
            model_for_api = adapter_model.split("/", 1)[1] if "/" in adapter_model else adapter_model
            rewritten_body = _rewrite_model_field(body_bytes, model_for_api)
            t0 = time.monotonic()

            try:
                upstream = await adapter.proxy_tts(
                    body_bytes=rewritten_body,
                    model_string=model_for_api,
                )
            except httpx.ConnectError as e:
                router.record_failure(result[0], key_state, 502)
                last_error = HTTPException(status_code=502, detail={
                    "error": {"message": f"Upstream connection failed: {e}", "type": "upstream_error", "code": 502}
                })
                continue
            except httpx.TimeoutException as e:
                router.record_failure(result[0], key_state, 504)
                last_error = HTTPException(status_code=504, detail={
                    "error": {"message": f"Upstream timeout: {e}", "type": "upstream_timeout", "code": 504}
                })
                continue
            except httpx.HTTPError as e:
                router.record_failure(result[0], key_state, 502)
                last_error = HTTPException(status_code=502, detail={
                    "error": {"message": f"Upstream request failed: {e}", "type": "upstream_error", "code": 502}
                })
                continue

            body = await upstream.aread()
            latency = int((time.monotonic() - t0) * 1000)

            if upstream.status_code >= 400:
                try:
                    detail = json.loads(body)
                except (json.JSONDecodeError, TypeError):
                    detail = {"error": {"message": body.decode(errors="replace"), "code": upstream.status_code}}
                cost = compute_cost(0, 0, model_entry.cost_per_1m_input, model_entry.cost_per_1m_output)
                await log_usage(
                    key_hash=user_key["key_hash"] if user_key else master_key or "",
                    key_prefix=user_key["key_prefix"] if user_key else "master",
                    model_name=model_name, provider=slug,
                    prompt_tokens=0, completion_tokens=0, total_tokens=0,
                    latency_ms=latency, status_code=upstream.status_code,
                    error=str(detail), cost_usd=cost,
                )
                emit({
                    "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
                    "key_prefix": user_key["key_prefix"] if user_key else "master",
                    "model": model_name, "provider": slug,
                    "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
                    "latency_ms": latency, "status": upstream.status_code,
                    "cost_usd": cost,
                })
                if upstream.status_code in (400, 401, 403, 404, 501):
                    raise HTTPException(status_code=upstream.status_code, detail=detail)
                router.record_failure(result[0], key_state, upstream.status_code)
                last_error = HTTPException(status_code=upstream.status_code, detail=detail)
                continue

            router.record_success(result[0])
            cost = compute_cost(0, 0, model_entry.cost_per_1m_input, model_entry.cost_per_1m_output)
            await log_usage(
                key_hash=user_key["key_hash"] if user_key else master_key or "",
                key_prefix=user_key["key_prefix"] if user_key else "master",
                model_name=model_name, provider=slug,
                prompt_tokens=0, completion_tokens=0, total_tokens=0,
                latency_ms=latency, status_code=upstream.status_code,
                cost_usd=cost,
            )
            emit({
                "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()),
                "key_prefix": user_key["key_prefix"] if user_key else "master",
                "model": model_name, "provider": slug,
                "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
                "latency_ms": latency, "status": upstream.status_code,
                "cost_usd": cost,
            })

            return Response(content=body, media_type=upstream.headers.get("content-type", "audio/mpeg"))

        if not _is_failover and model_entry and model_entry.failover_model:
            return await _proxy_audio_speech(
                body_bytes=body_bytes,
                model_name=model_entry.failover_model,
                user_key=user_key,
                master_key=master_key,
                app_state=app_state,
                _is_failover=True,
            )
        raise last_error or HTTPException(status_code=502, detail={
            "error": {"message": "All upstream providers failed", "type": "upstream_error", "code": 502}
        })

    finally:
        await _untrack_request(request_id)


async def _route_audio_speech(request: Request, user_key: dict = Depends(require_api_key)) -> Response:
    if _is_draining:
        return Response(
            content=json.dumps({"error": {"message": "Server is shutting down", "type": "draining", "code": 503}}),
            status_code=503, media_type="application/json", headers={"Retry-After": "30"},
        )

    app_state = request.app.state
    config: Config = getattr(app_state, "config")
    master_key = config.general_settings.master_key

    body_bytes = await request.body()
    try:
        peek = json.loads(body_bytes)
        model_name = peek.get("model", "")
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    if not model_name:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'model' is required", "type": "bad_request", "code": 400}
        })

    if user_key.get("role") == "user":
        if not check_model_access(user_key, model_name):
            raise HTTPException(status_code=403, detail={
                "error": {"message": f"Model '{model_name}' not allowed for this key", "type": "forbidden", "code": 403}
            })

    return await _proxy_audio_speech(
        body_bytes=body_bytes,
        model_name=model_name,
        user_key=user_key if user_key.get("role") == "user" else None,
        master_key=master_key,
        app_state=app_state,
    )


async def _route_embeddings(request: Request, user_key: dict = Depends(require_api_key)) -> Response:
    app_state = request.app.state
    config: Config = getattr(app_state, "config")
    master_key = config.general_settings.master_key

    body_bytes = await request.body()
    try:
        peek = json.loads(body_bytes)
        model_name = peek.get("model", "")
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Invalid JSON body", "type": "bad_request", "code": 400}
        })

    if not model_name:
        raise HTTPException(status_code=400, detail={
            "error": {"message": "Field 'model' is required", "type": "bad_request", "code": 400}
        })

    if user_key.get("role") == "user":
        if not check_model_access(user_key, model_name):
            raise HTTPException(status_code=403, detail={
                "error": {"message": f"Model '{model_name}' not allowed for this key", "type": "forbidden", "code": 403}
            })

    return await _proxy_request(
        body_bytes=body_bytes,
        model_name=model_name,
        stream=False,
        max_tokens=0,
        user_key=user_key if user_key.get("role") == "user" else None,
        master_key=master_key,
        app_state=app_state,
        route_type="embeddings",
    )


def create_app(app_state: dict[str, Any]) -> FastAPI:
    app = FastAPI(
        title="llm-pico",
        version="0.1.0",
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
    )
    for k, v in app_state.items():
        setattr(app.state, k, v)

    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_methods=["*"],
        allow_headers=["*"],
    )

    app.add_api_route("/v1/chat/completions", _route_chat_completions, methods=["POST"])
    app.add_api_route("/v1/completions", _route_completions, methods=["POST"])
    app.add_api_route("/v1/embeddings", _route_embeddings, methods=["POST"])
    app.add_api_route("/v1/audio/transcriptions", _route_audio_transcriptions, methods=["POST"])
    app.add_api_route("/v1/audio/speech", _route_audio_speech, methods=["POST"])
    app.add_api_route("/v1/models", _route_models, methods=["GET"])
    app.add_api_route("/v1/models/{model_id}", _route_single_model, methods=["GET"])
    app.add_api_route("/health", _health_check, methods=["GET"])

    app.include_router(admin_router, prefix="/admin")
    app.include_router(website_router, prefix="/admin")
    app.include_router(placeholder_router)

    @app.exception_handler(HTTPException)
    async def http_exception_handler(request: Request, exc: HTTPException):
        return Response(
            content=json.dumps(exc.detail),
            status_code=exc.status_code,
            media_type="application/json",
        )

    @app.exception_handler(Exception)
    async def generic_exception_handler(request: Request, exc: Exception):
        _log.exception("unhandled error handling %s %s", request.method, request.url.path)
        return Response(
            content=json.dumps({
                "error": {"message": "Internal server error", "type": "internal_error", "code": 500}
            }),
            status_code=500,
            media_type="application/json",
        )

    return app
