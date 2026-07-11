# llm-pico Codebase Walkthrough

A hyper-lightweight LLM proxy designed for Intel Atom D410 (1c/2t @1.66GHz, 2GB DDR2 RAM). Runs 24/7 serving 50 concurrent clients with <200MB RSS. Perfection of few features, zero external dependencies.

---

## High-Level Architecture

```
Client ──► FastAPI ──► Auth (Depends) ──► Rate Limiter ──► Router ──► Adapter ──► Upstream API
              │                              (all in-memory)      │
              └── Admin API ──► SQLite pool (2 conn)              └── httpx (shared per-provider)
                                         │
                                    In-memory LRU Cache
                                    SSE Event Bus (asyncio.Queue)
```

A request flows through:
1. **Auth** — `require_api_key` Depends validates `sk-pico-*` key or master key
2. **Model access check** — verify model is in user's allowlist
3. **Rate limiter** — check RPM/RPD/TPM/TPD/ASH/ASD limits, all in-memory, reserve tokens
4. **Router** — pick a healthy key + provider group (circuit breaker aware)
5. **Capability gating** — verify adapter + model support images/embeddings/etc.
6. **Adapter** — translate OpenAI-format request to provider format, make HTTP call via shared httpx client
7. **Response** — stream or buffer back to client, reconcile rate limit tokens, log usage, add X-RateLimit-* headers

### Lazy Provider Loading

Provider adapters in `providers/` are **lazily loaded on demand**. The `providers/__init__.py` registry does not import all adapters at module load time. Instead, calling `get_adapter("anthropic")` triggers `importlib.import_module("providers.anthropic")` only when that provider is first needed. The `@register` decorator then caches it. The OpenAI adapter is always importable as the fallback for unknown provider slugs.

---

## Project Structure

```
llm-pico/
├── api/                    # FastAPI routes, HTTP layer
│   ├── __init__.py
│   ├── __main__.py         # python -m entry
│   ├── cli.py              # Click CLI + uvicorn launcher
│   ├── server.py           # FastAPI app, lifespan, all proxy route handlers
│   └── admin.py            # Admin REST API (keys, teams, users, stats, logs, config reload)
├── core/                   # Business logic (no HTTP)
│   ├── __init__.py         # version string
│   ├── config.py           # YAML -> dataclasses
│   ├── db.py               # SQLite schema + 2-connection pool
│   ├── auth.py             # API key verification + user/team hierarchy
│   ├── router.py           # Model resolution + circuit breaker
│   ├── ratelimit.py        # In-memory rate limiter with batch SQLite flush
│   ├── usage.py            # Usage logging + cost + stats
│   ├── teams.py            # Team/User CRUD + budget + limit merging
│   ├── events.py           # SSE pub/sub (asyncio.Queue)
│   ├── cache.py            # In-memory LRU request cache (OrderedDict)
│   ├── models.py           # Pydantic schemas
│   └── placeholder.py      # 501 stubs for unsupported endpoints
├── providers/              # Provider adapters (lazy-loaded on demand)
│   ├── __init__.py         # Lazy registry (load adapter only when needed)
│   ├── base.py             # BaseAdapter ABC + shared httpx clients
│   ├── openai.py           # OpenAI passthrough (fallback for unknown slugs)
│   ├── anthropic.py        # Anthropic ↔ OpenAI translation
│   ├── gemini.py           # Gemini ↔ OpenAI translation
│   └── cloudflare.py       # Cloudflare passthrough (prefix-stripping)
├── website/                # Web dashboard SPA
│   ├── __init__.py
│   ├── routes.py           # FastAPI router for static files
│   └── static/
│       └── index.html      # Self-contained SPA (keys, teams, users, budgets, logs)
├── tests/                  # All tests (mirrors core/ structure)
│   ├── conftest.py
│   ├── test_admin_api.py
│   ├── test_router.py
│   ├── test_ratelimit.py
│   ├── test_retry_loop.py
│   ├── test_cost.py
│   ├── test_events.py
│   ├── test_teams.py
│   └── test_cache.py
├── docs/
│   └── understand.md       # Codebase walkthrough
├── config.example.yaml
├── users.example.yaml
├── pyproject.toml
├── Dockerfile
├── PLAN.md
└── README.md
```

---

