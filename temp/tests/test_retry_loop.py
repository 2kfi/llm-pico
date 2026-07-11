from __future__ import annotations

import json
from unittest.mock import AsyncMock, patch

import httpx
import pytest
from fastapi import HTTPException
from starlette.responses import StreamingResponse

from core.router import Router, CircuitBreaker, KeyState, ProviderGroup
from core.config import Config, GeneralSettings, LitellmParams, ModelEntry, RouterSettings


class MockAdapter:
    """Mock adapter that returns controlled responses."""
    supports_images = False
    supports_embeddings = False
    supports_stt = False
    supports_tts = False

    def __init__(self, api_key=None, api_base=None):
        self.api_key = api_key or ""
        self.api_base = api_base
        self.close_count = 0

    def has_image_input(self, body):
        return False

    async def close(self):
        self.close_count += 1


class MockRouter:
    def __init__(self, groups):
        self._groups = list(groups)
        self._index = 0
        self.failures = []
        self.successes = []

    def resolve(self, model_name):
        if self._index >= len(self._groups):
            return None
        result = self._groups[self._index]
        self._index = (self._index + 1) % len(self._groups)
        return result

    def record_failure(self, group, key, status_code):
        self.failures.append((group, key, status_code))

    def record_success(self, group):
        self.successes.append(group)


@pytest.mark.asyncio
async def test_retry_on_5xx():
    """Verify retry loop retries on 5xx error and picks next key."""
    key1 = KeyState(api_key="sk-key-1")
    key2 = KeyState(api_key="sk-key-2")
    group = ProviderGroup(
        provider_slug="openai",
        keys=[key1, key2],
        api_base="https://api.openai.com/v1",
    )
    entry = ModelEntry(
        model_name="test-model",
        litellm_params=LitellmParams(model="openai/gpt-4", api_key="sk-key-1"),
    )

    router = MockRouter([(group, key1, entry), (group, key2, entry)])

    # Import and test _proxy_request's logic directly
    from api.server import _proxy_request

    app_state_mock = type('State', (), {
        'config': Config(
            general_settings=GeneralSettings(master_key="mk-test"),
            router_settings=RouterSettings(num_retries=2),
        ),
        'router': router,
        'limiter': type('Limiter', (), {
            'check_and_reserve': AsyncMock(return_value=None),
            'reconcile': AsyncMock(),
        })(),
    })()

    call_count = 0

    async def failing_streaming(*args, **kwargs):
        nonlocal call_count
        call_count += 1
        if call_count < 3:
            raise HTTPException(status_code=502, detail={"error": "upstream failed"})
        return StreamingResponse(
            iter([b'data: {"choices": [{"delta": {"content": "ok"}}]}\n\n', b"data: [DONE]\n\n"]),
            media_type="text/event-stream",
        )

    with patch("api.server._handle_streaming", failing_streaming):
        response = await _proxy_request(
            body_bytes=json.dumps({"model": "test-model", "messages": [{"role": "user", "content": "hi"}]}).encode(),
            model_name="test-model",
            stream=True,
            max_tokens=100,
            user_key=None,
            master_key="mk-test",
            app_state=app_state_mock,
        )
        assert response.status_code == 200

    assert call_count == 3
    assert len(router.successes) == 1


@pytest.mark.asyncio
async def test_retry_exhausted_raises_last_error():
    """Verify all retries exhausted raises last error."""
    key1 = KeyState(api_key="sk-key-1")
    group = ProviderGroup(
        provider_slug="openai",
        keys=[key1],
        api_base="https://api.openai.com/v1",
    )
    entry = ModelEntry(
        model_name="test-model",
        litellm_params=LitellmParams(model="openai/gpt-4", api_key="sk-key-1"),
    )

    router = MockRouter([(group, key1, entry) for _ in range(5)])

    from api.server import _proxy_request
    import api.server as srv

    app_state_mock = type('State', (), {
        'config': Config(
            general_settings=GeneralSettings(master_key="mk-test"),
            router_settings=RouterSettings(num_retries=2),
        ),
        'router': router,
        'limiter': type('Limiter', (), {
            'check_and_reserve': AsyncMock(return_value=None),
            'reconcile': AsyncMock(),
        })(),
    })()

    call_count = 0

    async def always_fail(*args, **kwargs):
        nonlocal call_count
        call_count += 1
        raise HTTPException(status_code=502, detail={"error": "upstream failed"})

    with patch("api.server._handle_streaming", always_fail):
        with pytest.raises(HTTPException) as exc:
            await _proxy_request(
                body_bytes=json.dumps({"model": "test-model", "messages": [{"role": "user", "content": "hi"}]}).encode(),
                model_name="test-model",
                stream=True,
                max_tokens=100,
                user_key=None,
                master_key="mk-test",
                app_state=app_state_mock,
            )

        assert exc.value.status_code == 502

    assert call_count == 3  # initial + 2 retries
    assert len(router.failures) == 3


