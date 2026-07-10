# llm-pico — Full Implementation Plan

## 1. Overview

**llm-pico** is a lightweight (<200MB RAM) LLM proxy that presents a single OpenAI-compatible endpoint and routes requests to 11 different LLM providers. It handles authentication, rate limiting, usage tracking, and provider switching behind the scenes.

### Core Design Decisions

| Decision | Choice | Rationale |
|---|---|---|---|
| Language | Python | LiteLLM compat, rich ecosystem |
| HTTP framework | FastAPI | Async, OpenAI-compat docs, Pydantic validation built-in |
| HTTP client | httpx | Async, per-provider connection pools |
| Database | SQLite + aiosqlite | Zero-dependency persistence, survives restarts |
| Rate limiting (RPM/TPM) | In-memory sharded counters (dict + asyncio.Lock) | ~200ns per check, zero DB write contention on hot path |
| Rate limiting (RPD/TPD) | SQLite rate_counters, flush every 10s if changed | Day-level windows tolerate latency; SQLite for durability |
| Provider adapters | Custom (no SDKs) | Tiny dependency footprint, full control over format translation |
| Passthrough parsing | Raw bytes for OpenAI-compat providers | Parse only model/stream/max_tokens (3 fields), forward raw JSON. No Pydantic overhead on hot path. |
| Token burst protection | Anticipatory reservation | Reserve `prompt+max_tokens` upfront per stream. Release unused post-stream. Prevents concurrent burst overshoot. |
| Provider outage handling | Error-class-aware retry + circuit breaker | 5xx → fail fast, no retry. 429 → swap key. Circuit breaker: 3 consecutive 5xx = open 30s. |
| TLS | Reverse proxy only (nginx/caddy) | Keep the proxy simple |
| Deployment | pip package + Docker image | Both options |
| Port | 4000 | Default listen port |
| Key format | `sk-pico-<random>` | Distinct from raw OpenAI keys |
| Config reload | Graceful drain + restart | Track active streams, drain with timeout, then restart |

---

## 2. Project Structure

```
/home/2kfi/.llm-pico/
├── pyproject.toml              # PEP 621 project metadata + deps
├── README.md                   # Usage guide (Phase 3)
├── Dockerfile                  # Multi-stage Docker build (Phase 3)
├── docker-compose.yml          # Quick-start compose (Phase 3)
├── config.example.yaml         # Reference config with all providers
├── users.example.yaml          # Reference user keys file
│
└── llm_pico/
    ├── __init__.py             # Package metadata
    ├── __main__.py             # python -m llm_pico entry
    ├── cli.py                  # Click CLI: llm-pico [options]
    ├── config.py               # Load + validate config.yaml + users.yaml
    ├── server.py               # FastAPI app factory
    ├── auth.py                 # API key lookup + model allowlist check
    ├── router.py               # Model→provider resolution + key pooling
    ├── ratelimit.py            # Fixed-window counters (user + model level)
    ├── db.py                   # SQLite schema init + connection management
    ├── models.py               # Pydantic request/response schemas
    ├── admin.py                # Admin API router (/admin/*)
    ├── usage.py                # Usage logging to SQLite
    │
    ├── adapters/
    │   ├── __init__.py         # Adapter registry
    │   ├── base.py             # Abstract base adapter
    │   ├── openai.py           # OpenAI (passthrough)
    │   ├── anthropic.py        # Anthropic (translate)
    │   ├── gemini.py           # Google Gemini (translate)
    │   ├── groq.py             # Groq (OpenAI-compat passthrough)
    │   ├── openrouter.py       # OpenRouter (OpenAI-compat passthrough)
    │   ├── cloudflare.py       # Cloudflare Workers AI (translate)
    │   ├── nvidia.py           # NVIDIA NIM (OpenAI-compat passthrough)
    │   ├── zhipu.py            # Zhipu AI (translate)
    │   ├── ollama.py           # Ollama (OpenAI-compat, api_base)
    │   ├── llamacpp.py         # llama.cpp (OpenAI-compat, api_base)
    │   └── vllm.py             # vLLM (OpenAI-compat, api_base)
    │
    └── placeholder.py          # 501 stubs for image/audio/moderation
```

---

## 3. Dependencies

```toml
[project]
name = "llm-pico"
version = "0.1.0"
requires-python = ">=3.11"
dependencies = [
    "fastapi>=0.115.0",
    "uvicorn[standard]>=0.32.0",
    "httpx>=0.28.0",
    "aiosqlite>=0.20.0",
    "click>=8.1.0",
    "pyyaml>=6.0",
    "pydantic>=2.0",
    "pydantic-settings>=2.0",
]
```

No provider SDKs. All upstream communication via `httpx`.

Each provider adapter gets its own `httpx.AsyncClient` with bounded connection limits:
```python
limits=httpx.Limits(
    max_connections=5,           # 5 concurrent outbound connections per provider
    max_keepalive_connections=3, # 3 kept alive between requests
    keepalive_expiry=30.0,       # seconds before closing idle keepalive
)
timeout=httpx.Timeout(
    connect=10.0,                # 10s TCP connect timeout
    read=300.0,                  # 5min read timeout (for long streaming)
    pool_timeout=10.0,           # 10s wait for a pool slot
)
```

