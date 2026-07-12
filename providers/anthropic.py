from __future__ import annotations

import json
import logging
import time
import uuid
from typing import Any

import httpx

from providers.base import BaseAdapter
from providers import register

_log = logging.getLogger("llm-pico.providers.anthropic")

_AUDIO_MIME = {
    "mp3": "audio/mpeg",
    "wav": "audio/wav",
    "opus": "audio/opus",
    "aac": "audio/aac",
    "flac": "audio/flac",
    "ogg": "audio/ogg",
    "pcm": "audio/L16",
}


@register("anthropic")
class AnthropicAdapter(BaseAdapter):
    provider = "anthropic"
    supports_images = True

    def __init__(self, provider_slug: str = "anthropic", api_key: str | None = None, api_base: str | None = None) -> None:
        super().__init__(provider_slug=provider_slug, api_key=api_key, api_base=api_base)

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        super()._set_auth_headers(headers)
        headers["x-api-key"] = self.api_key
        headers["anthropic-version"] = "2023-06-01"

    def _base_url(self) -> str:
        return (self.api_base or "https://api.anthropic.com/v1").rstrip("/")

    def _openai_to_anthropic_messages(self, body: dict) -> tuple[list[dict], str | None]:
        system_prompt = None
        messages = []
        for msg in body.get("messages", []):
            role = msg["role"]
            content = msg.get("content", "")

            if role == "system":
                if isinstance(content, str):
                    system_prompt = content
                else:
                    texts = [p["text"] for p in content if p.get("type") == "text"]
                    system_prompt = "\n".join(texts)
                continue

            if isinstance(content, str):
                messages.append({"role": role, "content": content})
            else:
                parts = []
                for p in content:
                    if p.get("type") == "text":
                        parts.append({"type": "text", "text": p["text"]})
                    elif p.get("type") == "image_url":
                        url = p["image_url"]["url"]
                        if url.startswith("data:"):
                            media_type, b64 = url[5:].split(";", 1)
                            if b64.startswith("base64,"):
                                b64 = b64[7:]
                            media_type = media_type.split(";")[0]
                        else:
                            parts.append({"type": "text", "text": url})
                            continue
                        parts.append({
                            "type": "image",
                            "source": {"type": "base64", "media_type": media_type, "data": b64},
                        })
                    elif p.get("type") == "input_audio":
                        audio = p["input_audio"]
                        fmt = audio.get("format", "mp3")
                        media_type = _AUDIO_MIME.get(fmt, f"audio/{fmt}")
                        parts.append({
                            "type": "image",
                            "source": {"type": "base64", "media_type": media_type, "data": audio["data"]},
                        })
                messages.append({"role": role, "content": parts})

        return messages, system_prompt

    def _anthropic_to_openai_response(self, data: dict, model: str) -> dict:
        content_text = ""
        for block in data.get("content", []):
            if block.get("type") == "text":
                content_text += block.get("text", "")

        usage = data.get("usage", {})
        return {
            "id": data.get("id", f"msg_{uuid.uuid4().hex}"),
            "object": "chat.completion",
            "created": int(uuid.uuid4().time),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {
                    "role": data.get("role", "assistant"),
                    "content": content_text,
                },
                "finish_reason": _map_anthropic_stop(data.get("stop_reason", "")),
            }],
            "usage": {
                "prompt_tokens": usage.get("input_tokens", 0),
                "completion_tokens": usage.get("output_tokens", 0),
                "total_tokens": usage.get("input_tokens", 0) + usage.get("output_tokens", 0),
            },
        }

    def _build_anthropic_request(self, body: dict, model: str) -> dict:
        messages, system = self._openai_to_anthropic_messages(body)
        req = {
            "model": model,
            "messages": messages,
            "max_tokens": body.get("max_tokens", 4096) or 4096,
        }
        if system:
            req["system"] = system
        if body.get("temperature") is not None:
            req["temperature"] = body["temperature"]
        if body.get("top_p") is not None:
            req["top_p"] = body["top_p"]
        if body.get("stop"):
            req["stop_sequences"] = body["stop"] if isinstance(body["stop"], list) else [body["stop"]]
        if body.get("stream"):
            req["stream"] = True
        return req

    def peek_request(self, body: bytes) -> tuple[str, bool, int]:
        obj = json.loads(body)
        model = obj.get("model", "")
        max_tokens = obj.get("max_tokens", 4096) or 4096
        return model, True, max_tokens

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        body = json.loads(body_bytes)
        anthropic_req = self._build_anthropic_request(body, model_string)
        url = f"{self._base_url()}/messages"
        is_stream = body.get("stream", False)

        if is_stream:
            response = await self.client.send(
                self.client.build_request("POST", url, content=json.dumps(anthropic_req), headers=self._headers()),
                stream=True,
            )
        else:
            response = await self.client.post(url, content=json.dumps(anthropic_req), headers=self._headers())

        if response.status_code != 200:
            if not is_stream:
                return response
            await response.aread()
            return response

        if is_stream:
            return response

        data = response.json()
        openai_resp = self._anthropic_to_openai_response(data, model_string)
        response._content = json.dumps(openai_resp).encode()
        return response

    async def proxy_stream(
        self, response: httpx.Response
    ) -> tuple[list[bytes], dict[str, Any] | None]:
        chunks: list[bytes] = []
        usage: dict[str, Any] | None = None

        event_type = None
        async for line in response.aiter_lines():
            if not line:
                continue

            if line.startswith("event: "):
                event_type = line[7:]
                continue

            if line.startswith("data: "):
                raw_data = line[6:]
            else:
                continue

            try:
                data = json.loads(raw_data)
            except json.JSONDecodeError:
                continue

            if event_type == "message_start":
                msg = data.get("message", {})
                usage = {
                    "prompt_tokens": msg.get("usage", {}).get("input_tokens", 0),
                    "completion_tokens": 0,
                    "total_tokens": msg.get("usage", {}).get("input_tokens", 0),
                }
            elif event_type == "message_delta":
                delta_usage = data.get("usage", {})
                if usage and delta_usage:
                    usage["completion_tokens"] = delta_usage.get("output_tokens", 0)
                    usage["total_tokens"] = usage["prompt_tokens"] + usage["completion_tokens"]
            elif event_type == "content_block_delta":
                delta = data.get("delta", {})
                if delta.get("type") == "text_delta":
                    sse_data = {
                        "id": data.get("id", ""),
                        "object": "chat.completion.chunk",
                        "created": int(uuid.uuid4().time),
                        "model": data.get("model", ""),
                        "choices": [{
                            "index": data.get("index", 0),
                            "delta": {"content": delta.get("text", "")},
                            "finish_reason": None,
                        }],
                    }
                    chunk = f"data: {json.dumps(sse_data)}\n\n".encode()
                    chunks.append(chunk)
            elif event_type == "message_stop":
                chunks.append(b"data: [DONE]\n\n")

        return chunks, usage


def _map_anthropic_stop(reason: str) -> str:
    mapping = {
        "end_turn": "stop",
        "max_tokens": "length",
        "stop_sequence": "stop",
        "tool_use": "tool_calls",
    }
    return mapping.get(reason, "stop")