## Module Deep-Dive

### 1. `api/cli.py` — Entry Point

Uses Click to parse CLI args, resolves config + users paths, loads config, creates FastAPI app, and starts uvicorn.

Key behavior:
- Auto-detects `users.yaml` / `users.yml` next to config
- Defaults DB to `llm-pico.db` in config directory
- Sets logging levels (httpx/aiosqlite/uvicorn.access to WARNING in verbose mode)

### 2. `api/server.py` — Core Proxy Logic

**Routes:**
| Path | Method | Handler | Purpose |
|------|--------|---------|---------|
| `/v1/chat/completions` | POST | `_route_chat_completions` | Main chat endpoint |
| `/v1/completions` | POST | `_route_completions` | Alias for chat (legacy) |
| `/v1/embeddings` | POST | `_route_embeddings` | Embedding endpoint |
| `/v1/models` | GET | `_route_models` | List models (filtered by key allowlist) |
| `/v1/models/{id}` | GET | `_route_single_model` | Get single model |
| `/health` | GET | `_health_check` | Health check |

**Key function: `_proxy_request()`**

This is the heart of the proxy. It:

1. **Tracks in-flight** — increments counter for graceful drain
2. **Retry loop** (`for attempt in range(num_retries + 1)`):
   - Calls `router.resolve()` to get a healthy key/group
   - Checks capability flags (images, embeddings, stt, tts)
   - Creates adapter instance with key + base URL (uses shared httpx client)
   - Checks for image inputs in the request body
   - **First attempt only:** reserves rate limit tokens (both user-level and model-level) — all in-memory, no SQLite on hot path
   - Rewrites the `model` field in the body to the upstream model name
   - Calls `_handle_streaming()` or `_handle_buffered()`
   - On success: `router.record_success()`
   - On failure: `router.record_failure()` with status code
3. **Non-retryable errors** (400, 401, 403, 404, 501) propagate immediately
4. **Retryable errors** (429, 5xx, connection errors) trigger retry with next key/group

**Streaming vs Buffered:**

- `_handle_streaming()` — returns `StreamingResponse`, reads upstream chunks, tracks usage from SSE `data: ...usage...` lines, reconciles rate limit in background task
- `_handle_buffered()` — reads full response, parses JSON for `usage.total_tokens`, checks in-memory LRU cache, returns `Response` with full body

**Graceful drain:** On config reload, `_is_draining = True` causes new requests to get 503. The server waits up to 120s for in-flight requests to complete, then `os.execve` restarts.

### 3. `core/config.py` — Configuration Loading

Loads YAML into dataclasses:

```
Config
├── general_settings: GeneralSettings  (master_key, db_path, retention settings)
├── router_settings: RouterSettings    (routing_strategy, num_retries, cooldown_time, circuit_breaker)
└── model_list: list[ModelEntry]       (model_name, model_params, rpm/rpd/tpm/tpd, capabilities)
```

`ModelEntry` fields:
- `model_name` — the name clients use (e.g. `gpt-5.4-mini`)
- `model_params.model` — the upstream model string with provider prefix (e.g. `openai/gpt-5.4-mini`)
- `model_params.api_key` — the upstream API key
- `model_params.api_base` — optional custom base URL
- `rpm`, `rpd`, `tpm`, `tpd` — model-level rate limits
- `ash`, `asd` — audio seconds per hour/day (for STT/TTS models)
- `images`, `embeddings`, `stt`, `tts` — capability flags

**Environment variable interpolation:** Config values support `${ENV_VAR}` and `${VAR:-default}` syntax via `_resolve_env_vars()`. This allows secrets to be injected from environment variables without storing them in YAML files.

**Retention settings:** `general_settings.retention_days` (default: 30) controls how long usage_log entries are kept. A background task prunes entries older than this daily.

### 4. `core/router.py` — Routing & Circuit Breaker

**Key dataclasses:**

- `CircuitBreaker` — three states: CLOSED → OPEN (after `failure_threshold` failures) → HALF_OPEN (after `recovery_timeout`) → CLOSED (on success)
- `KeyState` — tracks `api_key`, `cooldown_until` (for 429/401/403), `fails` count
- `ProviderGroup` — groups keys by `(provider_slug, api_base)`, has one `CircuitBreaker` per group