11 providers × 5 connections = 55 outbound sockets max at any time. Well within the default 1024 FD soft limit.

---

## 4. SQLite Schema

File: auto-created at `{db_dir}/llm-pico.db` (default: next to config, overridable with `--db`).

```sql
CREATE TABLE IF NOT EXISTS user_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash        TEXT    NOT NULL UNIQUE,  -- SHA-256 of the raw key
    key_prefix      TEXT    NOT NULL,          -- First 12 chars for display (sk-pico-a1b2c3...)
    label           TEXT,                      -- Human-friendly name
    is_active       INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,          -- ISO-8601
    expires_at      TEXT,                     -- ISO-8601 or NULL
    model_allowlist TEXT,                     -- JSON array ["gpt-4",...] or NULL (all)
    rpm_limit       INTEGER,                  -- Per-user request limit per minute
    rpd_limit       INTEGER,                  -- Per-user request limit per day
    tpm_limit       INTEGER,                  -- Per-user token limit per minute
    tpd_limit       INTEGER                   -- Per-user token limit per day
);

CREATE TABLE IF NOT EXISTS usage_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash          TEXT    NOT NULL,
    key_prefix        TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,        -- The user-facing model_name
    provider          TEXT    NOT NULL,        -- The provider slug (openai, anthropic...)
    request_id        TEXT    NOT NULL,        -- UUID generated by proxy
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    status_code       INTEGER NOT NULL,
    error             TEXT,                    -- Error message if any, NULL on success
    created_at        TEXT    NOT NULL         -- ISO-8601
);

CREATE INDEX IF NOT EXISTS idx_usage_key    ON usage_log(key_hash);
CREATE INDEX IF NOT EXISTS idx_usage_model  ON usage_log(model_name);
CREATE INDEX IF NOT EXISTS idx_usage_time   ON usage_log(created_at);

-- Rate limit counters (fixed-window)
CREATE TABLE IF NOT EXISTS rate_counters (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash      TEXT    NOT NULL,
    model_name    TEXT    NOT NULL,
    level         TEXT    NOT NULL CHECK(level IN ('user', 'model')),
    window_type   TEXT    NOT NULL CHECK(window_type IN ('rpm', 'rpd', 'tpm', 'tpd')),
    window_start  TEXT    NOT NULL,            -- ISO-8601 truncated to minute or day
    count         INTEGER NOT NULL DEFAULT 0,
    UNIQUE(key_hash, model_name, level, window_type, window_start)
);

-- Admin audit log (who did what, when)
CREATE TABLE IF NOT EXISTS admin_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    action      TEXT    NOT NULL,              -- create_key, revoke_key, set_limits, etc.
    actor_hash  TEXT    NOT NULL,              -- master key hash
    details     TEXT,                          -- JSON payload with action details
    created_at  TEXT    NOT NULL
);
```

---

## 5. Rate Limiting — Two Layers × Two Storage Tiers

### Layer 1: Model-Level Limits (aggregate across all users)
Configured on each model entry in `config.yaml`:
```yaml
- model_name: gpt-5.4-mini
  litellm_params:
    model: openai/gpt-5.4-mini
    api_key: "..."
  rpm: 50       # Aggregate request limit per minute
  rpd: 5000     # Aggregate request limit per day
  tpm: 100000   # Aggregate token limit per minute
  tpd: 1000000  # Aggregate token limit per day
```

### Layer 2: User-Level Limits (per API key)
Configured per user key in `users.yaml`:
```yaml
- key: "sk-pico-dev-..."
  label: "dev-bot"
  models: [gpt-5.4-mini, gemma-4-31b-it]
  rpm: 100
  rpd: 10000
```

### Storage Strategy: Hybrid In-Memory + SQLite

Not all windows are equal. Per-minute windows (RPM/TPM) are on the hot path — checked on every request and reset every 60s. Per-day windows (RPD/TPD) reset every 24h and tolerate slightly stale reads.

| Window Types | Storage | Why |
|---|---|---|
| **RPM, TPM** | In-memory Python `dict` with `asyncio.Lock` per shard | ~200ns atomic increments. No SQLite write contention. |
| **RPD, TPD** | SQLite `rate_counters` table | Durable. Flushed from memory every 10s only if value changed. |

**Key insight:** SQLite writes serialize globally. By keeping RPM/TPM in memory, we eliminate the hottest SQLite contention entirely. RPD/TPD writes are infrequent enough (a few thousand per day per key-model) that SQLite handles them trivially.

### In-Memory Counter Architecture

```
rate_cache: dict[
    (key_hash, model_name, level, 'rpm' | 'tpm'),  # key
    {
        "window_start": "2026-07-10T14:35:00",
        "count": 42,
        "dirty": True,            # needs SQLite flush
    }
]
```

