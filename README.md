# llm-pico

Hyper-lightweight LLM proxy (<200MB RAM). Single OpenAI-compatible endpoint routing to 15+ models across 7 providers.

```bash
pip install llm-pico
llm-pico --config config.yaml --users users.yaml
```

## Features

- **OpenAI-compatible** — drop-in replacement for `openai` Python SDK, cURL, any OpenAI client
- **7 providers** — OpenAI, Google Gemini, Groq, Zhipu, Cloudflare, OpenRouter, NVIDIA NIM
- **3 custom adapters** — Anthropic, Gemini, Cloudflare (full request/response translation)
- **Passthrough for 4+ providers** — Groq, Zhipu, OpenRouter, NVIDIA NIM (raw bytes, OpenAI-compat)
- **Key pooling** — multiple API keys per provider, auto-failover on 429
- **Circuit breaker** — after 3 consecutive 5xx, skip provider for 30s
- **Rate limiting** — per-user and per-model: RPM (in-memory), RPD (SQLite), TPM, TPD
- **Streaming** — full SSE passthrough with token reconciliation
- **Auth** — `sk-pico-*` user keys with per-model allowlists
- **Admin API** — key CRUD, usage stats, audit log, config reload
- **Docker** — 168MB multi-stage image

## Quick Start

### 1. Configuration

Copy the example config and add your API keys:

```bash
cp config.example.yaml config.yaml
# Edit config.yaml — set master_key and API keys
```

Create user keys:

```bash
cp users.example.yaml users.yaml
# Edit users.yaml or use the admin API
```

### 2. Run

```bash
# Direct
llm-pico --verbose

# Docker
docker run -d -p 4000:4000 \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/users.yaml:/app/users.yaml \
  llm-pico:latest
```

### 3. Use

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-pico-dev-..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.4-mini",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

## Configuration

### config.yaml

```yaml
router_settings:
  routing_strategy: simple-shuffle   # how to pick between keys
  num_retries: 2                     # retries on 429/5xx
  cooldown_time: 45                  # seconds to cool down 429'd key
  circuit_breaker:
    failure_threshold: 3             # consecutive 5xx before opening
    recovery_timeout: 30             # seconds before retry

general_settings:
  master_key: "sk-pico-master-..."   # admin API key
  db_path: "llm-pico.db"             # optional, default: next to config

model_list:
  - model_name: gpt-5.4-mini         # the name clients use
    litellm_params:
      model: openai/gpt-5.4-mini     # provider/model-name
      api_key: "sk-..."
      api_base: null                 # optional override
    rpm: 50                          # model-level rate limits
    rpd: 5000
    ash: 7200                        # audio seconds per hour (STT/TTS)
    asd: 2880                        # audio seconds per day
    images: false                    # capability flags
    embeddings: false
    stt: false                       # speech-to-text
    tts: false                       # text-to-speech
```

### Model naming format

The `model` field in `litellm_params` uses the format `<provider>/<upstream-model-name>`.

| Provider | Slug | Example |
|----------|------|---------|
| OpenAI | `openai` | `openai/gpt-5.4-mini` |
| Google Gemini | `gemini` | `gemini/gemini-3-flash-preview` |
| Anthropic | `anthropic` | `anthropic/claude-sonnet-4` |
| Groq | `groq` | `groq/openai/gpt-oss-120b` |
| Zhipu | `zai` | `zai/glm-4-flash` |
| Cloudflare | `cloudflare` | `cloudflare/@cf/meta/llama-3.1-8b-instruct` |
| OpenRouter | `openrouter` | `openrouter/poolside/laguna-m.1:free` |
| NVIDIA NIM | `nvidia_nim` | `nvidia_nim/meta/llama3-70b-instruct` |

Unregistered provider slugs fall through to the OpenAI-compatible passthrough.

### users.yaml

```yaml
users:
  - key: "sk-pico-dev-abc123..."
    label: "dev-bot"
    models: null                    # null = all models allowed
    rpm: 100
    rpd: 10000
```

## Admin API

All admin endpoints require the master key in the `Authorization` header.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/keys` | List all user keys |
| `POST` | `/admin/keys` | Create a new `sk-pico-*` key |
| `DELETE` | `/admin/keys/{prefix}` | Revoke a key by prefix |
| `PUT` | `/admin/keys/{prefix}/models` | Set model allowlist |
| `PUT` | `/admin/keys/{prefix}/limits` | Set rate limits |
| `GET` | `/admin/usage` | Usage statistics |
| `GET` | `/admin/usage/top-models` | Top models by tokens |
| `POST` | `/admin/config/reload` | Graceful config reload |
| `GET` | `/admin/log` | Admin audit log |

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/v1/chat/completions` | POST | Chat completions (streaming + non-streaming) |
| `/v1/completions` | POST | Legacy completions |
| `/v1/embeddings` | POST | Embeddings |
| `/v1/models` | GET | List models (filtered by key allowlist) |
| `/v1/models/{id}` | GET | Single model |
| `/health` | GET | Health check |
| `/v1/images/*` | POST | 501 (placeholder) |
| `/v1/audio/*` | POST | 501 (placeholder) |
| `/v1/moderations` | POST | 501 (placeholder) |

## Architecture

```
Client ──► Auth ──► Rate Limiter ──► Router ──► Adapter ──► Upstream API
                                                  │
                                           httpx.AsyncClient
                                           (per-provider pools)
```

- **Auth**: SHA-256 key lookup in SQLite, per-model allowlists
- **Rate Limiter**: RPM/TPM in-memory (sharded locks, ~200ns), RPD/TPD in SQLite, token reservation + reconcile for streaming
- **Router**: Groups keys by `(provider, api_base)`, circuit breaker per group, simple-shuffle key selection, cooldown on 429/401/403
- **Adapter**: Translates OpenAI format → provider format and back, 3 custom adapters (Anthropic, Gemini, Cloudflare), rest pass through via OpenAIAdapter
- **Retry loop**: 5xx/429/connection errors retry with next healthy key; 400/401/403/404/501 propagate immediately

## Resource Usage

| Concurrency | RAM |
|-------------|-----|
| Idle | ~30MB |
| 10 concurrent | ~80MB |
| 50 concurrent | ~140MB |

## Development

```bash
git clone https://github.com/your-org/llm-pico
cd llm-pico
python -m venv .venv && source .venv/bin/activate
pip install -e .
pip install pytest pytest-asyncio
pytest
```

## Production Deployment

Run behind a reverse proxy (Caddy, nginx) for TLS termination:

```bash
llm-pico --host 127.0.0.1 --port 4000 --config /etc/llm-pico/config.yaml
```

For Docker, mount config and data directories:

```bash
docker run -d -p 127.0.0.1:4000:4000 \
  -v /etc/llm-pico/config.yaml:/app/config.yaml:ro \
  -v /etc/llm-pico/users.yaml:/app/users.yaml:ro \
  -v /data/llm-pico:/app/data \
  llm-pico:latest
```
