from __future__ import annotations

import json
import logging
from abc import ABC, abstractmethod
from typing import Any

import httpx

_log = logging.getLogger("llm-pico.adapter")


class BaseAdapter(ABC):
    provider: str = ""
    supports_images: bool = False
    supports_embeddings: bool = False
    supports_stt: bool = False
    supports_tts: bool = False

    def __init__(self, api_key: str | None = None, api_base: str | None = None) -> None:
        self.api_key = api_key or ""
        self.api_base = api_base

        pool_limits = httpx.Limits(
            max_connections=5,
            max_keepalive_connections=3,
            keepalive_expiry=30.0,
        )
        pool_timeout = httpx.Timeout(300.0, connect=10.0, pool=10.0)

        headers = {}
        self._set_auth_headers(headers)

        self.client = httpx.AsyncClient(
            limits=pool_limits,
            timeout=pool_timeout,
            headers=headers,
        )

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        headers["Content-Type"] = "application/json"

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
        await self.client.aclose()