- Each unique key gets its own `asyncio.Lock` (sharded by hash of the key)
- Increment: acquire lock, check window, increment
- Background task every 10s: iterate dirty entries, UPSERT into SQLite, clear dirty flag
- On startup: load RPD/TPD counters from SQLite (RPM/TPM start at 0 since old windows are already expired)

### Pre-Request Check Flow

```
1. Parse request → extract key_hash, model_name
2. Look up user key's limits, model's limits
3. Check RPM/TPM (in-memory, fast path):
   a. Compute minute window_start
   b. Look up or create in-memory counter
   c. Acquire shard lock → if count >= limit → 429
   d. Atomic increment → release lock
4. Check RPD/TPD (SQLite, slower path):
   a. Compute day window_start
   b. SELECT count FROM rate_counters (read from SQLite directly for RPD/TPD)
   c. If count >= limit → 429
   d. Increment in-memory, mark dirty (flushed to SQLite every 10s)
```

### Streaming Token Tracking: Estimate + Reconcile

TPM and TPD limits require knowing token counts *before* allowing a request. But for streaming completions, total tokens are only known after the final SSE chunk.

**Pre-stream estimation:**
```
prompt_tokens = count with tiktoken (or fallback: len(text) // 4)
estimated_output = request.max_tokens  # from the request body
  OR configurable default (4096)        # if max_tokens not set
estimated_total = prompt_tokens + estimated_output

check: current_count + estimated_total < limit
```

**Post-stream reconciliation:**
After the final SSE `data: [DONE]` chunk (or after the response is fully buffered):
```
actual_tokens = response.usage.total_tokens  # from provider's final chunk
delta = actual_tokens - estimated_total

# Adjust counters:
#   If we over-estimated (delta < 0): decrement TPM/TPD counter
#   If we under-estimated (delta > 0): increment TPM/TPD counter
#   This keeps counters accurate even after a long stream
```

**Why this works:**
- Estimation prevents gross overshoot (a 100k-token stream needs `max_tokens` set high enough)
- Reconciliation corrects the counter so the *next* request has an accurate view
- The edge case (under-estimate followed by another request before reconcile) is bounded by the estimate cap

### What Gets Logged vs. What Gets Counted

| Data | Logged to `usage_log` (immutable) | Counted in `rate_counters` (reconciled) |
|---|---|---|
| Prompt tokens | Yes, actual from response | Yes, adjusted post-stream |
| Completion tokens | Yes, actual from response | Yes, adjusted post-stream |
| Total tokens | Yes, actual from response | Yes, adjusted post-stream |
| Latency | Yes, from wall clock | No |
| Model name | Yes | Implicit via counter key |
| Provider | Yes | No |
| Status code | Yes | No |

---

## 6. API Surface

### Public Endpoints (User API Key)

| Method | Path | Body | Response | Streaming |
|---|---|---|---|---|
| `POST` | `/v1/chat/completions` | ChatCompletionRequest | ChatCompletionResponse | SSE (stream=True) |
| `POST` | `/v1/completions` | CompletionRequest | CompletionResponse | SSE |
| `POST` | `/v1/embeddings` | EmbeddingRequest | EmbeddingResponse | No |
| `GET` | `/v1/models` | — | ModelList (filtered by key) | No |
| `GET` | `/v1/models/{model}` | — | ModelObject | No |
| `POST` | `/v1/images/generations` | — | 501 placeholder | No |
| `POST` | `/v1/audio/transcriptions` | — | 501 placeholder | No |
| `POST` | `/v1/audio/speech` | — | 501 placeholder | No |
| `POST` | `/v1/moderations` | — | 501 placeholder | No |
| `GET` | `/health` | — | `{"status": "ok"}` | No |

### Admin Endpoints (Master API Key)

| Method | Path | Description | Body/Params |
|---|---|---|---|
| `GET` | `/admin/keys` | List all user keys (no hash exposure) | — |
| `POST` | `/admin/keys` | Create a new user key | `{"label": "...", "models": [...], "rpm": ...}` |
| `DELETE` | `/admin/keys/{prefix}` | Revoke a key by prefix | — |
| `PUT` | `/admin/keys/{prefix}/models` | Set model allowlist | `{"models": ["gpt-4", ...]}` (null = all) |
| `PUT` | `/admin/keys/{prefix}/limits` | Set rate limits | `{"rpm": ..., "rpd": ..., "tpm": ..., "tpd": ...}` |
| `GET` | `/admin/usage` | Aggregate usage (all keys + per key) | `?from=&to=&limit=` |
| `GET` | `/admin/usage/top-models` | Top models by token count | `?from=&to=&limit=` |
| `POST` | `/admin/config/reload` | Reload config from disk | — |
| `GET` | `/admin/log` | Admin action audit log | `?limit=` |

---

## 7. Config File Format

### config.yaml (extended from your LiteLLM template)