**`_build_index()`:**
Groups config entries by `(provider_slug, api_base)`. Multiple entries for the same provider + base end up in the same group (key pooling).

**`resolve(model_name)`:**
1. Gets all `ProviderGroup`s for the model
2. Skips groups with open circuit breakers
3. Skips keys that are in cooldown
4. Returns `(group, key, model_entry)` or `None` if no healthy key/group

**`record_failure()`:**
- 429: cools down the key for `cooldown_time` seconds
- 401/403: cools down for 365 days (marks key as dead)
- 5xx: increments the circuit breaker on the group

**`record_success()`:**
- Resets circuit breaker to CLOSED
- Resets all key fail counts in the group

### 5. `providers/` — Provider Adapters (Lazy-Loaded)

**Adapter Registry** (`providers/__init__.py`):
- `@register("slug")` decorator registers adapter class
- `get_adapter(slug)` looks up adapter by provider slug — triggers lazy import if not yet loaded
- Unregistered slugs fall through to `OpenAIAdapter` (always importable as fallback)

**Lazy loading mechanism:** `get_adapter()` uses `importlib.import_module()` to load adapter modules only when that provider is first requested. This avoids importing all provider dependencies at startup. The `@register` decorator caches the class in a module-level dict after first import.

**BaseAdapter** (`base.py`):
- Creates a **shared** `httpx.AsyncClient` per provider slug via `_get_client()` factory, stored in `_shared_clients` dict. Pool limits: max 10 connections, 5 keepalive, 15s expiry. Timeouts: 300s overall, 10s connect, 10s pool.
- `__init__` accepts `provider_slug` and retrieves the shared client for that provider
- `peek_request(body)` — extracts model, stream, max_tokens from raw JSON
- `has_image_input(body)` — checks for `type: "image_url"` in message content
- `proxy_stream(response)` — helper for SSE streaming implementations
- Capability class vars: `supports_images`, `supports_embeddings`, `supports_stt`, `supports_tts`

**OpenAIAdapter** (`openai.py`):
- Raw-bytes passthrough (no JSON parsing)
- `proxy_request()` POSTs to `<base>/chat/completions`
- Also has `proxy_completions()` and `proxy_embeddings()`
- `supports_images = True`, `supports_embeddings = True`

**AnthropicAdapter** (`anthropic.py`):
- Translates OpenAI format → Anthropic `/v1/messages`:
  - System prompt extracted from `role: "system"` messages
  - Image URLs (base64) converted to Anthropic `image` blocks
  - `messages` list restructured (Anthropic requires user/model alternation)
- Translates Anthropic response → OpenAI format:
  - `content` blocks concatenated into `choices[0].message.content`
  - `usage.input_tokens` + `usage.output_tokens` → `usage.prompt_tokens` + `usage.completion_tokens`
  - `stop_reason` mapped (`end_turn→stop`, `max_tokens→length`)
- Streaming (`proxy_stream`): reads SSE `event: content_block_delta` lines, converts to OpenAI `data: {...}` chunks, tracks usage from `message_start` + `message_delta` events
- `supports_images = True`

**GeminiAdapter** (`gemini.py`):
- Translates OpenAI format → Google AI `generateContent`:
  - System prompt → `systemInstruction`
  - Messages → `contents` with `user`/`model` roles
  - Image URLs (base64) → `inline_data`
  - Generation config: `maxOutputTokens`, `temperature`, `topP`, `stopSequences`
- API key sent as query param `?key=`
- Two endpoints: `generateContent` (non-streaming) and `streamGenerateContent` (streaming)
- Streaming (`proxy_stream`): reads JSON lines, converts each candidate to SSE `data: {...}`, appends `[DONE]`, tracks usage from `usageMetadata`
- `supports_images = True`, `supports_embeddings = True`

**CloudflareAdapter** (`cloudflare.py`):
- OpenAI-compatible raw-bytes passthrough
- Strips `cloudflare/` prefix from model name before forwarding
- `proxy_embeddings()` strips prefix from model field in body
- `supports_embeddings = True` (images not supported)

### 6. `core/ratelimit.py` — In-Memory Rate Limiter

**All 6 window types are in-memory** with periodic batch flush to SQLite:

