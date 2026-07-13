from __future__ import annotations

import json
import logging
from typing import Any

import httpx

from providers.base import BaseAdapter
from providers import register

_log = logging.getLogger("llm-pico.providers.cohere")


@register("cohere")
class CohereAdapter(BaseAdapter):
    provider = "cohere"
    supports_embeddings = True

    def __init__(self, provider_slug: str = "cohere", api_key: str | None = None, api_base: str | None = None) -> None:
        super().__init__(provider_slug=provider_slug, api_key=api_key, api_base=api_base)

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        headers["Content-Type"] = "application/json"
        headers["Authorization"] = f"Bearer {self.api_key}"

    def _base_url(self) -> str:
        return (self.api_base or "https://api.cohere.ai/compatibility/v1").rstrip("/")

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        body = json.loads(body_bytes)
        # Cohere expects model without provider prefix
        if "/" in body.get("model", ""):
            body["model"] = body["model"].split("/", 1)[1]
        url = f"{self._base_url()}/chat/completions"
        return await self.client.post(url, content=json.dumps(body), headers=self._headers())

    async def proxy_embeddings(self, body_bytes: bytes) -> httpx.Response:
        body = json.loads(body_bytes)
        # Cohere requires input_type for embeddings
        if "input_type" not in body:
            body["input_type"] = "search_document"
        # Cohere expects model without provider prefix
        if "/" in body.get("model", ""):
            body["model"] = body["model"].split("/", 1)[1]
        url = f"{self._base_url()}/embeddings"
        return await self.client.post(url, content=json.dumps(body), headers=self._headers())