```yaml
# ==========================================
# GLOBAL SETTINGS
# ==========================================

general_settings:
  master_key: "sk-pico-master-..."     # Admin API key
  db_path: "/data/llm-pico.db"         # Optional, default: next to config

router_settings:
  routing_strategy: simple-shuffle     # How to pick between multiple keys for same model
  num_retries: 2                       # Retries on 429/401/timeout (not 5xx)
  cooldown_time: 45                    # Seconds to cool down a rate-limited key
  allowed_fails: 1                     # Consecutive 429 fails before cooldown
  circuit_breaker:
    enabled: true
    failure_threshold: 3               # Consecutive 5xx before opening circuit
    recovery_timeout: 30               # Seconds in OPEN state before trying again

# ==========================================
# PROVIDER / MODEL DEFINITIONS
# ==========================================

model_list:
  # --- OpenAI ---
  - model_name: gpt-4-turbo
    litellm_params:
      model: openai/gpt-4-turbo
      api_key: "sk-..."
    rpm: 50
    rpd: 5000

  # Multiple keys load-balanced for the same model_name:
  - model_name: gpt-4-turbo
    litellm_params:
      model: openai/gpt-4-turbo
      api_key: "sk-...-key2"
    rpm: 50

  # --- Anthropic ---
  - model_name: claude-3-opus
    litellm_params:
      model: anthropic/claude-3-opus-20240229
      api_key: "sk-ant-..."

  # --- Google Gemini ---
  - model_name: gemini-2-flash
    litellm_params:
      model: gemini/gemini-2.0-flash
      api_key: "AIza..."

  # --- Groq (OpenAI-compat) ---
  - model_name: groq-llama-3
    litellm_params:
      model: groq/llama3-70b-8192
      api_key: "gsk_..."

  # --- OpenRouter ---
  - model_name: orion-deepseek
    litellm_params:
      model: openrouter/deepseek/deepseek-r1
      api_key: "sk-or-..."

  # --- Cloudflare Workers AI ---
  - model_name: cf-llama
    litellm_params:
      model: cloudflare/@cf/meta/llama-3.1-8b-instruct
      api_key: "cfut_..."
      api_base: "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run/"

  # --- NVIDIA NIM ---
  - model_name: nvidia-llama
    litellm_params:
      model: nvidia_nim/meta/llama3-70b-instruct
      api_key: "nvapi-..."

  # --- Zhipu AI ---
  - model_name: glm-4-flash
    litellm_params:
      model: zhipu/glm-4-flash
      api_key: "7cbb..."

  # --- Ollama (local) ---
  - model_name: ollama-llama3
    litellm_params:
      model: ollama/llama3
      api_base: "http://localhost:11434/v1"
      # No api_key needed for local

  # --- llama.cpp (local) ---
  - model_name: local-llama
    litellm_params:
      model: llamacpp/llama-3-8b
      api_base: "http://localhost:8080/v1"

  # --- vLLM (local) ---
  - model_name: vllm-mistral
    litellm_params:
      model: vllm/mistral-7b
      api_base: "http://localhost:8000/v1"
      api_key: "internal-key"   # Optional
```

### users.yaml

```yaml
users:
  - key: "sk-pico-dev-abc123def456"
    label: "development-bot"
    models: null                  # null = access to ALL configured models
    rpm: 100
    rpd: 10000

  - key: "sk-pico-ci-789ghi"
    label: "ci-pipeline"
    models:
      - gpt-4-turbo
      - gemini-2-flash
    tpd: 500000                   # token limit per day

  - key: "sk-pico-test-xyz"
    label: "test-user"
    # No limits = unlimited (within model-level limits)
    # No model list = all models
```

---

## 8. Request Flow (Detailed)

```
┌─────────┐     ┌───────────────────────────────────────────────────────────────┐
│ Client  │────▶│  llm-pico                                                      │
│ (curl/  │     │                                                               │
│  SDK)   │     │  1. Auth: Extract Bearer token, hash it,                      │
│         │     │     look up in SQLite. Fail if not found / inactive           │
│         │     │                                                               │
│         │     │  2. Allowlist: If key has model_allowlist,                    │
│         │     │     check model_name is in it. Fail if not.                   │
│         │     │                                                               │
│         │     │  3. Rate Limit — RPM/TPM: in-memory dict (fast path).         │
│         │     │     If streaming: estimate_total = prompt + max_tokens.       │
│         │     │     If any limit exceeded → 429.                              │
│         │     │                                                               │
│         │     │  4. Rate Limit — RPD/TPD: SQLite counters.                    │
│         │     │     If any limit exceeded → 429.                              │
│         │     │                                                               │
│         │     │  5. Router: Find all entries in model_list matching            │
│         │     │     model_name. Pick one via simple-shuffle.                  │
│         │     │     Resolve provider slug + model string.                     │
│         │     │                                                               │
│         │     │  6. Adapter: Translate OpenAI request → provider              │
│         │     │     format. For OpenAI-compat providers,                      │
│         │     │     this is nearly a passthrough.                             │
│         │     │                                                               │
│         │     │  7. Proxy: Send via per-provider httpx pool (max 5 conns).    │
│         │     │     Handle streaming or buffered response.                    │
│         │     │                                                               │
│         │     │  8. Adapter: Translate provider response → OpenAI             │
│         │     │     format. Extract actual token counts from final chunk.     │
│         │     │                                                               │
│         │     │  9. Logging: Insert usage_log row with actual tokens,         │
│         │     │     latency, status.                                          │
│         │     │                                                               │
│         │     │  10. Reconcile: Adjust TPM/TPD counters with delta between    │
│         │     │      estimated and actual tokens. Mark dirty for SQLite flush.│
│         │     │                                                               │
│         │     │  11. Response: Return OpenAI-format response to               │
│         │     │      client (stream SSE or single JSON).                      │
│         │     │                                                               │
└─────────┘     └───────────────────────────────────────────────────────────────┘
```

