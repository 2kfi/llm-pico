# API Endpoints

llm-pico exposes an OpenAI-compatible API. All endpoints require authentication via Bearer token.

## Base URL

```
http://localhost:4000
```

## Authentication

All requests require an `Authorization: Bearer <key>` header.

- **User keys:** `sk-pico-*` (from `users.yaml` or admin API)
- **Master key:** Set in `general_settings.master_key` (full admin access)

## Endpoints

### POST /v1/chat/completions

OpenAI-compatible chat completions. Supports streaming and buffered responses.

**Request:**

```json
{
  "model": "gpt-4",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": false,
  "max_tokens": 1024,
  "temperature": 0.7
}
```

**Streaming request:**

```json
{
  "model": "gpt-4",
  "messages": [{"role": "user", "content": "Hello!"}],
  "stream": true
}
```

**Response (buffered):**

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "gpt-4",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Hello! How can I help?"},
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 8,
    "total_tokens": 20
  }
}
```

**Streaming response:** Standard SSE format with `data:` lines.

**Error responses:**

| Status | Meaning |
|--------|---------|
| 400 | Invalid JSON, missing model, capability mismatch |
| 401 | Invalid or missing API key |
| 403 | Model not allowed for this key |
| 404 | Model not found or all keys exhausted |
| 429 | Rate limit exceeded or budget exceeded |
| 500 | Internal server error |
| 501 | Adapter capability not implemented |
| 502 | Upstream provider error |
| 503 | Service degraded (degradation mode active) |
| 504 | Upstream timeout |

### POST /v1/completions

Alias for `/v1/chat/completions`. Same request/response format.

### POST /v1/embeddings

Text embeddings.

**Request:**

```json
{
  "model": "text-embedding-ada-002",
  "input": "The quick brown fox jumps over the lazy dog"
}
```

**Response:**

```json
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "embedding": [0.0023, -0.0091, ...],
      "index": 0
    }
  ],
  "model": "text-embedding-ada-002",
  "usage": {"prompt_tokens": 8, "total_tokens": 8}
}
```

**Requirements:**
- Model must have `embeddings: true` in config
- Adapter must support embeddings (OpenAI, Gemini, Cloudflare, Cohere)

### POST /v1/audio/transcriptions

Speech-to-text. Accepts multipart form data.

**Request:**

```bash
curl -X POST http://localhost:4000/v1/audio/transcriptions \
  -H "Authorization: Bearer $USER_KEY" \
  -F "file=@audio.wav" \
  -F "model=whisper-1"
```

**Form fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `file` | Yes | Audio file (wav, mp3, m4a, etc.) |
| `model` | Yes | Model name (e.g., `whisper-1`) |
| `response_format` | No | `json`, `text`, `verbose_json`, `srt`, `vtt` |
| `language` | No | ISO 639-1 code (e.g., `en`) |
| `temperature` | No | 0.0 to 1.0 |

**Requirements:**
- Model must have `stt: true` in config

### POST /v1/audio/speech

Text-to-speech.

**Request:**

```json
{
  "model": "tts-1",
  "input": "Hello, world!",
  "voice": "alloy"
}
```

**Response:** Binary audio data with appropriate `Content-Type`.

**Requirements:**
- Model must have `tts: true` in config

### GET /v1/models

List available models. Filtered by the requesting key's model allowlist.

**Request:**

```bash
curl http://localhost:4000/v1/models \
  -H "Authorization: Bearer $USER_KEY"
```

**Response:**

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-4",
      "object": "model",
      "created": 1234567890,
      "owned_by": "openai",
      "permission": [],
      "root": "gpt-4",
      "parent": null
    }
  ]
}
```

### GET /v1/models/{model_id}

Get a single model by ID.

**Response:**

```json
{
  "id": "gpt-4",
  "object": "model",
  "created": 1234567890,
  "owned_by": "openai"
}
```

### GET /health

Health check. No authentication required.

**Response:**

```json
{
  "status": "ok",
  "version": "0.1.0"
}
```

## Placeholder Endpoints

These endpoints return `501 Not Implemented`:

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/images/generations` | Image generation |
| POST | `/v1/images/edits` | Image editing |
| POST | `/v1/images/variations` | Image variations |
| POST | `/v1/audio/translations` | Audio translation |
| POST | `/v1/moderations` | Content moderation |

## Rate Limit Headers

All responses include rate limit headers:

```
X-RateLimit-rpm-Limit: 100
X-RateLimit-rpm-Remaining: 95
X-RateLimit-rpm-Reset: 1234567890

X-RateLimit-tpm-Limit: 150000
X-RateLimit-tpm-Remaining: 145000
X-RateLimit-tpm-Reset: 1234567890
```

Windows: `rpm`, `rpd`, `tpm`, `tpd`, `ash`, `asd`

## Error Response Format

All errors follow OpenAI's format:

```json
{
  "error": {
    "message": "Human-readable error message",
    "type": "error_type",
    "code": 400
  }
}
```

## Request Flow

```
Client → Auth → IP Check → Rate Limit → Degradation → Router → Adapter → Upstream API
                    ↓               ↓              ↓
             Budget Check    Token Reserve    Model Chain
```

1. **Auth** — Validate Bearer token (master key or user key)
2. **IP check** — Reject if client IP not in key's `ip_allowlist`
3. **Rate limit** — Check all windows atomically (user + model level)
4. **Budget** — Check monthly spend against user budget
5. **Degradation** — If mode is `reject`, return 503; if `queue`, wait; if `normal`, continue
6. **Cache** — Return cached response if available (non-streaming only)
7. **Router** — Pick provider key via weighted round-robin (health-score weighted)
8. **Adapter** — Translate request format if needed
9. **Forward** — Send to upstream API
10. **Retry** — On 429/5xx, rotate key and retry (up to `num_retries`)
11. **Failover** — If all retries exhausted, try `failover_model` if configured
12. **Chain** — If `model_chain` is set, process additional models in sequence

### Request Traces

Every request records spans in the `request_traces` table (auth → rate limit → router → upstream). Use `GET /admin/traces/{request_id}` to view the waterfall for debugging latency.

### Degradation Modes

| Mode | Behavior |
|------|----------|
| `normal` | All requests processed |
| `reject` | All requests return 503 immediately |
| `queue` | Requests queued, processed when mode returns to normal |
| `fallback_only` | Only failover models are used |

### Weighted Round-Robin

Keys are selected with probability proportional to their `health_score`. Healthier keys (fewer recent failures) are chosen more often. Cooldown keys are skipped.

### Chain-of-LLMs

When `model_chain` is configured on a team, requests pass through multiple models in sequence. Each model processes the output of the previous one. An optional `chain_rewrites_response` instruction rewrites the user prompt before the first model call.
