from __future__ import annotations

import json
import logging
from abc import ABC, abstractmethod
from typing import Any

import httpx

_log = logging.getLogger("llm-pico.providers.base")

_shared_clients: dict[str, httpx.AsyncClient] = {}
_shared_clients_lock: Any = None


def _get_lock():
    global _shared_clients_lock
    if _shared_clients_lock is None:
        import asyncio
        try:
            _shared_clients_lock = asyncio.Lock()
        except RuntimeError:
            _shared_clients_lock = None
    return _shared_clients_lock


def _get_client(provider_slug: str) -> httpx.AsyncClient:
    if provider_slug in _shared_clients:
        return _shared_clients[provider_slug]

    limits = httpx.Limits(
        max_connections=10,
        max_keepalive_connections=5,
        keepalive_expiry=15,
    )
    timeout = httpx.Timeout(timeout=300.0, connect=10.0, pool=5.0)

    client = httpx.AsyncClient(limits=limits, timeout=timeout)
    _shared_clients[provider_slug] = client
    _log.debug("created shared httpx client for provider=%s", provider_slug)
    return client


async def close_all_clients() -> None:
    for slug, client in list(_shared_clients.items()):
        try:
            await client.aclose()
        except Exception:
            pass
        _log.debug("closed shared httpx client for provider=%s", slug)
    _shared_clients.clear()


class BaseAdapter(ABC):
    provider: str = ""
    supports_images: bool = False
    supports_embeddings: bool = False
    supports_stt: bool = False
    supports_tts: bool = False

    def __init__(
        self,
        provider_slug: str = "",
        api_key: str | None = None,
        api_base: str | None = None,
    ) -> None:
        self.api_key = api_key or ""
        self.api_base = api_base
        self._provider_slug = provider_slug

        if provider_slug:
            self.client = _get_client(provider_slug)
        else:
            limits = httpx.Limits(
                max_connections=10,
                max_keepalive_connections=5,
                keepalive_expiry=15,
            )
            timeout = httpx.Timeout(timeout=300.0, connect=10.0, pool=5.0)
            self.client = httpx.AsyncClient(limits=limits, timeout=timeout)
            self._owns_client = True

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        headers["Content-Type"] = "application/json"

    def _headers(self) -> dict[str, str]:
        h: dict[str, str] = {}
        self._set_auth_headers(h)
        return h

    def peek_request(self, body: bytes) -> tuple[str, bool, int]:
        obj = json.loads(body)
        return (
            obj.get("model", ""),
            obj.get("stream", False),
            obj.get("max_tokens", 4096) or 4096,
        )

    def has_image_input(self, body: bytes) -> bool:
        try:
            obj = json.loads(body)
            for msg in obj.get("messages", []):
                content = msg.get("content", "")
                if isinstance(content, list):
                    for part in content:
                        if isinstance(part, dict) and part.get("type") == "image_url":
                            return True
            return False
        except Exception:
            return False

    @abstractmethod
    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        ...

    async def proxy_tts(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        raise NotImplementedError("TTS not supported by this adapter")

    async def proxy_stream(
        self, response: httpx.Response
    ) -> tuple[list[bytes], dict[str, Any] | None]:
        chunks: list[bytes] = []
        usage: dict[str, Any] | None = None

        async for chunk in response.aiter_bytes():
            chunks.append(chunk)
            if b"usage" in chunk:
                try:
                    text = chunk.decode("utf-8", errors="replace")
                    for line in text.split("\n"):
                        if line.startswith("data: ") and "[DONE]" not in line:
                            data = json.loads(line[6:])
                            if "usage" in data:
                                usage = data["usage"]
                except (json.JSONDecodeError, UnicodeDecodeError):
                    pass

        return chunks, usage

    async def close(self) -> None:
        pass
