from __future__ import annotations

import json
import logging
import re
import time
import uuid
from typing import Any

import httpx

from providers.base import BaseAdapter
from providers import register

_log = logging.getLogger("llm-pico.providers.gemini")


class _URLSanitizeFilter(logging.Filter):
    """Strip API keys from URLs in log messages."""

    _KEY_RE = re.compile(r"([?&]key=)[^&\s]+")

    def filter(self, record: logging.LogRecord) -> bool:
        if isinstance(record.msg, str):
            record.msg = self._KEY_RE.sub(r"\1[REDACTED]", record.msg)
        if record.args:
            if isinstance(record.args, tuple):
                record.args = tuple(
                    self._KEY_RE.sub(r"\1[REDACTED]", a) if isinstance(a, str) else a
                    for a in record.args
                )
        return True


_log.addFilter(_URLSanitizeFilter())

_AUDIO_MIME = {
    "mp3": "audio/mpeg",
    "wav": "audio/wav",
    "opus": "audio/opus",
    "aac": "audio/aac",
    "flac": "audio/flac",
    "ogg": "audio/ogg",
    "pcm": "audio/L16",
}


@register("gemini")
class GeminiAdapter(BaseAdapter):
    provider = "gemini"
    supports_images = True
    supports_embeddings = True

    def __init__(self, provider_slug: str = "gemini", api_key: str | None = None, api_base: str | None = None) -> None:
        super().__init__(provider_slug=provider_slug, api_key=api_key, api_base=api_base)

    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        headers["Content-Type"] = "application/json"

    def _base_url(self) -> str:
        return (self.api_base or "https://generativelanguage.googleapis.com/v1beta").rstrip("/")

    def _build_url(self, model: str, stream: bool = False) -> str:
        base = self._base_url()
        clean_model = model.split("/", 1)[1] if "/" in model else model
        endpoint = "streamGenerateContent" if stream else "generateContent"
        return f"{base}/models/{clean_model}:{endpoint}?key={self.api_key}"

    def _openai_to_gemini_contents(self, body: dict) -> tuple[list[dict], dict | None]:
        system_instruction = None
        contents = []

        for msg in body.get("messages", []):
            role = msg["role"]
            content = msg.get("content", "")

            if role == "system":
                if isinstance(content, str):
                    system_instruction = {"parts": [{"text": content}]}
                else:
                    texts = [p["text"] for p in content if p.get("type") == "text"]
                    system_instruction = {"parts": [{"text": t} for t in texts]}
                continue

            gemini_role = "user" if role in ("user", "tool") else "model"
            if isinstance(content, str):
                contents.append({"role": gemini_role, "parts": [{"text": content}]})
            else:
                parts = []
                for p in content:
                    if p.get("type") == "text":
                        parts.append({"text": p["text"]})
                    elif p.get("type") == "image_url":
                        url = p["image_url"]["url"]
                        if url.startswith("data:"):
                            mime_type, b64 = url[5:].split(";", 1)
                            if b64.startswith("base64,"):
                                b64 = b64[7:]
                            mime_type = mime_type.split(";")[0]
                            parts.append({"inline_data": {"mime_type": mime_type, "data": b64}})
                        else:
                            parts.append({"text": url})
                    elif p.get("type") == "input_audio":
                        audio = p["input_audio"]
                        fmt = audio.get("format", "mp3")
                        mime_type = _AUDIO_MIME.get(fmt, f"audio/{fmt}")
                        parts.append({"inline_data": {"mime_type": mime_type, "data": audio["data"]}})
                contents.append({"role": gemini_role, "parts": parts})

        return contents, system_instruction

    def _gemini_to_openai_response(self, data: dict, model: str) -> dict:
        candidates = data.get("candidates", [])
        choices = []
        for i, cand in enumerate(candidates):
            content = cand.get("content", {})
            parts = content.get("parts", [])
            text = "".join(p.get("text", "") for p in parts if not p.get("thought"))
            choices.append({
                "index": i,
                "message": {
                    "role": content.get("role", "assistant"),
                    "content": text,
                },
                "finish_reason": _map_gemini_finish(cand.get("finishReason", "")),
            })

        usage = data.get("usageMetadata", {})
        return {
            "id": f"chatcmpl-{uuid.uuid4().hex}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": choices,
            "usage": {
                "prompt_tokens": usage.get("promptTokenCount", 0),
                "completion_tokens": usage.get("candidatesTokenCount", 0),
                "total_tokens": usage.get("totalTokenCount", 0),
            },
        }

    def _build_gemini_request(self, body: dict, model: str) -> dict:
        contents, system = self._openai_to_gemini_contents(body)
        req = {"contents": contents}
        if system:
            req["systemInstruction"] = system

        gc = {}
        if body.get("max_tokens"):
            gc["maxOutputTokens"] = body["max_tokens"]
        if body.get("temperature") is not None:
            gc["temperature"] = body["temperature"]
        if body.get("top_p") is not None:
            gc["topP"] = body["top_p"]
        if body.get("stop"):
            stops = body["stop"] if isinstance(body["stop"], list) else [body["stop"]]
            gc["stopSequences"] = stops
        if gc:
            req["generationConfig"] = gc

        return req

    def peek_request(self, body: bytes) -> tuple[str, bool, int]:
        obj = json.loads(body)
        model = obj.get("model", "")
        stream = obj.get("stream", False)
        max_tokens = obj.get("max_tokens", 4096) or 4096
        return model, stream, max_tokens

    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        body = json.loads(body_bytes)
        stream = body.get("stream", False)
        gemini_req = self._build_gemini_request(body, model_string)
        url = self._build_url(model_string, stream=stream)
        response = await self.client.post(url, content=json.dumps(gemini_req))

        if response.status_code != 200:
            return response

        if stream:
            return response

        data = response.json()
        openai_resp = self._gemini_to_openai_response(data, model_string)
        response._content = json.dumps(openai_resp).encode()
        return response

    async def proxy_embeddings(self, body_bytes: bytes) -> httpx.Response:
        body = json.loads(body_bytes)
        inp = body.get("input", "")
        inputs = inp if isinstance(inp, list) else [inp]

        model = body.get("model", "")
        clean_model = model.split("/", 1)[1] if "/" in model else model
        gemini_model = f"models/{clean_model}"
        requests_list = []
        for text in inputs:
            requests_list.append({
                "model": gemini_model,
                "content": {"parts": [{"text": text}]},
            })
        url = f"{self._base_url()}/models/{clean_model}:batchEmbedContents?key={self.api_key}"
        response = await self.client.post(url, content=json.dumps({"requests": requests_list}))

        if response.status_code != 200:
            return response

        data = response.json()
        embeddings = data.get("embeddings", [])

        openai_data = []
        for i, emb in enumerate(embeddings):
            openai_data.append({
                "object": "embedding",
                "embedding": emb.get("values", []),
                "index": i,
            })

        openai_resp = {
            "object": "list",
            "data": openai_data,
            "model": model,
            "usage": {
                "prompt_tokens": sum(len(t.split()) for t in inputs),
                "total_tokens": sum(len(t.split()) for t in inputs),
            },
        }

        response._content = json.dumps(openai_resp).encode()
        return response

    async def proxy_stream(
        self, response: httpx.Response
    ) -> tuple[list[bytes], dict[str, Any] | None]:
        chunks: list[bytes] = []
        usage: dict[str, Any] | None = None

        async for line in response.aiter_lines():
            if not line.strip():
                continue
            try:
                data = json.loads(line)
            except json.JSONDecodeError:
                continue

            if "usageMetadata" in data:
                meta = data["usageMetadata"]
                usage = {
                    "prompt_tokens": meta.get("promptTokenCount", 0),
                    "completion_tokens": meta.get("candidatesTokenCount", 0),
                    "total_tokens": meta.get("totalTokenCount", 0),
                }

            candidates = data.get("candidates", [])
            for cand in candidates:
                parts = cand.get("content", {}).get("parts", [])
                text = "".join(p.get("text", "") for p in parts if not p.get("thought"))
                finish = _map_gemini_finish(cand.get("finishReason", ""))

                sse_data = {
                    "id": f"chatcmpl-{uuid.uuid4().hex}",
                    "object": "chat.completion.chunk",
                    "created": int(time.time()),
                    "model": cand.get("model", ""),
                    "choices": [{
                        "index": cand.get("index", 0),
                        "delta": {"content": text},
                        "finish_reason": finish or None,
                    }],
                }
                chunks.append(f"data: {json.dumps(sse_data)}\n\n".encode())

        if chunks:
            chunks.append(b"data: [DONE]\n\n")

        return chunks, usage


def _map_gemini_finish(reason: str) -> str:
    mapping = {
        "STOP": "stop",
        "MAX_TOKENS": "length",
        "SAFETY": "content_filter",
        "RECITATION": "content_filter",
        "OTHER": "stop",
    }
    return mapping.get(reason, "stop")