---

## 9. Adapter Design

### Base Adapter Interface

```python
class BaseAdapter(ABC):
    provider: str  # e.g., "openai", "anthropic"

    @abstractmethod
    async def translate_request(self, body: dict, model_string: str) -> tuple[dict, dict]:
        """
        Translate OpenAI-format request dict to provider-native format.
        Returns (headers, provider_body).
        """
        ...

    @abstractmethod
    async def translate_response(self, response: httpx.Response) -> dict:
        """
        Translate provider-native response to OpenAI-format response dict.
        """
        ...

    async def handle_stream_chunk(self, chunk: bytes) -> bytes:
        """
        Optional: translate individual SSE chunks for streaming responses.
        Default: pass through unchanged.
        """
        return chunk
```

### Adapter Categories

| Category | Providers | Translation Complexity |
|---|---|---|
| **Passthrough** | OpenAI, Groq, OpenRouter | Zero translation. Body goes as-is. |
| **OpenAI-compat + base URL** | Ollama, llama.cpp, vLLM, NVIDIA | Zero translation. Base URL configurable. |
| **Header transform** | Anthropic | Change `model` prefix, map `messages` format, map `max_tokens`, handle `stop_sequences`. Response maps back. |
| **Full transform** | Gemini, Cloudflare, Zhipu | Significant body restructuring. |

### Stream Handling

For streaming, the adapter's `handle_stream_chunk` processes each SSE `data:` line:
- **Passthrough**: Forward `data: {"choices":[...delta...]}` as-is
- **Anthropic**: Translates Anthropic SSE format → OpenAI SSE format
- **Gemini**: Translates Gemini server-sent events → OpenAI format

---

## 10. Passthrough Architecture (Raw Bytes)

For the 7 OpenAI-compatible providers (OpenAI, Groq, OpenRouter, Ollama, llama.cpp, vLLM, NVIDIA), the request body is forwarded as raw bytes without Pydantic parsing.

### Inbound (client → proxy → provider)

```
POST /v1/chat/completions
Body: raw JSON bytes ──→ read_peek: parse only 3 fields from JSON
  ├── model       → for routing + provider selection
  ├── stream      → for response mode (SSE vs buffered)
  └── max_tokens  → for rate limit reservation

Remaining bytes → stored as raw `body_bytes` → forwarded verbatim to upstream
```

Implementation:
```python
import json

def peek_request(body: bytes) -> tuple[str, bool, int]:
    """Parse only the 3 fields needed. Returns (model, stream, max_tokens)."""
    obj = json.loads(body)
    return (
        obj.get("model", ""),
        obj.get("stream", False),
        obj.get("max_tokens", 4096),
    )
```

### Outbound (provider → proxy → client)

For buffered responses: forward raw response body as-is (no Pydantic parse).
For streaming responses: forward SSE `data:` lines as raw bytes. Intercept only the final `data: [DONE]` and any chunk containing `"usage"` for token reconciliation.

```python
async def proxy_stream(response: httpx.Response, writer: asyncio.StreamWriter):
    """Forward SSE chunks as raw bytes. Extract usage from final chunk."""
    usage = None
    async for chunk in response.aiter_bytes():
        if b'"usage"' in chunk and b'[DONE]' not in chunk:
            # Extract token counts from the final usage chunk
            try:
                data = json.loads(chunk.removeprefix(b"data: "))
                usage = data.get("usage")
            except (json.JSONDecodeError, IndexError):
                pass
        writer.write(chunk)  # Forward raw bytes
    return usage
```

## 11. Anticipatory Reservation (TPM/TPD Burst Protection)

Extended from the rate limiter flow in §5:

```
On request start (under shard lock):
  reservation = prompt_tokens + max(request.max_tokens, 4096)
  available = limit - current_count
  if reservation > available → 429 (not enough budget for worst case)
  counter += reservation   ← reserved atomically

On stream complete (after usage extracted from final chunk):
  actual = response.usage.total_tokens
  unused = reservation - actual
  counter -= unused        ← release what we didn't consume
  # If actual > reservation (max_tokens undersized):
  #   counter += (actual - reservation)  # should be rare
```

**Why this prevents bursts:** Each request locks its worst-case allocation upfront. 10 concurrent requests each reserving 16k tokens need 160k available in the limit. The shard lock serializes the check+reserve atomically — no two requests can both see "enough room" and overshoot.