| Window Type | Storage | Persistence |
|-------------|---------|-------------|
| RPM (requests per minute) | In-memory dict | Batched flush |
| TPM (tokens per minute) | In-memory dict | Batched flush |
| RPD (requests per day) | In-memory dict | Batched flush |
| TPD (tokens per day) | In-memory dict | Batched flush |
| ASH (audio seconds/hour) | In-memory dict | Batched flush |
| ASD (audio seconds/day) | In-memory dict | Batched flush |

**No SQLite on the hot path.** All reads and writes happen in-memory. The `rate_counters` table in SQLite serves only as persistence/restart recovery — counters are loaded from SQLite on startup and flushed back periodically.

**Optimized for single-core Atom:**
- **4 shard locks** instead of 64 — reduces lock contention overhead on a single-core CPU
- **Batched flush** uses a single transaction (reduces COMMIT overhead from per-write to per-batch)
- **`_window_start` is cached** — reduces `time.strftime` calls since window boundaries change infrequently
- **SQLite `rate_counters`** table is now persistence/restart recovery only

**Two levels checked:**
- `user` level — from user key config (rpm_limit, rpd_limit, etc.)
- `model` level — from model entry config (rpm, rpd, etc.)

**Flow:**
1. `check_and_reserve()` checks all 6 window types against the limit + reservation
2. All reads/writes are in-memory dict operations
3. Window start: RPM/TPM/ASH = minute boundary, RPD/TPD/ASD = day boundary
4. `reconcile()` adjusts TPM/TPD/ASH/ASD after actual usage is known (streaming may use fewer tokens than reserved)
5. Background task flushes dirty entries to SQLite in batched transactions every 10s

### 7. `core/auth.py` — Authentication

- `hash_key()` — SHA-256 of raw key
- `verify_api_key()` — checks master key first (compared via `hmac.compare_digest()` for constant-time comparison), then looks up hash in `user_keys` table
- Returns dict with `role` (admin/user), `key_hash`, `key_prefix`, `model_allowlist`, rate limits
- Expired keys (based on `expires_at`) are rejected
- `check_model_access()` — verifies model is in user's allowlist (null allowlist = all models allowed)
- `seed_users()` — inserts users from `users.yaml` at startup (`INSERT OR IGNORE`)

**FastAPI Depends functions:**
- `require_api_key(request)` — extracts Bearer token, verifies, attaches `request.state.user_key`. Used by all proxy routes. No more duplicated auth logic across 6+ route handlers.
- `require_master_key(request)` — same flow but enforces admin role. Used by all admin routes.

### 8. `core/db.py` — Database

**2-connection pool** managed via `asyncio.Queue`. Connections are created on demand and reused.

**`get_db()` context manager:** Acquires a connection from the pool, yields it, and releases it back. If the pool is empty and both connections are in use, the caller awaits until one is returned.

**`close_db()`:** Drains and closes all pool connections cleanly.

**PRAGMA optimizations:**
```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 2000;
PRAGMA mmap_size = 67108864;       -- 64MB
PRAGMA cache_size = -8192;         -- 8MB
PRAGMA temp_store = MEMORY;
PRAGMA foreign_keys = ON;
```

**Tables:**
- `user_keys` — API keys with hashes, labels, allowlists, rate limits, expiry
- `usage_log` — per-request usage records (pruned hourly by background task)
- `rate_counters` — persisted daily counters for restart recovery only (hot path is in-memory)
- `admin_log` — admin action audit trail

### 9. `core/usage.py` — Usage Tracking

- `log_usage()` — inserts a row into `usage_log` with token counts + latency
- `get_usage_stats()` — aggregate stats with optional filtering by key hash / date range
- `get_top_models()` — top N models by total tokens
- Logs are pruned every hour by the background task based on `retention_days` setting (default 30 days)

### 10. `api/admin.py` — Admin REST API

