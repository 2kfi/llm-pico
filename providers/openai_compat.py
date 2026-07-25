from __future__ import annotations

import json
import logging

import httpx

from providers.base import BaseAdapter
from providers import register

_log = logging.getLogger("llm-pico.providers.openai_compat")


@register("openai-compat")
class OpenAICompatAdapter(BaseAdapter):
    """Universal adapter for ANY OpenAI-compatible endpoint.
    Works with: Groq, Together, Fireworks, DeepInfra, vLLM, Ollama, LM Studio, etc."""
    provider = "openai-compat"
    supports_images = True

    def __init__(self, provider_slug: str = "openai-compat", api_key: str | None = None, api_base: str | None = None) -> None:
        super().__init__(provider_slug=provider_slug, api_key=api_key, api_base=api_base)

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        super()._set_auth_headers(headers)
        headers["Authorization"] = f"Bearer {self.api_key}"

    def _base_url(self) -> str:
        return (self.api_base or "https://api.openai.com/v1").rstrip("/")

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        url = f"{self._base_url()}/chat/completions"
        return await self.client.post(url, content=body_bytes, headers=self._headers())

    async def proxy_embeddings(self, body_bytes: bytes) -> httpx.Response:
        url = f"{self._base_url()}/embeddings"
        return await self.client.post(url, content=body_bytes, headers=self._headers())

    async def proxy_completions(self, body_bytes: bytes) -> httpx.Response:
        url = f"{self._base_url()}/completions"
        return await self.client.post(url, content=body_bytes, headers=self._headers())