## 12. Circuit Breaker Design

Per-provider state machine:

```
               ┌────────────────────────────┐
               │           CLOSED            │
               │  (normal operation)         │
               │  error_count = 0            │
               └─────────┬──────────────────┘
                         │ 5xx response
                         │ error_count++
                         │ if error_count >= threshold (3)
                         v
               ┌────────────────────────────┐
               │           OPEN              │
               │  (fail fast)                │
               │  All requests → 502         │
               │  Wait recovery_timeout (30s)│
               └─────────┬──────────────────┘
                         │ timeout expires
                         v
               ┌────────────────────────────┐
               │         HALF_OPEN           │
               │  (test the waters)          │
               │  Try next request upstream  │
               └─────────┬──────────────────┘
                    ┌────┴────┐
                    │         │
                  success    5xx
                    │         │
                    v         v
               CLOSED       OPEN
          (error_count=0)  (reset timer)
```

Implementation in `router.py`:
```python
@dataclass
class CircuitBreakerState:
    provider: str
    state: Literal["CLOSED", "OPEN", "HALF_OPEN"] = "CLOSED"
    error_count: int = 0
    opened_at: float | None = None

    def record_failure(self) -> None:
        self.error_count += 1
        if self.error_count >= 3:
            self.state = "OPEN"
            self.opened_at = time.monotonic()

    def record_success(self) -> None:
        self.state = "CLOSED"
        self.error_count = 0
        self.opened_at = None

    def is_request_allowed(self) -> bool:
        if self.state == "CLOSED":
            return True
        if self.state == "OPEN":
            if time.monotonic() - self.opened_at >= 30:
                self.state = "HALF_OPEN"
                return True
            return False
        # HALF_OPEN: allow exactly one request
        return True
```

---

## 13. Implementation Phases

### Phase 1 — Core Skeleton (this session)

Files to create:
1. `pyproject.toml` — Project config + dependencies
2. `llm_pico/__init__.py` — `__version__ = "0.1.0"`
3. `llm_pico/__main__.py` — `from .cli import main; main()`
4. `llm_pico/cli.py` — Click CLI with options:
   - `--host` (default: `0.0.0.0`)
   - `--port` (default: `4000`)
   - `--config` (default: `./config.yaml`)
   - `--users` (default: `./users.yaml`)
   - `--db` (default: `./llm-pico.db`)
   - `--verbose` / `-v` (flag)
5. `llm_pico/config.py` — Loads YAML, validates structure:
   - `load_config(path)` → `Config` dataclass
   - `load_users(path)` → `list[UserKey]`
   - Validates required fields, model_name uniqueness
   - Auto-detects users.yaml next to config.yaml
6. `llm_pico/db.py` — SQLite init:
   - `get_db()` — async context manager for connection
   - `init_db()` — create tables + indexes
   - WAL mode for performance
7. `llm_pico/models.py` — Pydantic schemas:
   - `ChatCompletionRequest`, `ChatCompletionResponse`
   - `CompletionRequest`, `CompletionResponse`
   - `EmbeddingRequest`, `EmbeddingResponse`
   - `ModelList`, `ModelObject`
   - `ErrorResponse` (for 4xx/5xx)
   - `UserKeyCreate`, `UserKeyResponse`, `KeyList`
   - `UsageStats`, `UsageSummary`
8. `llm_pico/auth.py` — Auth middleware:
   - `verify_api_key(auth_header)` → `UserKey` or raise `401`
   - `check_model_access(user_key, model_name)` → bool or raise `403`
   - Admin key check: `verify_master_key(auth_header, master_key)`
9. `llm_pico/router.py` — Request routing:
   - `resolve_model(model_name)` → `list[ModelEntry]`
   - `pick_entry(entries, strategy="simple-shuffle")` → `ModelEntry`
   - Builds in-memory index from config on startup
   - Key cooldown tracking for 429 handling
10. `llm_pico/ratelimit.py` — Hybrid rate limiter:
    - In-memory cache for RPM/TPM: dict with `asyncio.Lock` per shard, ~200ns increments
    - SQLite `rate_counters` for RPD/TPD: durable, flushed from memory every 10s
    - `check_rate_limit(key_hash, model_name, limits, is_streaming, prompt_tokens)` → pass/429
      - RPM/TPM checked via in-memory cache (fast path)
      - RPD/TPD checked via SQLite (slower path, day-level windows)
      - Streaming: estimate output tokens from `max_tokens` (default 4096)
    - `reconcile_tokens(key_hash, model_name, estimated_total, actual_total)` → adjust counters post-stream
    - Background task: `_flush_dirty_counters()` every 10s
    - Returns rate limit headers: `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`
11. `llm_pico/usage.py` — Usage logger:
    - `log_usage(key_hash, key_prefix, model_name, provider, tokens, latency, status, error)`
    - `get_usage_stats(...)` — aggregate queries for admin API