All endpoints use `require_master_key` Depends (Bearer auth). No more `await _require_master(request)` boilerplate.

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/admin/keys` | List all user keys (without private key hashes) |
| POST | `/admin/keys` | Create a new `sk-pico-*` key |
| DELETE | `/admin/keys/{prefix}` | Revoke a key by prefix |
| PUT | `/admin/keys/{prefix}/models` | Update model allowlist for a key |
| PUT | `/admin/keys/{prefix}/limits` | Update rate limits for a key |
| GET | `/admin/budgets` | List all team/user budgets (single SQL query, no N+1) |
| GET | `/admin/usage` | Usage stats (with query filters) |
| GET | `/admin/usage/top-models` | Top models by usage |
| GET | `/admin/log` | Admin audit log |
| GET | `/admin/logs` | Paginated usage logs with filters |
| GET | `/admin/logs/stream` | SSE stream of live usage logs |
| POST | `/admin/config/reload` | Graceful reload (drain + execve) |

Key creation generates `sk-pico-<64-char-hex>` and returns the raw key once (it is not stored in plaintext, only the SHA-256 hash).

**Budget page N+1 fix:** `GET /admin/budgets` uses a single SQL query with JOINs instead of N+1 per-user queries.

---

## Request Flow (Complete Walkthrough)

1. **Client** sends `POST /v1/chat/completions` with `Authorization: Bearer sk-pico-...`
2. **`_route_chat_completions()`** extracts Bearer token, verifies via `require_api_key` Depends
3. If admin key → `user_key = None` (no rate limiting), if user key → check model access
4. Peek at JSON body: extract `model`, `stream`, `max_tokens`
5. **`_proxy_request()`**:
   - Generate request_id, track in-flight
   - Enter retry loop (default: 3 attempts total)
   - `core.router.resolve(model_name)` gets a healthy key/group
   - Check capability gates (images/embeddings/stt/tts)
   - Create adapter instance via `providers.get_adapter(slug)` — uses shared httpx client per provider
   - **First attempt:** `core.ratelimit.check_and_reserve()` for both user + model limits — all in-memory, no SQLite on hot path
   - Rewrite `model` field in body to upstream name
   - Call `_handle_streaming()` or `_handle_buffered()`:
     - Open httpx connection to upstream via shared client pool
     - Stream/buffer response data
     - On completion: `core.usage.log_usage()`, `core.ratelimit.reconcile()`
   - On success: `core.router.record_success()`, return response with X-RateLimit-* headers
   - On failure: `core.router.record_failure()`, close adapter, retry
6. After all retries exhausted: raise last error

**X-RateLimit-* headers** are added to every response: `X-RateLimit-Remaining-RPM`, `X-RateLimit-Remaining-TPM`, etc. These reflect the in-memory counter state after the request.

---

## Configuration

**`config.yaml`** format:
```yaml
router_settings:
  routing_strategy: simple-shuffle
  num_retries: 2
  cooldown_time: 45
  circuit_breaker:
    failure_threshold: 3
    recovery_timeout: 30

general_settings:
  master_key: "${MASTER_KEY}"           # env var interpolation
  db_path: "llm-pico.db"
  retention_days: 30                    # usage_log retention (days)

model_list:
  - model_name: gpt-5.4-mini
    model_params:
      model: openai/gpt-5.4-mini    # "provider/model-name"
      api_key: "${OPENAI_API_KEY}"  # env var interpolation
      api_base: null                 # optional override
    rpm: 100
    rpd: 10000
    ash: 7200                       # audio seconds per hour (STT/TTS)
    asd: 2880                       # audio seconds per day
    images: false
    embeddings: false
    stt: false
    tts: false
```

Environment variables are resolved via `${ENV_VAR}` syntax. Default values use `${VAR:-default}`. This keeps secrets out of YAML files.

**`users.yaml`** format:
```yaml
users:
  - key: "sk-pico-dev-..."
    label: "dev-bot"
    models: null          # null = all models allowed
    rpm: 100
    rpd: 10000
    tpm: null
    tpd: null
