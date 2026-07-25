from __future__ import annotations

import json
import logging
import re

import httpx

from providers.base import BaseAdapter
from providers import register

_log = logging.getLogger("llm-pico.providers.openai")


@register("openai")
class OpenAIAdapter(BaseAdapter):
    provider = "openai"
    supports_images = True
    supports_embeddings = True

    def __init__(self, provider_slug: str = "openai", api_key: str | None = None, api_base: str | None = None) -> None:
        super().__init__(provider_slug=provider_slug, api_key=api_key, api_base=api_base)

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        super()._set_auth_headers(headers)
        headers["Authorization"] = f"Bearer {self.api_key}"

    def _base_url(self) -> str:
        return (self.api_base or "https://api.openai.com/v1").rstrip("/")

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        body = json.loads(body_bytes)
        # Newer OpenAI models (gpt-5*, o3*, o4*) reject max_tokens and require max_completion_tokens
        model_str = body.get("model", "")
        is_newer_model = any(model_str.startswith(p) for p in ("gpt-5", "o3", "o4"))
        if is_newer_model and "max_tokens" in body and "max_completion_tokens" not in body:
            body["max_completion_tokens"] = body.pop("max_tokens")
        url = f"{self._base_url()}/chat/completions"
        response = await self.client.post(url, content=json.dumps(body), headers=self._headers())

        if response.status_code == 200:
            resp_body = json.loads(response.content)
            if "choices" in resp_body:
                for choice in resp_body["choices"]:
                    msg = choice.get("message", {})
                    if "content" in msg and msg["content"]:
                        msg["content"] = re.sub(r"<think>.*?</think>", "", msg["content"], flags=re.DOTALL).strip()
            response._content = json.dumps(resp_body).encode()

        return response

    async def proxy_completions(self, body_bytes: bytes) -> httpx.Response:
        url = f"{self._base_url()}/completions"
        return await self.client.post(url, content=body_bytes, headers=self._headers())

    async def proxy_embeddings(self, body_bytes: bytes) -> httpx.Response:
        url = f"{self._base_url()}/embeddings"
        return await self.client.post(url, content=body_bytes, headers=self._headers())

    async def proxy_audio_transcriptions(
        self,
        audio_bytes: bytes,
        filename: str,
        content_type: str,
        model_string: str,
        extra_data: dict | None = None,
    ) -> httpx.Response:
        url = f"{self._base_url()}/audio/transcriptions"
        model = model_string.split("/", 1)[-1] if "/" in model_string else model_string
        files = {"file": (filename, audio_bytes, content_type)}
        data = {"model": model}
        if extra_data:
            data.update(extra_data)
        h = self._headers()
        h.pop("Content-Type", None)
        return await self.client.post(url, files=files, data=data, headers=h)

    async def proxy_tts(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        url = f"{self._base_url()}/audio/speech"
        return await self.client.post(url, content=body_bytes, headers=self._headers())

    async def probe_capabilities(self, model: str) -> dict:
        """Send minimal request to detect model capabilities."""
        caps = {"supports_tools": False, "supports_vision": False, "supports_json": False}
        try:
            probe_body = json.dumps({
                "model": model,
                "messages": [{"role": "user", "content": "Hi"}],
                "max_tokens": 1,
                "tools": [{"type": "function", "function": {"name": "test", "parameters": {"type": "object", "properties": {}}}}],
                "response_format": {"type": "json_object"},
            })
            resp = await self.client.post(
                f"{self._base_url()}/chat/completions",
                content=probe_body,
                headers=self._headers(),
            )
            if resp.status_code == 200:
                caps["supports_tools"] = True
                caps["supports_json"] = True
            elif resp.status_code == 400:
                text = resp.text.lower()
                if "tools" in text or "function" in text:
                    caps["supports_tools"] = False
                if "response_format" in text:
                    caps["supports_json"] = False
        except Exception:
            pass
        return caps