12. `llm_pico/server.py` — FastAPI app factory:
    - `create_app(config, users)` → `FastAPI`
    - Adds middleware: auth, rate limit, CORS
    - Mounts public routes and admin router
    - In-flight request registry: `set[request_id]` with asyncio events per stream
    - `startup` event: init DB, load config, build router index, start background tasks (rate counter flush, stale counter cleanup)
    - `shutdown` event: close DB, cancel background tasks
13. `llm_pico/admin.py` — Admin API router:
    - All admin endpoints with master key auth
    - Key CRUD: create (returns full key once), list (no hash), revoke, update limits/models
    - Usage queries: aggregate, per-key, top-models
    - Config reload endpoint (graceful drain + restart):
      ```
      POST /admin/config/reload:
        1. Set server state to "draining" (503 to new requests with Retry-After)
        2. Signal active streaming connections to finish naturally
        3. Wait up to N seconds (configurable, default 120) for drain
        4. After timeout: log warning for remaining streams, force-close
        5. exec() the current process (re-reads config on restart)
      ```
    - Audit logging
14. `llm_pico/adapters/__init__.py` — Adapter registry:
    - `get_adapter(provider)` → `BaseAdapter`
    - Maps provider slugs to adapter classes
15. `llm_pico/adapters/base.py` — Abstract base:
    - `BaseAdapter` with interface methods
    - Default `handle_stream_chunk` (passthrough)
16. `llm_pico/adapters/openai.py` — OpenAI adapter:
    - Passthrough — returns body unchanged
    - Handles streaming SSE by forwarding raw lines
17. `llm_pico/placeholder.py` — Placeholder endpoints:
    - Returns `{"error": "Not implemented", "message": "Image generation is not supported yet"}`
    - HTTP 501 status code

### Phase 2 — All Provider Adapters

Create adapter files:
1. `adapters/anthropic.py` — Full translate:
   - Request: map `messages` format (system → `system`, user/assistant messages), map `max_tokens`, `stop`, `temperature`, `top_p`
   - Response: map `content[].text` → `choices[].delta.content`, handle streaming SSE events
   - Handle Anthropic's `content_block_start`, `content_block_delta`, `message_delta` events → OpenAI delta format
2. `adapters/gemini.py` — Full translate:
   - Request: map `messages` → `contents` array (user: `role: user`, assistant: `role: model`), system → `system_instruction`, map generation config
   - Response: extract `candidates[0].content.parts[0].text` → OpenAI format
   - Stream: translate Gemini's server-sent events
3. `adapters/groq.py` — Passthrough (already OpenAI-compat)
4. `adapters/openrouter.py` — Passthrough
5. `adapters/cloudflare.py` — Translate:
   - Request: CF Workers AI uses `{"messages": [...]}` format already close to OpenAI
   - Need to handle model name mapping and account_id in base URL
6. `adapters/nvidia.py` — Passthrough (OpenAI-compat)
7. `adapters/zhipu.py` — Translate to Zhipu format
8. `adapters/ollama.py` — Passthrough (OpenAI-compat API)
9. `adapters/llamacpp.py` — Passthrough (OpenAI-compat API)
10. `adapters/vllm.py` — Passthrough (OpenAI-compat API)

### Phase 3 — Polish

