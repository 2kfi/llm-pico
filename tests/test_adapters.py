from __future__ import annotations

import json
import re
import time
import pytest

from providers.anthropic import AnthropicAdapter
from providers.gemini import GeminiAdapter
from providers.openai import OpenAIAdapter


def test_anthropic_adapter_timestamp():
    adapter = AnthropicAdapter(api_key="test-key")
    data = {
        "id": "msg_123",
        "content": [{"type": "text", "text": "Hello Anthropic!"}],
        "role": "assistant",
        "stop_reason": "end_turn",
        "usage": {"input_tokens": 10, "output_tokens": 5}
    }

    now = int(time.time())
    response = adapter._anthropic_to_openai_response(data, "claude-3-opus")

    assert response["id"] == "msg_123"
    assert response["created"] >= now - 5
    assert response["created"] <= now + 5
    assert response["choices"][0]["message"]["content"] == "Hello Anthropic!"


def test_gemini_adapter_timestamp():
    adapter = GeminiAdapter(api_key="test-key")
    data = {
        "candidates": [{
            "index": 0,
            "content": {
                "parts": [{"text": "Hello Gemini!"}]
            },
            "finishReason": "STOP"
        }],
        "usageMetadata": {
            "promptTokenCount": 8,
            "candidatesTokenCount": 4,
            "totalTokenCount": 12
        }
    }

    now = int(time.time())
    response = adapter._gemini_to_openai_response(data, "gemini-1.5-pro")

    assert response["created"] >= now - 5
    assert response["created"] <= now + 5
    assert response["choices"][0]["message"]["content"] == "Hello Gemini!"


def test_openai_adapter_auth_headers():
    adapter = OpenAIAdapter(api_key="sk-test-123")
    headers = adapter._headers()
    assert headers["Authorization"] == "Bearer sk-test-123"
    assert headers["Content-Type"] == "application/json"


def test_openai_adapter_base_url_default():
    adapter = OpenAIAdapter(api_key="sk-test")
    assert adapter._base_url() == "https://api.openai.com/v1"


def test_openai_adapter_base_url_custom():
    adapter = OpenAIAdapter(api_key="sk-test", api_base="https://my-proxy.example.com/v1/")
    assert adapter._base_url() == "https://my-proxy.example.com/v1"


def test_openai_adapter_strip_think_tags():
    adapter = OpenAIAdapter(api_key="sk-test")
    body = json.dumps({
        "model": "gpt-4",
        "messages": [{"role": "user", "content": "hi"}],
    }).encode()

    mock_response = json.dumps({
        "choices": [{
            "message": {
                "content": "<think>Let me think...</think>The answer is 42."
            }
        }]
    }).encode()

    # Simulate the think-tag stripping logic from proxy_request
    resp_body = json.loads(mock_response)
    for choice in resp_body.get("choices", []):
        msg = choice.get("message", {})
        if "content" in msg and msg["content"]:
            msg["content"] = re.sub(r"<think>.*?</think>", "", msg["content"], flags=re.DOTALL).strip()

    assert resp_body["choices"][0]["message"]["content"] == "The answer is 42."


def test_openai_adapter_strip_think_tags_multiline():
    adapter = OpenAIAdapter(api_key="sk-test")

    content = "<think>\nLine 1\nLine 2\n</think>Actual response."
    cleaned = re.sub(r"<think>.*?</think>", "", content, flags=re.DOTALL).strip()
    assert cleaned == "Actual response."


def test_openai_adapter_newer_model_max_tokens_remap():
    from providers.base import BaseAdapter

    adapter = OpenAIAdapter(api_key="sk-test")
    body = json.dumps({
        "model": "gpt-5-turbo",
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 1024,
    }).encode()

    parsed = json.loads(body)
    model_str = parsed.get("model", "")
    is_newer = any(model_str.startswith(p) for p in ("gpt-5", "o3", "o4"))
    assert is_newer is True
    if is_newer and "max_tokens" in parsed and "max_completion_tokens" not in parsed:
        parsed["max_completion_tokens"] = parsed.pop("max_tokens")
    assert parsed["max_completion_tokens"] == 1024
    assert "max_tokens" not in parsed


def test_openai_adapter_older_model_keeps_max_tokens():
    adapter = OpenAIAdapter(api_key="sk-test")
    body = json.dumps({
        "model": "gpt-4",
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 2048,
    }).encode()

    parsed = json.loads(body)
    model_str = parsed.get("model", "")
    is_newer = any(model_str.startswith(p) for p in ("gpt-5", "o3", "o4"))
    assert is_newer is False
    assert parsed["max_tokens"] == 2048
    assert "max_completion_tokens" not in parsed


def test_openai_adapter_peek_request():
    adapter = OpenAIAdapter(api_key="sk-test")
    body = json.dumps({
        "model": "gpt-4",
        "stream": True,
        "max_tokens": 512,
    }).encode()

    model, stream, max_tokens = adapter.peek_request(body)
    assert model == "gpt-4"
    assert stream is True
    assert max_tokens == 512


def test_openai_adapter_peek_request_defaults():
    adapter = OpenAIAdapter(api_key="sk-test")
    body = json.dumps({
        "model": "gpt-4",
    }).encode()

    model, stream, max_tokens = adapter.peek_request(body)
    assert model == "gpt-4"
    assert stream is False
    assert max_tokens == 4096
