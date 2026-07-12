# Provider Adapters Reference

llm-pico translates OpenAI-format requests to each upstream provider's native API format and translates responses back. Each provider is implemented as a separate adapter class in `providers/`. Adapters are **lazily loaded** — only imported when first used.

---

## Supported Providers

| Provider | Prefix | Adapter Class | Streaming | Images | Embeddings | STT/TTS |
|----------|--------|---------------|-----------|--------|------------|---------|
| OpenAI | `openai/` | `OpenAIAdapter` | Yes (SSE) | Yes | Yes | No |
| Anthropic | `anthropic/` | `AnthropicAdapter` | Yes (SSE) | Yes | No | No |
| Google Gemini | `gemini/` | `GeminiAdapter` | Yes (JSON lines) | Yes | Yes | No |
| Cloudflare Workers AI | `cloudflare/` | `CloudflareAdapter` | Yes (SSE) | No | Yes | No |

Any unrecognized prefix falls through to the OpenAI adapter as a raw passthrough.

---

## OpenAI Adapter (`providers/openai.py`)

**When to use:** Any OpenAI-compatible endpoint (OpenAI, Groq, OpenRouter, NVIDIA NIM, Zhipu, or any third-party OpenAI-compat server).

**Key behaviors:**
- Parses JSON to apply two body-level transformations:
  - **`max_tokens` → `max_completion_tokens`**: Newer models (gpt-5.4-mini+) reject `max_tokens`; this is rewritten transparently.
  - **Thinking-tag stripping**: `<think>...</think>` blocks are removed from response content.
- Guards JSON parsing on 200 responses — only parses when `content-type` contains `json` (avoids crash on SSE streaming responses).
- Shared httpx client per provider slug (max 10 connections, 300s timeout).

**Configuration:**
```yaml
model_params:
  model: "openai/gpt-5.4-mini"
  api_key: "${OPENAI_API_KEY}"
  api_base: null           # or custom endpoint
```

---

## Anthropic Adapter (`providers/anthropic.py`)

**When to use:** Direct Anthropic API access for Claude models.

**Translation details:**
- Request: OpenAI messages → Anthropic `/v1/messages` format
  - `role: "system"` messages extracted into the `system` parameter
  - `image_url` content parts converted to Anthropic `image` blocks with base64 `source`
  - `input_audio` content parts converted to Anthropic `image` blocks with audio MIME types
  - `messages` list restructured (Anthropic requires user/model alternation)
- Response: Anthropic → OpenAI format
  - `content` blocks concatenated into `choices[0].message.content`
  - `usage.input_tokens` / `usage.output_tokens` → `usage.prompt_tokens` / `usage.completion_tokens`
  - `stop_reason` mapped: `end_turn` → `stop`, `max_tokens` → `length`, `tool_use` → `tool_calls`
- Streaming: reads SSE `event: content_block_delta` lines, converts to OpenAI `data: {...}` chunks; usage tracked from `message_start` + `message_delta` events

**Configuration:**
```yaml
model_params:
  model: "anthropic/claude-opus-4-5"
  api_key: "${ANTHROPIC_API_KEY}"
  api_base: null           # default: https://api.anthropic.com/v1
```

**Note:** The adapter sends `anthropic-version: 2023-06-01` header automatically.

---

## Gemini Adapter (`providers/gemini.py`)

**When to use:** Google Gemini / Gemma models via the Generative Language API.

**Translation details:**
- Request: OpenAI messages → Google AI `generateContent` format
  - `role: "system"` messages → `systemInstruction`
  - Messages → `contents` with `user` / `model` roles
  - `image_url` base64 → `inline_data` with `mime_type`
  - Generation config: `maxOutputTokens`, `temperature`, `topP`, `stopSequences`
- API key sent as query parameter `?key=` (not header)
- Two endpoints: `generateContent` (non-streaming) and `streamGenerateContent` (streaming)
- Streaming reads JSON lines (not SSE), each line may contain multiple candidates; usage tracked from `usageMetadata`
- Thought/reasoning parts (`"thought": true`) are excluded from response content

**Configuration:**
```yaml
model_params:
  model: "gemini/gemma-4-31b-it"
  api_key: "${GEMINI_API_KEY}"
  api_base: null           # default: https://generativelanguage.googleapis.com/v1beta
```

**Embeddings:**
The Gemini adapter also supports embeddings via `batchEmbedContents`. Set `embeddings: true` on the model entry and use the `/v1/embeddings` endpoint.

```yaml
model_params:
  model: "gemini/gemini-embedding-2"
  api_key: "${GEMINI_API_KEY}"
embeddings: true
```

---

## Cloudflare Workers AI Adapter (`providers/cloudflare.py`)

**When to use:** Cloudflare Workers AI models.

**Key behaviors:**
- OpenAI-compatible passthrough (no request/response translation needed)
- Strips the `cloudflare/` prefix from the model name before forwarding
- `proxy_embeddings()` strips prefix from the model field in the request body
- `api_base` must include your Cloudflare account ID: `https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/ai/v1`

**Configuration:**
```yaml
model_params:
  model: "cloudflare/@cf/zai-org/glm-5.2"
  api_key: "${CF_API_TOKEN}"
  api_base: "https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/ai/v1"
```

---

## Writing a New Adapter

1. Create `providers/yourprovider.py`
2. Subclass `BaseAdapter`:
   ```python
   from providers.base import BaseAdapter
   from providers import register

   @register("yourprovider")
   class YourAdapter(BaseAdapter):
       provider = "yourprovider"
       supports_images = True    # set capability flags

       def _base_url(self) -> str:
           return (self.api_base or "https://api.example.com/v1").rstrip("/")

       def peek_request(self, body: bytes) -> tuple[str, bool, int]:
           obj = json.loads(body)
           return obj.get("model", ""), obj.get("stream", False), obj.get("max_tokens", 4096) or 4096

       async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
           body = json.loads(body_bytes)
           # Translate OpenAI format → provider format
           url = f"{self._base_url()}/chat/completions"
           return await self.client.post(url, content=json.dumps(body), headers=self._headers())
   ```
3. The adapter is auto-registered via `@register` and lazy-loaded when `get_adapter("yourprovider")` is first called — no import needed in `__init__.py`.

**`BaseAdapter` shared infrastructure:**
- `_headers()` — returns auth headers (API key in `Authorization: Bearer ...` for most providers)
- `_get_client(provider_slug)` — shared `httpx.AsyncClient` per provider slug (max 10 connections, 300s timeout, 15s keepalive)
- `has_image_input(body)` — checks for `type: "image_url"` in message content
- `proxy_stream(response)` — default SSE streaming helper (override for non-SSE providers like Gemini/Anthropic)
