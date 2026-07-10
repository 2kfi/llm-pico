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
from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.middleware.cors import CORSMiddleware
from starlette.responses import StreamingResponse

from .adapters import get_adapter
from .adapters.openai import OpenAIAdapter
from .admin import router as admin_router
from .auth import (
    check_model_access,
    extract_bearer,
    hash_key,
    prefix_from_key,
    seed_users,
    verify_api_key,
)
from .config import Config
from .db import close_db, init_db
from .placeholder import router as placeholder_router
from .ratelimit import get_limiter
from .router import Router
from .usage import log_usage

_log = logging.getLogger("llm-pico.server")

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

    yield

    await limiter.stop()
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
):
    request_id = os.urandom(8).hex()
    await _track_request(request_id)

    try:
        config: Config = getattr(app_state, "config")
        router: Router = getattr(app_state, "router")
        limiter = getattr(app_state, "limiter")

        result = router.resolve(model_name)
        if result is None:
            raise HTTPException(status_code=404, detail={
                "error": {"message": f"Model '{model_name}' not available", "type": "model_not_found", "code": 404}
            })

        provider_group, key_state, model_entry = result

        slug = provider_group.provider_slug
        adapter_cls = get_adapter(slug)

        if adapter_cls is None:
            adapter_cls = OpenAIAdapter
            _log.debug("no adapter for '%s', using OpenAI-compat passthrough", slug)

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

        # Check adapter capabilities
        if route_type == "embeddings" and not adapter_cls.supports_embeddings:
            raise HTTPException(status_code=501, detail={
                "error": {"message": f"Adapter '{slug}' does not support embeddings yet", "type": "not_implemented", "code": 501}
            })

        adapter = adapter_cls(
            api_key=key_state.api_key,
            api_base=provider_group.api_base,
        )

        # Check image input capability for chat completions
        if route_type == "chat" and adapter.has_image_input(body_bytes):
            if not model_entry.images:
                raise HTTPException(status_code=400, detail={
                    "error": {"message": f"Model '{model_name}' does not support image inputs", "type": "bad_request", "code": 400}
                })
            if not adapter_cls.supports_images:
                raise HTTPException(status_code=501, detail={
                    "error": {"message": f"Adapter '{slug}' does not support image inputs yet", "type": "not_implemented", "code": 501}
                })

        # Rewrite the model field to the provider's actual model string
        provider_model = model_entry.litellm_params.model
        body_bytes = _rewrite_model_field(body_bytes, provider_model)

        try:
            adapter_model = model_entry.litellm_params.model
            prompt_tokens = max(1, len(body_bytes) // 4)

            limits = {
                "_level": "user",
                "rpm": user_key.get("rpm_limit") if user_key else None,
                "rpd": user_key.get("rpd_limit") if user_key else None,
                "tpm": user_key.get("tpm_limit") if user_key else None,
                "tpd": user_key.get("tpd_limit") if user_key else None,
            } if user_key else {}

            model_limits = {
                "_level": "model",
                "rpm": model_entry.rpm,
                "rpd": model_entry.rpd,
                "tpm": model_entry.tpm,
                "tpd": model_entry.tpd,
            }

            reservation = prompt_tokens + max_tokens

            for l in (limits, model_limits):
                rejected = None
                has_any = any(v is not None for k, v in l.items() if k != "_level")
                if has_any:
                    rejected = await limiter.check_and_reserve(
                        key_hash=user_key["key_hash"] if user_key else master_key or "admin",
                        model_name=model_name,
                        limits=l,
                        reservation=reservation,
                    )
                if rejected:
                    raise HTTPException(status_code=429, detail={
                        "error": {
                            "message": f"Rate limit exceeded: {rejected['exceeded']}",
                            "type": "rate_limit_exceeded",
                            "code": 429,
                            "retry_after": rejected["retry_after"],
                        }
                    })

            if stream:
                return await _handle_streaming(
                    adapter=adapter,
                    body_bytes=body_bytes,
                    model_string=adapter_model,
                    user_key=user_key,
                    master_key=master_key or "",
                    model_name=model_name,
                    provider_slug=slug,
                    reservation=reservation,
                    limiter=limiter,
                    limits=limits,
                    request_id=request_id,
                )
            else:
                return await _handle_buffered(
                    adapter=adapter,
                    body_bytes=body_bytes,
                    model_string=adapter_model,
                    user_key=user_key,
                    master_key=master_key or "",
                    model_name=model_name,
                    provider_slug=slug,
                    reservation=reservation,
                    limiter=limiter,
                    limits=limits,
                    request_id=request_id,
                )

        finally:
            await adapter.close()

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

    async def _log_and_reconcile():
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

        await log_usage(
            key_hash=user_key["key_hash"] if user_key else master_key,
            key_prefix=user_key["key_prefix"] if user_key else "master",
            model_name=model_name,
            provider=provider_slug,
            prompt_tokens=0,
            completion_tokens=0,
            total_tokens=actual_tokens,
            latency_ms=latency,
            status_code=200,
        )

        if user_key:
            await limiter.reconcile(
                key_hash=user_key["key_hash"],
                model_name=model_name,
                limits=limits,
                actual_tokens=actual_tokens,
                reserved_tokens=reservation,
            )

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
) -> Response:
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

    body = await upstream.aread()
    latency = int((time.monotonic() - t0) * 1000)

    if upstream.status_code >= 400:
        raise HTTPException(status_code=upstream.status_code, detail=json.loads(body or b"{}"))

    try:
        resp_data = json.loads(body)
        usage_data = resp_data.get("usage", {})
        actual_tokens = usage_data.get("total_tokens", reservation)
    except (json.JSONDecodeError, KeyError):
        actual_tokens = reservation

    await log_usage(
        key_hash=user_key["key_hash"] if user_key else master_key,
        key_prefix=user_key["key_prefix"] if user_key else "master",
        model_name=model_name,
        provider=provider_slug,
        prompt_tokens=usage_data.get("prompt_tokens", 0),
        completion_tokens=usage_data.get("completion_tokens", 0),
        total_tokens=actual_tokens,
        latency_ms=latency,
        status_code=upstream.status_code,
    )

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

    return Response(content=body, media_type="application/json", status_code=upstream.status_code)