@pytest.mark.asyncio
async def test_non_retryable_errors_propagate_immediately():
    """Verify 400/401/403/404/501 are NOT retried."""
    key1 = KeyState(api_key="sk-key-1")
    group = ProviderGroup(
        provider_slug="openai",
        keys=[key1],
        api_base="https://api.openai.com/v1",
    )
    entry = ModelEntry(
        model_name="test-model",
        litellm_params=LitellmParams(model="openai/gpt-4", api_key="sk-key-1"),
    )

    router = MockRouter([(group, key1, entry) for _ in range(5)])

    from api.server import _proxy_request

    app_state_mock = type('State', (), {
        'config': Config(
            general_settings=GeneralSettings(master_key="mk-test"),
            router_settings=RouterSettings(num_retries=2),
        ),
        'router': router,
        'limiter': type('Limiter', (), {
            'check_and_reserve': AsyncMock(return_value=None),
            'reconcile': AsyncMock(),
        })(),
    })()

    for status in (400, 401, 403, 404, 501):
        call_count = 0
        expected_detail = {"error": {"message": f"non-retryable-{status}", "code": status}}

        async def fail_with_status(*args, _s=status, **kwargs):
            nonlocal call_count
            call_count += 1
            raise HTTPException(status_code=_s, detail=expected_detail)

        with patch("api.server._handle_streaming", fail_with_status):
            with pytest.raises(HTTPException) as exc:
                await _proxy_request(
                    body_bytes=json.dumps({"model": "test-model", "messages": [{"role": "user", "content": "hi"}]}).encode(),
                    model_name="test-model",
                    stream=True,
                    max_tokens=100,
                    user_key=None,
                    master_key="mk-test",
                    app_state=app_state_mock,
                )

            assert exc.value.status_code == status
            assert call_count == 1, f"status {status} was retried but should not be"


@pytest.mark.asyncio
async def test_failover_model_called_when_retries_exhausted():
    """Verify failover model is called after all retries fail."""
    key1 = KeyState(api_key="sk-key-1")
    group = ProviderGroup(
        provider_slug="openai",
        keys=[key1],
        api_base="https://api.openai.com/v1",
    )
    entry = ModelEntry(
        model_name="test-model",
        litellm_params=LitellmParams(model="openai/gpt-4", api_key="sk-key-1"),
        failover_model="fallback-model",
    )
    fallback_entry = ModelEntry(
        model_name="fallback-model",
        litellm_params=LitellmParams(model="openai/gpt-3.5", api_key="sk-key-1"),
    )
    fallback_group = ProviderGroup(
        provider_slug="openai",
        keys=[KeyState(api_key="sk-fallback-key")],
        api_base="https://api.openai.com/v1",
    )

    resolve_results: list = [(group, key1, entry)] * 10 + [(fallback_group, fallback_group.keys[0], fallback_entry)] * 10
    router = MockRouter(resolve_results)

    from api.server import _proxy_request

    app_state_mock = type('State', (), {
        'config': Config(
            general_settings=GeneralSettings(master_key="mk-test"),
            router_settings=RouterSettings(num_retries=2),
        ),
        'router': router,
        'limiter': type('Limiter', (), {
            'check_and_reserve': AsyncMock(return_value=None),
            'reconcile': AsyncMock(),
        })(),
    })()

    call_count = 0

    async def fail_then_succeed(*args, **kwargs):
        nonlocal call_count
        call_count += 1
        if call_count < 4:
            raise HTTPException(status_code=502, detail={"error": "upstream failed"})
        return StreamingResponse(
            iter([b'data: {"choices": [{"delta": {"content": "fallback ok"}}]}\n\n', b"data: [DONE]\n\n"]),
            media_type="text/event-stream",
        )

    with patch("api.server._handle_streaming", fail_then_succeed):
        response = await _proxy_request(
            body_bytes=json.dumps({"model": "test-model", "messages": [{"role": "user", "content": "hi"}]}).encode(),
            model_name="test-model",
            stream=True,
            max_tokens=100,
            user_key=None,
            master_key="mk-test",
            app_state=app_state_mock,
        )
        assert response.status_code == 200

    assert call_count == 4  # 3 retries on primary + 1 on failover


@pytest.mark.asyncio
async def test_failover_does_not_chain():
    """Verify failover only goes one level deep (no recursive chain)."""
    key1 = KeyState(api_key="sk-key-1")
    group = ProviderGroup(
        provider_slug="openai",
        keys=[key1],
        api_base="https://api.openai.com/v1",
    )
    entry = ModelEntry(
        model_name="test-model",
        litellm_params=LitellmParams(model="openai/gpt-4", api_key="sk-key-1"),
        failover_model="fallback-model",
    )
    fallback_entry = ModelEntry(
        model_name="fallback-model",
        litellm_params=LitellmParams(model="openai/gpt-3.5", api_key="sk-key-1"),
        failover_model="deep-fallback",  # this should NOT be triggered
    )

    resolve_results: list = [(group, key1, entry)] * 10 + [(group, key1, fallback_entry)] * 10
    router = MockRouter(resolve_results)

    from api.server import _proxy_request

    app_state_mock = type('State', (), {
        'config': Config(
            general_settings=GeneralSettings(master_key="mk-test"),
            router_settings=RouterSettings(num_retries=2),
        ),
        'router': router,
        'limiter': type('Limiter', (), {
            'check_and_reserve': AsyncMock(return_value=None),
            'reconcile': AsyncMock(),
        })(),
    })()

    call_count = 0

    async def always_fail(*args, **kwargs):
        nonlocal call_count
        call_count += 1
        raise HTTPException(status_code=502, detail={"error": "upstream failed"})

    with patch("api.server._handle_streaming", always_fail):
        with pytest.raises(HTTPException) as exc:
            await _proxy_request(
                body_bytes=json.dumps({"model": "test-model", "messages": [{"role": "user", "content": "hi"}]}).encode(),
                model_name="test-model",
                stream=True,
                max_tokens=100,
                user_key=None,
                master_key="mk-test",
                app_state=app_state_mock,
            )
        assert exc.value.status_code == 502

    # 3 retries on primary + 3 retries on failover = 6; no chaining to deep-fallback
    assert call_count == 6