```

---

## Key Design Decisions

- **`${ENV_VAR}` config** — secrets stay in environment variables, not YAML files; `${VAR:-default}` for optional overrides
- **Constant-time auth** — master key compared via `hmac.compare_digest()` to prevent timing attacks
- **FastAPI Depends for auth** — `require_api_key` and `require_master_key` eliminate duplicated auth logic across 6+ route handlers
- **Shared httpx clients per provider** — one `AsyncClient` per provider slug (max 10 connections, 5 keepalive, 15s expiry), reused across all adapter instances for that provider
- **SQLite connection pool** — 2-connection pool via `asyncio.Queue` with mmap 64MB, cache 8MB, WAL mode, busy_timeout 2000ms
- **In-memory LRU cache** — OrderedDict with 256 entries, 1h TTL. No disk I/O for cache operations. Replaces SQLite BLOB storage.
- **All rate limit windows in-memory** — 6 window types (RPM, TPM, RPD, TPD, ASH, ASD) all in-memory with batched flush. No SQLite on hot path.
- **4 shard locks** — optimized for single-core Atom (down from 64), reduces lock contention overhead
- **Batched SQLite flush** — single transaction per batch reduces COMMIT overhead
- **Cached `_window_start`** — reduces `time.strftime` calls since window boundaries change infrequently
- **Usage log retention** — 30 days default, pruned hourly by background task
- **X-RateLimit-* headers** — every response includes remaining quota for all applicable windows
- **No Pydantic for request bodies** — only 3 fields parsed from raw JSON (`model`, `stream`, `max_tokens`), avoids full deserialization overhead
- **Circuit breaker per provider group** — not per key, because a provider outage affects all keys for that provider
- **Token reservations** — streaming response tokens are unknown upfront; we reserve `prompt_tokens + max_tokens` and reconcile afterward
- **Provider slug from model string** — first segment before `/` (e.g. `groq/openai/...` → `groq`); unregistered slugs fall through to `OpenAIAdapter`
- **Key pooling** — multiple config entries for the same `(provider_slug, api_base)` share keys in one `ProviderGroup`; `resolve()` skips cooled-down keys within the group
- **Retries pick next key** — `resolve()` is called each retry, so if key A got a 429, key B (if available) is tried next. If all keys in a group are cooled down, the group is skipped, potentially picking a different group
- **No HTTPS** — designed to run behind a reverse proxy (nginx, Caddy, etc.)
- **Lazy provider loading** — adapter modules are imported only when first requested, keeping startup fast and avoiding unnecessary provider SDK imports
- **Separation of HTTP and business logic** — `api/` handles all FastAPI/HTTP concerns; `core/` contains pure business logic with no HTTP imports

**Rejected:**
- **Redis** — adds external dependency, single-core Atom doesn't benefit from distributed cache; in-memory dicts are sufficient for 50 concurrent clients
- **JWT** — unnecessary complexity for a proxy; simple API key hash lookup is fast enough
- **OAuth2** — out of scope for a lightweight proxy
- **Key rotation** — adds operational complexity; compromised keys can be revoked via admin API
- **Soft budgets** — enforcement is sufficient; gradual throttle adds complexity without clear benefit on single-core hardware
- **Failover chains** — circuit breaker + retry loop covers failure scenarios; chained failover adds latency and complexity

---

## Running

```bash
# Direct
llm-pico --config config.yaml --users users.yaml --verbose

# Docker
docker run -d -p 4000:4000 \
  -v /path/to/config.yaml:/app/config.yaml \
  -v /path/to/users.yaml:/app/users.yaml \
  llm-pico:latest
```

---

## Adapter Pattern

To add a new provider:
1. Create `providers/newprovider.py`
2. Subclass `BaseAdapter`, set `provider = "newprovider"`
3. Decorate with `@register("newprovider")`
4. Implement `proxy_request()` and optionally override `proxy_stream()`
5. Set capability flags as class vars
6. The adapter will be automatically lazy-loaded when `get_adapter("newprovider")` is first called — no import needed in `__init__.py`

`BaseAdapter.__init__` now accepts `provider_slug` and uses a shared httpx client retrieved via `_get_client(provider_slug)`. The shared client pool is configured with max 10 connections, 5 keepalive, and 15s expiry — tuned for the Atom D410's single core.

The adapter translates OpenAI-format requests to the provider's format and provider responses back to OpenAI format.

---

## Resource Budget

Measured on Intel Atom D410 (1c/2t @1.66GHz, 2GB DDR2):

| Concurrency | RAM | Startup |
|-------------|-----|---------|
| Idle | ~54MB | ~1.9s |
| 10 concurrent | ~91MB | — |
| 50 concurrent | ~127MB | — |

Well within the 200MB RSS target at maximum expected load. The in-memory LRU cache (256 entries) adds ~2MB. The 2-connection SQLite pool with mmap 64MB contributes ~70MB virtual but only pages in as needed. Shared httpx clients per provider avoid per-request connection overhead.
