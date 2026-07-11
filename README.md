# llm-pico

Hyper-lightweight LLM proxy — <200MB RSS, runs on a 15-year-old Atom CPU, zero external deps.

```bash
pip install llm-pico
llm-pico --config config.yaml --users users.yaml
```

## Features

- **OpenAI-compatible** — drop-in replacement for `openai` Python SDK, cURL, any OpenAI client
- **4 provider adapters** — OpenAI, Anthropic, Gemini, Cloudflare + any OpenAI-compatible API via passthrough
- **Key pooling** — multiple API keys per provider with circuit breaker + cooldown
- **6-window rate limiting** — RPM/RPD/TPM/TPD/ASH/ASD — all in-memory with periodic flush to SQLite
- **Full SSE streaming** — token reconciliation for accurate usage tracking
- **Per-user monthly budgets** — hard block, no soft limits
- **SQLite-backed** — zero-database-server, just a file
- **In-memory LRU request cache** — 256 entries, no disk I/O
- **Dark-theme admin dashboard** — key/team/user/budget management, live SSE log stream
- **X-RateLimit-\* response headers** — clients see their limits in real time
- **Config reload** — graceful drain + `execve` for zero-downtime updates
- **`${ENV_VAR}` config syntax** — secrets stay in environment variables, not config files
- **Constant-time auth** — `hmac.compare_digest` for master key verification
- **67 tests** — all passing

## Quick Start

### 1. Configuration

Copy the example config and add your API keys:

```bash
cp config.example.yaml config.yaml
# Edit config.yaml — set master_key and API keys
```

Or use environment variables directly in config:

```yaml
general_settings:
  master_key: "${LLM_PICO_MASTER_KEY}"   # from environment
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
general_settings:
  master_key: "sk-pico-master-..."       # admin API key
  db_path: "llm-pico.db"                 # optional, default: next to config
  usage_log_retention_days: 30           # days to keep per-user usage logs
  admin_log_retention_days: 90           # days to keep admin audit log

router_settings:
  routing_strategy: simple-shuffle       # how to pick between keys
  num_retries: 2                         # retries on 429/5xx
  cooldown_time: 45                      # seconds to cool down 429'd key
  circuit_breaker:
    failure_threshold: 3                 # consecutive 5xx before opening
    recovery_timeout: 30                 # seconds before retry

model_list:
  - model_name: gpt-5.4-mini             # the name clients use
    litellm_params:
      model: openai/gpt-5.4-mini         # provider/model-name
      api_key: "sk-..."
      api_base: null                     # optional override
    rpm: 50                              # model-level rate limits
    rpd: 5000
    ash: 7200                            # audio seconds per hour (STT/TTS)
    asd: 2880                            # audio seconds per day
    images: false                        # capability flags
    embeddings: false
    stt: false                           # speech-to-text
    tts: false                           # text-to-speech
```

Any value in the config can use `${VAR_NAME}` or `${VAR_NAME:-default}` syntax to reference environment variables. This keeps secrets out of config files entirely.

### Model naming format

The `model` field in `litellm_params` uses the format `<provider>/<upstream-model-name>`.

| Provider | Slug | Example |
|----------|------|---------|
| OpenAI | `openai` | `openai/gpt-5.4-mini` |
| Google Gemini | `gemini` | `gemini/gemini-3-flash-preview` |
| Anthropic | `anthropic` | `anthropic/claude-sonnet-4` |
| Cloudflare | `cloudflare` | `cloudflare/@cf/meta/llama-3.1-8b-instruct` |

Unregistered provider slugs fall through to the OpenAI-compatible passthrough.

### users.yaml

```yaml
users:
  - key: "sk-pico-dev-abc123..."
    label: "dev-bot"
    models: null                        # null = all models allowed
    rpm: 100
    rpd: 10000
    monthly_budget_tokens: 1000000      # hard block at limit
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
| `GET` | `/admin/budgets` | Budget summary (all users with spend) |
| `GET` | `/admin/teams` | List teams |
| `POST` | `/admin/teams` | Create team |
| `POST` | `/admin/config/reload` | Graceful config reload |
| `GET` | `/admin/log` | Admin audit log |
| `GET` | `/admin/logs` | Live log HTML dashboard |
| `GET` | `/admin/logs/stream` | SSE live log stream |

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
Client ──► Auth (Depends) ──► Rate Limiter ──► Router ──► Adapter ──► Upstream API
                               (in-memory)        │
                                          httpx.AsyncClient (shared per provider)
                                          SQLite (pool of 2 connections)
```

- **Auth**: FastAPI `Depends()`, `hmac.compare_digest` for master key — constant-time, no timing side-channels
- **Rate Limiter**: All 6 windows in-memory with periodic batch flush to SQLite, 4 shard locks for contention-free increments
- **Cache**: In-memory LRU (256 entries, no disk I/O)
- **Router**: Groups keys by `(provider, api_base)`, circuit breaker per group, simple-shuffle key selection, cooldown on 429/401/403
- **Adapter**: Translates OpenAI format → provider format and back, 3 custom adapters (Anthropic, Gemini, Cloudflare), rest pass through via OpenAIAdapter
- **httpx**: Shared client per provider (10 max connections, 5 keepalive, 15s expiry)
- **SQLite**: 2-connection pool, 64MB mmap, 8MB page cache, WAL mode

## Resource Usage

Measured on Intel Atom D410 (1 core / 2 threads @ 1.66 GHz), 2 GB DDR2:

| Concurrency | RAM |
|-------------|-----|
| Idle | ~54MB |
| 10 concurrent | ~91MB |
| 50 concurrent | ~127MB |

Startup time: ~1.9s

## What We Deliberately Don't Do

- **Redis** — violates zero-dependency design
- **PostgreSQL** — SQLite is sufficient for this scale
- **JWT/OAuth2/SSO** — master key is adequate for single-team deployments
- **Multi-level failover chains** — single failover model is enough for 4 providers
- **Key rotation with grace periods** — env-var config solves the real problem
- **Soft budgets** — hard block is simpler and safer
- **100+ provider adapters** — 4 + OpenAI-compatible fallback covers 95% of use cases

Design philosophy: **perfection of few features over abundance of mediocre ones**. A knife, not a Swiss Army knife.

## Development

```bash
git clone https://github.com/your-org/llm-pico
cd llm-pico
python -m venv .venv && source .venv/bin/activate
pip install -e ".[dev]"
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
