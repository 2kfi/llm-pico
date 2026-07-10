from __future__ import annotations

import json
import logging
from typing import Any

import httpx

from .base import BaseAdapter
from . import register

_log = logging.getLogger("llm-pico.adapter.cloudflare")


@register("cloudflare")
class CloudflareAdapter(BaseAdapter):
    provider = "cloudflare"
    supports_embeddings = True

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        headers["Content-Type"] = "application/json"
        headers["Authorization"] = f"Bearer {self.api_key}"

    def _base_url(self) -> str:
        return (self.api_base or "https://api.cloudflare.com/client/v4/accounts/UNSET/ai/v1").rstrip("/")

    def _strip_prefix(self, model_string: str) -> str:
        if model_string.startswith("cloudflare/"):
            return model_string[len("cloudflare/"):]
        return model_string

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        body = json.loads(body_bytes)
        body["model"] = self._strip_prefix(model_string)
        url = f"{self._base_url()}/chat/completions"
        return await self.client.post(url, content=json.dumps(body))

    async def proxy_embeddings(self, body_bytes: bytes) -> httpx.Response:
        body = json.loads(body_bytes)
        if "model" in body:
            body["model"] = self._strip_prefix(body["model"])
        url = f"{self._base_url()}/embeddings"
        return await self.client.post(url, content=json.dumps(body))
