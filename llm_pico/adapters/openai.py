from __future__ import annotations

import logging

import httpx

from .base import BaseAdapter
from . import register

_log = logging.getLogger("llm-pico.adapter.openai")


@register("openai")
class OpenAIAdapter(BaseAdapter):
    provider = "openai"
    supports_images = True
    supports_embeddings = True

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        super()._set_auth_headers(headers)
        headers["Authorization"] = f"Bearer {self.api_key}"

    def _base_url(self) -> str:
        return (self.api_base or "https://api.openai.com/v1").rstrip("/")

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        url = f"{self._base_url()}/chat/completions"
        return await self.client.post(url, content=body_bytes)

    async def proxy_completions(self, body_bytes: bytes) -> httpx.Response:
        url = f"{self._base_url()}/completions"
        return await self.client.post(url, content=body_bytes)

    async def proxy_embeddings(self, body_bytes: bytes) -> httpx.Response:
        url = f"{self._base_url()}/embeddings"
        return await self.client.post(url, content=body_bytes)