async def _route_chat_completions(request: Request) -> Response:
    if _is_draining:
        return Response(
            content=json.dumps({"error": {"message": "Server is shutting down", "type": "draining", "code": 503}}),
            status_code=503,
            media_type="application/json",
            headers={"Retry-After": "30"},
        )

    app_state = request.app.state
    config: Config = getattr(app_state, "config")

    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Missing or invalid Authorization header", "type": "unauthorized", "code": 401}
        })

    master_key = config.general_settings.master_key
    user_key = await verify_api_key(raw_key, master_key)
    if user_key is None:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}
        })

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


async def _route_models(request: Request) -> Response:
    app_state = request.app.state
    router: Router = getattr(app_state, "router")
    config: Config = getattr(app_state, "config")

    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Missing or invalid Authorization header", "type": "unauthorized", "code": 401}
        })

    master_key = config.general_settings.master_key
    user_key = await verify_api_key(raw_key, master_key)
    if user_key is None:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}
        })

    all_models = router.get_model_names()

    if user_key.get("role") == "user" and user_key.get("model_allowlist"):
        allowed = user_key["model_allowlist"]
        all_models = [m for m in all_models if m in allowed]

    models = _make_model_list_response(all_models)
    return Response(
        content=json.dumps({"object": "list", "data": models}),
        media_type="application/json",
    )


async def _route_single_model(request: Request, model_id: str) -> Response:
    app_state = request.app.state
    router: Router = getattr(app_state, "router")
    config: Config = getattr(app_state, "config")

    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Missing or invalid Authorization header", "type": "unauthorized", "code": 401}
        })

    master_key = config.general_settings.master_key
    user_key = await verify_api_key(raw_key, master_key)
    if user_key is None:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}
        })

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


async def _route_completions(request: Request) -> Response:
    return await _route_chat_completions(request)


async def _route_embeddings(request: Request) -> Response:
    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Missing or invalid Authorization header", "type": "unauthorized", "code": 401}
        })

    app_state = request.app.state
    config: Config = getattr(app_state, "config")
    master_key = config.general_settings.master_key
    user_key = await verify_api_key(raw_key, master_key)
    if user_key is None:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}
        })

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
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    app.add_api_route("/v1/chat/completions", _route_chat_completions, methods=["POST"])
    app.add_api_route("/v1/completions", _route_completions, methods=["POST"])
    app.add_api_route("/v1/embeddings", _route_embeddings, methods=["POST"])
    app.add_api_route("/v1/models", _route_models, methods=["GET"])
    app.add_api_route("/v1/models/{model_id}", _route_single_model, methods=["GET"])
    app.add_api_route("/health", _health_check, methods=["GET"])

    app.include_router(admin_router, prefix="/admin")
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