1. Rate limit response headers: `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`
2. Config validation with rich error messages (which field, what's wrong)
3. Dockerfile + docker-compose.yml
4. README.md with:
   - Quick start
   - Configuration reference
   - Provider-specific notes
   - Admin API reference
   - Production deployment guide
6. `config.example.yaml` — Full reference with all providers, comments
7. `users.example.yaml` — Reference user file
8. Test suite:
   - `tests/test_auth.py`
   - `tests/test_ratelimit.py`
   - `tests/test_config.py`
   - `tests/test_router.py`
   - `tests/test_adapters.py` (unit tests with mocked httpx)
   - `tests/test_admin.py`
   - `tests/test_integration.py` (end-to-end with local providers)

---

## 11. CLI Usage

```bash
# Start the proxy
llm-pico

# With options
llm-pico --port 8080 --host 127.0.0.1 --config /etc/llm-pico/config.yaml

# With verbose logging
llm-pico -v

# With custom db path
llm-pico --db /data/llm-pico.db

# Version
llm-pico --version

# Help
llm-pico --help
```

---

## 12. Key Format & Security

- **Master key**: `sk-pico-master-<64-char-hex>` — defined in config.yaml
- **User keys**: `sk-pico-<64-char-hex>` — generated by admin API, stored in users.yaml or DB
- **Storage**: SHA-256 hashed in SQLite. Raw key shown only once on creation.
- **Transport**: Always over HTTPS (terminated by reverse proxy, not the proxy itself)
- **Rate limit key prefix disclosure**: Admin API returns only `key_prefix` (e.g., `sk-pico-a1b2c3`), never the full key
- **Logging**: Full request/response bodies never logged (only tokens, model, latency)

---

## 13. Error Handling

| Scenario | HTTP Status | Response Body |
|---|---|---|
| Missing/wrong API key | 401 | `{"error": "unauthorized", "message": "Invalid API key"}` |
| Key expired | 403 | `{"error": "forbidden", "message": "API key expired"}` |
| Model not in allowlist | 403 | `{"error": "forbidden", "message": "Model not allowed for this key"}` |
| Rate limit exceeded (user) | 429 | `{"error": "rate_limit_exceeded", "message": "User rate limit exceeded", "retry_after": 30}` |
| Rate limit exceeded (model) | 429 | `{"error": "rate_limit_exceeded", "message": "Model rate limit exceeded", "retry_after": 30}` |
| Model not found in config | 404 | `{"error": "model_not_found", "message": "Model 'xyz' not configured"}` |
| Upstream provider error | 502 | `{"error": "upstream_error", "message": "Provider returned 500"}` |
| Upstream timeout | 504 | `{"error": "upstream_timeout", "message": "Provider timed out"}` |
| Unknown provider | 500 | `{"error": "internal_error", "message": "No adapter for provider 'xyz'"}` |
| Placeholder endpoint | 501 | `{"error": "not_implemented", "message": "Not implemented: audio transcription"}` |

---

## 14. Resource Budget (RAM)

Per-provider httpx pools capped at 5 connections each × 11 providers = 55 outbound sockets max. Here are the estimates at different concurrency levels:

| Component | Idle | 10 concurrent | 50 concurrent |
|---|---|---|---|
| Python interpreter | ~10MB | ~10MB | ~10MB |
| FastAPI + uvicorn | ~8MB | ~12MB | ~15MB |
| httpx (per-provider pools) | ~5MB | ~15MB | ~25MB |
| aiosqlite | ~2MB | ~2MB | ~2MB |
| SQLite DB cache | ~1MB | ~10MB | ~10MB |
| Request buffers | 0 | ~20MB | ~60MB |
| Response streaming buffers (4KB each) | 0 | ~5MB | ~15MB |
| In-memory rate cache | ~1MB | ~1MB | ~2MB |
| Provider adapter cache | ~2MB | ~2MB | ~2MB |
| Router index | ~1MB | ~1MB | ~1MB |
| **Total** | **~30MB** | **~78MB** | **~142MB** |

Well under 200MB at all concurrency levels through 50 concurrent requests.

**FD budget:** 11 providers × up to 5 connections = 55 sockets + 1 SQLite DB FD + 1 config FD + misc = ~60 FDs. Default `ulimit -n` is 1024. Headroom for spikes (provider retries creating temp sockets).

---

## 15. Open Questions (Answered)

| Question | Answer |
|---|---|
| Language? | Python (LiteLLM compat) |
| Deployment? | pip package + Docker |
| Database? | SQLite (no external deps) |
| Rate limit scope? | User × Model × Provider |
| Rate limit storage? | RPM/TPM in-memory (dict+Lock), RPD/TPD in SQLite flush every 10s |
| Streaming token tracking? | Estimate + reconcile (pre-check with max_tokens, correct post-stream) |
| httpx connection strategy? | Per-provider pools, max 5 connections each, 55 total |
| Streaming? | Yes, full SSE |
| Admin features? | Keys CRUD, usage stats, config reload (graceful drain) |
| Model access? | Per-user allowlists, default: all |
| Config reload? | Graceful drain + restart (drain active streams with timeout, then restart) |
| Usage tracking? | Everything (tokens, latency, model, provider) |
| Logging? | Quiet default, `--verbose` for debug |
| Anthropic approach? | Accept OpenAI format, translate via adapter |
| Fallback? | Error-class-aware: 429→swap key, 5xx→fail fast. Per-provider circuit breaker (3 fails, 30s recovery) |
| /v1/models? | Yes, with per-user filtering |
| Endpoints? | Full OpenAI surface (placeholders for image/audio) |
| Port? | 4000 |
| Key bootstrap? | users.yaml file |
| TLS? | Reverse proxy only |
| CLI name? | `llm-pico` |
| Key prefix? | `sk-pico-...` |
| Extra providers? | Ollama, llama.cpp, vLLM added |

---

## 16. File Creation Order (Build Sequence)

1. `pyproject.toml` + `llm_pico/__init__.py`
2. `llm_pico/__main__.py` + `llm_pico/cli.py`
3. `llm_pico/models.py` (Pydantic schemas)
4. `llm_pico/db.py` (SQLite init)
5. `llm_pico/config.py` (YAML load + validate)
6. `llm_pico/auth.py` (key lookup + allowlist)
7. `llm_pico/ratelimit.py` (fixed-window counters)
8. `llm_pico/usage.py` (usage logging + stats queries)
9. `llm_pico/router.py` (model resolution + key selection)
10. `llm_pico/adapters/base.py` + `adapters/__init__.py`
11. `llm_pico/adapters/openai.py` (passthrough)
12. `llm_pico/server.py` (FastAPI app, route mounting)
13. `llm_pico/admin.py` (admin endpoints)
14. `llm_pico/placeholder.py` (501 stubs)
15. `config.example.yaml` + `users.example.yaml`
16. `tests/` directory with test files
17. `Dockerfile` + `docker-compose.yml`
18. `README.md`
