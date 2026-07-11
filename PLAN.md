# llm-pico — Full Implementation Plan

> **Note**: This is a **living document** reflecting the *actual* current state of the codebase, not an aspirational design doc. It is updated as the project evolves.

---

## Codebase Restructure (Current)

The codebase has been restructured into a clean layered architecture with three main packages:

- **`api/`** — All HTTP/FastAPI concerns (routes, middleware, CLI entry point)
- **`core/`** — Pure business logic (no HTTP imports, just functions that take params and return values)
- **`providers/`** — Provider adapters, lazily loaded on demand via `importlib`
- **`website/`** — Web dashboard SPA served at `/admin/dashboard`
- **`tests/`** — All 67 tests, mirroring the `core/` and `api/` structure

Key principle: `core/` never imports FastAPI. `api/` imports from `core/` and `providers/`. Provider adapters are loaded only when first needed, keeping startup fast.

---

## What We Deliberately Reject

These are not TODOs. They are design decisions. Each was considered and rejected.

| Feature | Why We Rejected It | When We'd Reconsider |
|---|---|---|
| **Redis** | Violates zero-dependency design. In-memory dicts + SQLite batched flush handle 50 concurrent clients fine. Redis is the first thing people reach for and the last thing they need at this scale. | 500+ concurrent clients, or if single-process persistence becomes a hard requirement |
| **PostgreSQL** | SQLite is the correct database for a single-process proxy with ≤50 clients. WAL mode + connection pooling covers our write contention needs. PostgreSQL adds an entire server process, connection auth, and config management for zero benefit at our scale. | 1000+ clients, or if we need concurrent multi-process writes |
| **JWT / OAuth2 / SSO** | Master key is sufficient for single-team deployments. JWT adds token expiry, refresh flows, and cryptographic verification — none of which solve a problem we actually have. OAuth2/SSO are enterprise features for enterprise deployments. | When we have multi-team / multi-org deployments that need federated identity |
| **Multi-level failover chains** | We have 4 providers. A single failover level (pick a healthy alternate model) covers the realistic failure modes. Chained failover adds complexity proportional to N² for negligible uptime gain at our provider count. | 10+ providers, or if uptime SLOs demand it |
| **Key rotation with grace periods** | `${ENV_VAR}` config syntax solves the real problem — rotating secrets without code changes. Grace periods add complexity around two valid keys existing simultaneously, which is a security liability, not a feature. | Multi-team deployments where key rotation is a compliance requirement |
| **Soft budgets** | Hard block is simpler and safer. When a user hits their monthly budget, they get a 429. No partial degradation, no "soft warnings" that get ignored. Predictable behavior is more valuable than "helpful" behavior. | Enterprise clients who specifically require graduated throttling |
| **100+ provider adapters** | 4 dedicated adapters + custom (OpenAI-compat fallback) covers 95% of use cases. Most LLM providers either speak OpenAI-compatible API or have a small translation surface. Abstraction for abstraction's sake creates maintenance burden. | 10+ providers with genuinely different APIs that can't use OpenAI-compat fallback |
| **Provider transformation abstraction** | Premature with 4 providers. Each provider has 1-2 unique quirks that don't justify a formal abstraction layer. Adding a "ProviderTransformer" interface now would be YAGNI. | 10+ providers, or when 3+ providers share the same translation pattern |

---

## 1. Overview

**llm-pico** is a hyper-lightweight (<200MB RSS) LLM proxy designed to run 24/7 on **commodity/legacy hardware** — Intel Atom D410 (1c/2t @1.66GHz), 2GB DDR2 RAM. It handles 50 concurrent clients at 1M tokens throughput with zero external dependencies beyond Python 3.11+.

The design philosophy is **perfection of few features over abundance of mediocre ones**. We are a knife, not a Swiss Army knife. Every feature that requires an external dependency (Redis, PostgreSQL, OAuth2) is rejected on principle.

### Core Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Target hardware | Intel Atom D410, 2GB DDR2, 24/7 uptime | Real constraints breed real engineering |
| Max concurrent clients | 50 | Saturate the target hardware without OOM |
| Memory budget | <200MB RSS | Leave headroom for OS + swap on 2GB system |
| Design philosophy | Perfection over options, knife not Swiss Army knife | Every feature must justify its existence against the zero-dep principle |
| Language | Python 3.11+ | LiteLLM compat, rich ecosystem |
| HTTP framework | FastAPI | Async, OpenAI-compat docs, Pydantic validation built-in |
| HTTP client | httpx | Async, shared client per provider (not per request), connection pooling |
| Database | SQLite + aiosqlite | Zero-dependency persistence, connection pool (2 connections), 64MB mmap, 8MB cache, temp_store=MEMORY |
| Cache | In-memory LRU (256 entries) | Replaces SQLite BLOB cache — no disk I/O on hot path |
| Rate limiting | All windows in-memory with periodic flush | No SQLite on hot path. Batched single-transaction flush every 60s for daily counters |
| Config secrets | `${ENV_VAR}` syntax | Secrets stay in environment, never in YAML files |
| Auth | FastAPI `Depends()` | No duplicated auth blocks — single dependency injection point |
| Provider adapters | Custom (no SDKs), lazy-loaded | Tiny dependency footprint, full control, imports only when needed |
| Passthrough parsing | Raw bytes for OpenAI-compat providers | Parse only model/stream/max_tokens (3 fields), forward raw JSON. No Pydantic overhead on hot path. |
| Token burst protection | Anticipatory reservation | Reserve `prompt+max_tokens` upfront per stream. Release unused post-stream. Prevents concurrent burst overshoot. |
| Provider outage handling | Error-class-aware retry + circuit breaker + failover | 429 → cool down key. 400/401/403/404/501 → propagate immediately. 5xx → circuit breaker (3 consecutive = open 30s). All retries exhausted → try failover_model (1 level, no chaining). |
| Capability gating | Explicit YAML flags only (images, embeddings, stt, tts) | Both model-level flag + adapter-level class var; no auto-detection |
| Audio (STT/TTS) | Full route handlers with retry loop | multipart form for STT, JSON for TTS; ash/asd rate limits |
| Teams & Users | Hierarchy: Team → User → API Key | Each level has own rate limits; per-user monthly budget only (no team-level budget) |
| Cost tracking | per-model `cost_per_1m_input` + `cost_per_1m_output` (USD) | Computed at request time, logged to `usage_log.cost_usd`; blended fallback |
| Live log stream | Internal asyncio.Queue pub/sub + SSE endpoint | Zero external deps; per-subscriber backpressure (maxsize 256, drop oldest) |
| Exact-match caching | In-memory LRU, SHA-256 body key | Per-model opt-in (`can_cache: true`), TTL 1h, skips upstream on hit |
| Usage log retention | 30-day auto-prune | Prevents unbounded disk growth on 24/7 deployments |
| Rate limit headers | `X-RateLimit-*` on every response | Clients can self-throttle without guessing |
| TLS | Reverse proxy only (nginx/caddy) | Keep the proxy simple |
| Deployment | pip package + Docker | Both options |
| Port | 4000 | Default listen port |
| Key format | `sk-pico-<64-hex>` | Distinct from raw provider keys |
| Config reload | Graceful drain + `os.execve()` restart | Track active streams via request registry, drain with 120s timeout, then exec |
| Architecture | Layered (`api/` + `core/` + `providers/`) | Clear separation of HTTP layer from business logic; easy to test core in isolation |
| Authentication | Master key + hashed user keys | No Redis, no JWT, no OAuth2, no SSO — by design |

### Provider Matrix

All providers are **lazy-loaded** via `providers/__init__.py`. Calling `get_adapter("anthropic")` triggers `importlib.import_module()` only on first use. OpenAI adapter is always importable as fallback.

| Provider Slug | Adapter | Images | Embeddings | STT | TTS | Adapts From/To |
|---|---|---|---|---|---|---|
| `openai` | OpenAIAdapter | ✅ | ✅ | ✅ | ✅ | Passthrough |
| `anthropic` | AnthropicAdapter | ✅ | ❌ | ❌ | ❌ | Anthropic ↔ OpenAI |
| `gemini` | GeminiAdapter | ✅ | ✅ | ❌ | ❌ | Gemini ↔ OpenAI |
| `cloudflare` | CloudflareAdapter | ❌ | ✅ | ❌ | ❌ | Passthrough (prefix-stripping) |
| `groq` | OpenAIAdapter (fallback) | ❌ | ❌ | ✅ | ✅ | Passthrough (needs api_base) |
| `zhipu` | OpenAIAdapter (fallback) | ❌ | ❌ | ❌ | ❌ | Passthrough |
| `openrouter` | OpenAIAdapter (fallback) | ❌ | ❌ | ❌ | ❌ | Passthrough |
| `nvidia_nim` | OpenAIAdapter (fallback) | ❌ | ❌ | ❌ | ❌ | Passthrough |

Unregistered slugs fall through to `OpenAIAdapter` via `get_adapter(slug) or OpenAIAdapter` in `api/server.py`.

---

## 2. Project Structure

```
/home/2kfi/.llm-pico/
├── pyproject.toml              # PEP 621 project metadata + deps
├── README.md                   # Quick start, config reference, admin API docs
├── PLAN.md                     # This file — living implementation plan
├── Dockerfile                  # Multi-stage Docker build (python:3.12-slim, 168MB)
├── .dockerignore
├── .gitignore
├── config.example.yaml         # Reference config with all 7 providers (placeholder keys)
├── users.example.yaml          # Reference user keys file
│
├── api/                        # FastAPI routes, HTTP layer
│   ├── __init__.py
│   ├── __main__.py             # python -m entry
│   ├── cli.py                  # Click CLI + uvicorn launcher
│   ├── server.py               # FastAPI app, lifespan, all proxy route handlers
│   └── admin.py                # Admin REST API (keys, teams, users, stats, logs, config reload)
│
├── core/                       # Business logic (no HTTP)
│   ├── __init__.py             # version string
│   ├── config.py               # YAML -> dataclasses, ${ENV_VAR} interpolation
│   ├── db.py                   # SQLite schema + connection pool
│   ├── auth.py                 # API key verification + user/team hierarchy
│   ├── router.py               # Model resolution + circuit breaker
│   ├── ratelimit.py            # Hybrid rate limiter (in-memory + periodic SQLite flush)
│   ├── usage.py                # Usage logging + cost + stats + 30d retention
│   ├── teams.py                # Team/User CRUD + budget + limit merging
│   ├── events.py               # SSE pub/sub (asyncio.Queue)
│   ├── cache.py                # In-memory LRU exact-match request cache
│   ├── models.py               # Pydantic schemas
│   └── placeholder.py          # 501 stubs for unsupported endpoints
│
├── providers/                  # Provider adapters (lazy-loaded on demand)
│   ├── __init__.py             # Lazy registry (load adapter only when needed)
│   ├── base.py                 # BaseAdapter ABC
│   ├── openai.py               # OpenAI passthrough (fallback for unknown slugs)
│   ├── anthropic.py            # Anthropic ↔ OpenAI translation
│   ├── gemini.py               # Gemini ↔ OpenAI translation
│   └── cloudflare.py           # Cloudflare passthrough (prefix-stripping)
│
├── website/                    # Web dashboard SPA
│   ├── __init__.py
│   ├── routes.py               # FastAPI router for static files
│   └── static/
│       └── index.html          # Self-contained SPA (keys, teams, users, budgets, logs)
│
├── tests/                      # All 67 tests (mirrors core/ structure)
│   ├── conftest.py             # 4 fixtures: config, single_model, multi_key, dual_group
│   ├── test_router.py          # 7 tests: resolve, cooldown, circuit breaker FSM
│   ├── test_ratelimit.py       # 10 tests: RPM, reservations, reconcile, user+model levels, ASH/ASD
│   ├── test_retry_loop.py      # 5 tests: 5xx retry, exhaustion, non-retryable, failover, no-chain
│   ├── test_cost.py            # 5 tests: both rates, blended, null, zero
│   ├── test_events.py          # 3 tests: emit, multiple subs, backpressure
│   ├── test_teams.py           # 11 tests: CRUD, limits, budget, cascade, hierarchy
│   ├── test_cache.py           # 4 tests: set/get, key uniqueness, expiry, clear
│   └── test_admin_api.py       # 22 tests: HTTP-level admin API tests
│
├── docs/
│   └── understand.md           # Full codebase walkthrough
│
├── config-litellm.yml          # UNTRACKED — active config (real API keys — do NOT commit)
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
    "httpx>=0.28.0",      # pool_timeout kwarg renamed to pool= in >=0.28
    "aiosqlite>=0.20.0",
    "click>=8.1.0",
    "pyyaml>=6.0",
    "pydantic>=2.0",
]
```

No provider SDKs. All upstream communication via `httpx.AsyncClient`.

### httpx Pool Configuration

One shared `httpx.AsyncClient` per provider, reused across all requests. No per-request client allocation.

```python
limits=httpx.Limits(
    max_connections=10,
    max_keepalive_connections=5,
    keepalive_expiry=15.0,
)
timeout=httpx.Timeout(300.0, connect=10.0, pool=5.0)
```

**CRITICAL:** `httpx >= 0.28` renamed `pool_timeout=` → `pool=`. The old keyword raises `TypeError`.

### SQLite Connection Pool

```python
# 2 connections: one for reads (usage stats, cache lookups), one for writes (rate counters, usage log)
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA cache_size=-8000;       # 8MB page cache
PRAGMA mmap_size=67108864;     # 64MB memory-mapped I/O
PRAGMA temp_store=MEMORY;       # temp tables in RAM
PRAGMA busy_timeout=5000;       # 5s wait on lock contention
```

---

## 4. SQLite Schema

File: auto-created at `{db_dir}/llm-pico.db` (default: next to config, overridable with `--db`).

### Current Tables

```sql
CREATE TABLE IF NOT EXISTS user_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash        TEXT    NOT NULL UNIQUE,     -- SHA-256 of raw key
    key_prefix      TEXT    NOT NULL,             -- First 12 chars for display
    label           TEXT,
    is_active       INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,             -- ISO-8601
    expires_at      TEXT,
    model_allowlist TEXT,                         -- JSON array or NULL (all)
    rpm_limit       INTEGER,
    rpd_limit       INTEGER,
    tpm_limit       INTEGER,
    tpd_limit       INTEGER,
    user_id         INTEGER,                     -- FK to users(id) — NULL for legacy keys
    FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE TABLE IF NOT EXISTS usage_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash          TEXT    NOT NULL,
    key_prefix        TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,
    provider          TEXT    NOT NULL,
    request_id        TEXT    NOT NULL,             -- UUID generated by proxy
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    status_code       INTEGER NOT NULL,
    error             TEXT,
    cost_usd          REAL,                        -- Computed at request time, NULL if model has no pricing
    created_at        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_key   ON usage_log(key_hash);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_log(model_name);
CREATE INDEX IF NOT EXISTS idx_usage_time  ON usage_log(created_at);

CREATE TABLE IF NOT EXISTS rate_counters (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash      TEXT    NOT NULL,
    model_name    TEXT    NOT NULL,
    level         TEXT    NOT NULL CHECK(level IN ('user', 'model')),
    window_type   TEXT    NOT NULL CHECK(window_type IN ('rpd', 'tpd', 'asd')),
    window_start  TEXT    NOT NULL,                  -- ISO-8601 truncated to minute or day
    count         INTEGER NOT NULL DEFAULT 0,
    UNIQUE(key_hash, model_name, level, window_type, window_start)
);

CREATE TABLE IF NOT EXISTS admin_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    action      TEXT    NOT NULL,
    actor_hash  TEXT    NOT NULL,
    details     TEXT,
    created_at  TEXT    NOT NULL
);
```

### New Tables: Teams & Users

```sql
CREATE TABLE IF NOT EXISTS teams (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    description     TEXT,
    is_active       INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL,
    model_allowlist TEXT,                       -- JSON array or NULL
    rpm_limit       INTEGER,                     -- Team-level limit override
    rpd_limit       INTEGER,
    tpm_limit       INTEGER,
    tpd_limit       INTEGER
    -- NOTE: No monthly_budget_usd — per-user budget only
);

CREATE TABLE IF NOT EXISTS users (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    team_id           INTEGER NOT NULL REFERENCES teams(id),
    email             TEXT    NOT NULL UNIQUE,
    name              TEXT    NOT NULL,
    is_active         INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT    NOT NULL,
    model_allowlist   TEXT,                      -- JSON array or NULL
    rpm_limit         INTEGER,                    -- User-level limit override
    rpd_limit         INTEGER,
    tpm_limit         INTEGER,
    tpd_limit         INTEGER,
    monthly_budget_usd REAL                      -- Per-user monthly cap (USD); NULL = no cap
);
```

---

## 5. Rate Limiting — Three Layers × Six Window Types

### Three Layers (with Teams)

1. **Model-Level Limits** — Configured per `model_list` entry in config YAML:
   ```yaml
   - model_name: gpt-5.4-mini
     rpm: 50
     rpd: 5000
   ```

2. **User-Level Limits** — Resolved from hierarchy (most restrictive wins):
   ```
   effective_limit = min(
       key.row.rpm_limit,        # from user_keys
       user.row.rpm_limit,       # from users table (NULL = inherit)
       team.row.rpm_limit        # from teams table (NULL = unlimited)
   )
   ```

3. **Per-User Monthly Budget** — Checked on every request (individual cap only, no team-level budget):
   ```
   user_spend = SELECT SUM(cost_usd) FROM usage_log
                WHERE key_hash IN (SELECT key_hash FROM user_keys WHERE user_id = ?)
                AND created_at >= start_of_month()
   if user_spend + this_request_cost > user.monthly_budget_usd → 429
   ```

### Six Window Types — All In-Memory with Periodic Flush

| Window | Meaning | Storage | retry_after |
|---|---|---|---|
| **RPM** | Requests/minute | In-memory dict (no shards) | 60s |
| **TPM** | Tokens/minute | In-memory dict (no shards) | 60s |
| **RPD** | Requests/day | In-memory dict, batched flush every 60s | 86400s |
| **TPD** | Tokens/day | In-memory dict, batched flush every 60s | 86400s |
| **ASH** | Audio secs/hour | In-memory dict (no shards) | 3600s |
| **ASD** | Audio secs/day | In-memory dict, batched flush every 60s | 86400s |

**All daily counters are now in-memory with periodic SQLite flush.** The `rate_counters` table exists for persistence/restart recovery only. Single-transaction batch flush eliminates per-entry COMMIT overhead — all dirty daily counters are written in one `BEGIN IMMEDIATE ... COMMIT` block every 60 seconds. Hot-path rate checks never touch SQLite.

---

## 6. API Surface

### Public Endpoints (User or Master API Key)

Every response includes `X-RateLimit-*` headers:

| Method | Path | Handler | Notes |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | `_route_chat_completions` | Streaming (SSE) or buffered |
| `POST` | `/v1/completions` | `_route_completions` | Alias to chat |
| `POST` | `/v1/embeddings` | `_route_embeddings` | Passthrough |
| `POST` | `/v1/audio/transcriptions` | `_route_audio_transcriptions` | Multipart form STT |
| `POST` | `/v1/audio/speech` | `_route_audio_speech` | JSON body TTS |
| `GET` | `/v1/models` | `_route_models` | Filtered by user allowlist |
| `GET` | `/v1/models/{model_id}` | `_route_single_model` | Single model lookup |
| `GET` | `/health` | `_health_check` | `{"status": "ok"}` |
| `POST` | `/v1/images/generations` | placeholder | 501 |
| `POST` | `/v1/images/edits` | placeholder | 501 |
| `POST` | `/v1/images/variations` | placeholder | 501 |
| `POST` | `/v1/audio/translations` | placeholder | 501 |
| `POST` | `/v1/moderations` | placeholder | 501 |

### Admin Endpoints (Master API Key)

| Method | Path | Phase | Description |
|---|---|---|---|
| `GET` | `/admin/keys` | Existing | List all user keys |
| `POST` | `/admin/keys` | Existing | Create a new user key (returns raw key once) |
| `DELETE` | `/admin/keys/{prefix}` | Existing | Revoke a key (soft-delete) |
| `PUT` | `/admin/keys/{prefix}/models` | Existing | Set model allowlist |
| `PUT` | `/admin/keys/{prefix}/limits` | Existing | Set rate limits |
| `PUT` | `/admin/keys/{prefix}/user` | Teams | Assign key to a user |
| `GET` | `/admin/usage` | Existing | Aggregate usage stats (now includes `total_cost_usd`) |
| `GET` | `/admin/usage/top-models` | Existing | Top models by token count |
| `GET` | `/admin/log` | Existing | Admin action audit log |
| `POST` | `/admin/config/reload` | Existing | Graceful drain + restart |
| `GET` | `/admin/stats/costs` | Cost | Cost breakdown by user/model/date range |
| `POST` | `/admin/teams` | Teams | Create a team |
| `GET` | `/admin/teams` | Teams | List all teams |
| `GET` | `/admin/teams/{id}` | Teams | Team details + month-to-date spend |
| `PUT` | `/admin/teams/{id}/limits` | Teams | Set team-level rate limits |
| `PUT` | `/admin/teams/{id}/budget` | Teams | Set team monthly budget cap |
| `POST` | `/admin/teams/{id}/users` | Teams | Create user under team |
| `GET` | `/admin/teams/{id}/users` | Teams | List users in team |
| `GET` | `/admin/teams/{id}/usage` | Teams | Team usage + cost breakdown |
| `PUT` | `/admin/users/{id}/limits` | Teams | Set user-level rate limits |
| `PUT` | `/admin/users/{id}/budget` | Teams | Set user monthly budget cap |
| `GET` | `/admin/users/{id}/usage` | Teams | Per-user usage + cost |
| `GET` | `/admin/logs/stream` | SSE | Live SSE event stream of completed requests |
| `GET` | `/admin/logs` | SSE | Minimal HTML dashboard (embedded) |

---

## 7. Config File Format

### Config YAML (`config.yaml` / `config.yml`)

Both `.yaml` and `.yml` extensions work. Auto-detected next to config file.

Secrets use `${ENV_VAR}` syntax — the value is interpolated from the environment at load time. This keeps secrets out of YAML files entirely.

```yaml
general_settings:
  master_key: "${MASTER_KEY}"     # Resolved from env at startup

router_settings:
  routing_strategy: simple-shuffle
  num_retries: 2
  cooldown_time: 45
  allowed_fails: 1
  circuit_breaker:
    enabled: true
    failure_threshold: 3
    recovery_timeout: 30

model_list:
  - model_name: gpt-5.4-mini
    model_params:
      model: openai/gpt-5.4-mini
      api_key: "${OPENAI_API_KEY}"
      api_base: "https://..."
    rpm: 50
    rpd: 5000
    tpm: 100000
    tpd: 1000000
    ash: 7200
    asd: 2880
    images: false
    embeddings: true
    stt: true
    tts: true
    failover_model: "gemma-4-31b-it"
    cost_per_1m_input: 15.00          # $15 per 1M prompt tokens (USD)
    cost_per_1m_output: 60.00         # $60 per 1M completion tokens (USD)

  # Blended rate fallback (set only one, or neither):
  - model_name: gemini-3-flash-preview
    model_params:
      model: gemini/gemini-3-flash-preview
      api_key: "${GEMINI_API_KEY}"
    cost_per_1m_output: 0.15          # $0.15 per 1M total tokens (blended)

  # Key pooling: same model_name + different api_key = load-balanced
  - model_name: nvidia-nemotron-...
    model_params:
      model: openrouter/nvidia/...
      api_key: "${OPENROUTER_API_KEY}"
      api_base: "https://openrouter.ai/api/v1"
    rpd: 50
```

### `${ENV_VAR}` Interpolation Rules

- `${VAR_NAME}` → replaced with `os.environ["VAR_NAME"]` at config load time
- If the env var is not set and no default is provided, startup fails with a clear error
- Syntax: `${VAR:-default}` for optional values with defaults
- Any YAML value can use this syntax: keys, api_base, master_key, etc.

### Cost Pricing Rules

- `cost_per_1m_input` and `cost_per_1m_output` are both optional floats (USD)
- If both set: `cost = (prompt/1M * input) + (completion/1M * output)`
- If only `cost_per_1m_output` set: used as blended rate on `total_tokens`
- If only `cost_per_1m_input` set: used as blended rate on `total_tokens`
- If neither set: `cost_usd` logs as `NULL`

### Users YAML (`users.yaml` / `users.yml`)

```yaml
- key: "sk-pico-dev-abc123def456"
  label: "development-bot"
  models: null                 # null = all models
  rpm: 100
  rpd: 10000

- key: "sk-pico-ci-789ghi"
  label: "ci-pipeline"
  models: [gpt-5.4-mini, gemini-2-flash]
  tpd: 500000
```

---

## 8. Request Flow (Detailed)

```
Client → llm-pico:

1. Auth (FastAPI Depends)
   - Extract "Bearer <key>" from Authorization header
   - Master key check first (hmac.compare_digest)
   - SHA-256 hash user key, look up in SQLite user_keys
   - Reject if not found, inactive, or expired
   - If user_key.user_id is set: resolve user + team hierarchy

2. Model Access
   - Resolve effective model_allowlist: min(key, user, team)
   - Verify model_name is in effective allowlist
   - Returns 403 if not allowed

3. Body Peek (for non-audio routes)
   - Parse JSON → extract model, stream, max_tokens

4. Cache Check (non-streaming, can_cache=true models only)
   - SHA-256 hash of raw body → lookup in-memory LRU cache
   - If hit and not expired → return cached response body immediately

5. Budget Check (per-user only, no team budget)
   - If user has monthly_budget_usd:
     SELECT SUM(cost_usd) FROM usage_log WHERE key_hash IN (user's keys) ...
     If (spend + estimated_cost) > budget → 429
   - Estimated cost = compute_cost(prompt, max_tokens, cost_in, cost_out)

6. Rate Limit — RPM/TPM/ASH (in-memory, fast path)
   - Resolve effective limits: min(key, user, team)
   - Check count + reservation ≤ limit
   - If exceeded → 429 with retry_after + X-RateLimit-* headers

6. Rate Limit — RPD/TPD/ASD (in-memory, periodic flush)
   - Same limit resolution
   - In-memory counter check, UPSERT to dirty dict
   - Background task flushes dirty counters to SQLite every 60s

7. Router Resolution
   - resolve(model_name): pick healthy key/group via simple-shuffle

8. Capability Gating + Adapter Selection
   - get_adapter(slug) lazy-loads provider module on first use

9. Upstream Request + Error Handling + Failover
   (identical to current flow)

10. Response Handling
    - Streaming: StreamingResponse with SSE, background reconcile task
    - Buffered: Read full body

11. Cost Computation
    - Look up model_entry pricing (cost_per_1m_input, cost_per_1m_output)
    - compute_cost(prompt_tokens, completion_tokens, ...) → cost_usd

12. Usage Logging
    - INSERT INTO usage_log with cost_usd

13. SSE Event Emission
    - emit({key_prefix, model, prompt_tokens, completion_tokens,
            latency_ms, status, cost_usd})
    - Fans out to all subscribed asyncio.Queue subscribers

14. Rate Limit Reconciliation (streaming only)
    - Adjust TPM/TPD counters with delta
```

---

## 9. Adapter Design

### Adapter Catalog

| Adapter | Location | Lines | Type | Special |
|---|---|---|---|---|
| `OpenAIAdapter` | `providers/openai.py` | 56 | Passthrough (raw bytes) | Supports chat, completions, embeddings, audio STT, audio TTS |
| `AnthropicAdapter` | `providers/anthropic.py` | 198 | Full translate | Images via base64, 1:2 SSE event mapping |
| `GeminiAdapter` | `providers/gemini.py` | 199 | Full translate | `generateContent` API, `?key=` auth, embeddings |
| `CloudflareAdapter` | `providers/cloudflare.py` | 43 | Passthrough | Strips `cloudflare/` prefix, `/ai/v1` base path |

All adapters are lazy-loaded via `providers/__init__.py` using `importlib.import_module()`. The `@register` decorator caches the class after first import.

---

## 10. Circuit Breaker Design

```
CLOSED → (3 consecutive 5xx) → OPEN → (30s timeout) → HALF_OPEN
                                                          |
                                                    success → CLOSED
                                                      5xx → OPEN
```

---

## 11. Error Handling

| Scenario | HTTP Status | Retry? |
|---|---|---|
| Missing/wrong API key | 401 | No |
| Key expired / user inactive / team inactive | 403 | No |
| Model not in allowlist (key/user/team) | 403 | No |
| Monthly budget exceeded (user or team) | 429 | No (immediate 429) |
| Model not in config | 404 | No |
| Model lacks capability | 400 | No |
| Rate limit exceeded | 429 | Yes — cool down key, try next |
| Upstream 5xx | 502 | Yes — circuit breaker, try next key |
| Upstream timeout | 504 | Yes — try next key |
| Connection error | 502 | Yes — try next key |
| All retries exhausted | 502 | Try failover_model (1 level) |
| Placeholder endpoint | 501 | No |

---

## 12. Live SSE Log Stream (`core/events.py`)

### Design

A lightweight pub/sub bus using `asyncio.Queue`. No external dependencies.

```python
# core/events.py

import asyncio
import json
from typing import Any

_subs: set[asyncio.Queue] = set()

def emit(event: dict[str, Any]) -> None:
    """Fan out to all subscribers. Drop oldest entry on slow consumers."""
    payload = json.dumps(event)
    for q in list(_subs):
        try:
            q.put_nowait(payload)
        except asyncio.QueueFull:
            try:
                q.get_nowait()
                q.put_nowait(payload)
            except (asyncio.QueueEmpty, asyncio.QueueFull):
                pass

def subscribe() -> asyncio.Queue:
    q: asyncio.Queue = asyncio.Queue(maxsize=256)
    _subs.add(q)
    return q

def unsubscribe(q: asyncio.Queue) -> None:
    _subs.discard(q)
```

### Integration Points

One line inserted after every successful `log_usage()` call (8 call sites across `api/server.py`):

```python
from core.events import emit
emit({
    "ts": created_at,
    "key_prefix": key_prefix,
    "model": model_name,
    "provider": slug,
    "prompt_tokens": prompt_tokens,
    "completion_tokens": completion_tokens,
    "total_tokens": total_tokens,
    "latency_ms": latency,
    "status": status_code,
    "cost_usd": cost_usd,
})
```

### SSE Endpoint

```python
@router.get("/logs/stream")
async def log_stream(request: Request) -> StreamingResponse:
    await _require_master(request)
    q = subscribe()
    async def generate():
        try:
            while True:
                payload = await asyncio.wait_for(q.get(), timeout=30)
                yield f"data: {payload}\n\n"
        except asyncio.TimeoutError:
            yield f"data: {json.dumps({'type': 'keepalive'})}\n\n"
        finally:
            unsubscribe(q)
    return StreamingResponse(generate(), media_type="text/event-stream")
```

### Embedded HTML Dashboard

```python
@router.get("/logs", include_in_schema=False)
async def log_dashboard(request: Request) -> Response:
    await _require_master(request)
    return Response(content=HTML_PAGE, media_type="text/html")
```

A single self-contained HTML page with `EventSource("/admin/logs/stream")`, a `<pre>` log display with auto-scroll, and styling for a terminal-like dashboard.

---

## 13. Graceful Draining

(Identical to current state.)

---

## 14. Key Format & Security

- **Master key**: `sk-pico-master-<64-char-hex>` — defined in `general_settings.master_key`
- **User keys**: `sk-pico-<64-char-hex>` — generated by admin API, stored in SQLite
- **User key → User mapping**: `user_keys.user_id` FK to `users.id` (nullable for legacy keys)
- **Storage**: SHA-256 hashed in SQLite. Raw key shown only once on creation.
- **Transport**: Always over HTTPS (terminated by reverse proxy)
- **Audit**: All admin actions logged to `admin_log` table
- **Auth**: FastAPI `Depends()` — single dependency injection point, no duplicated auth blocks
- **Comparison**: `hmac.compare_digest()` for timing-safe key comparison

---

## 15. Resource Budget (RAM)

Measured on Python 3.12, Intel Atom D410 (1c/2t @1.66GHz), 2GB DDR2.

| Component | Idle | 10 concurrent | 50 concurrent |
|---|---|---|---|
| Python interpreter | ~14MB | ~14MB | ~14MB |
| FastAPI + uvicorn + deps | ~37MB | ~37MB | ~37MB |
| core.* modules | ~0.2MB | ~0.2MB | ~0.2MB |
| Provider modules (lazy) | ~0.6MB | ~0.6MB | ~0.6MB |
| App imports (server, admin, website) | ~2.9MB | ~2.9MB | ~2.9MB |
| SQLite shared pool (2 conn + 8MB cache + 64MB mmap) | ~2MB | ~10MB | ~10MB |
| In-memory LRU cache (256 entries) | ~1MB | ~10MB | ~20MB |
| httpx shared clients (4 providers, 10 conn each) | ~2MB | ~5MB | ~10MB |
| Request buffers | 0 | ~10MB | ~30MB |
| Rate limit cache | ~0.5MB | ~0.5MB | ~1MB |
| SSE event queues | 0 | ~0.5MB | ~1MB |
| Router index | ~0.5MB | ~0.5MB | ~0.5MB |
| **Total** | **~60MB** | **~91MB** | **~127MB** |

---

## 16. Test Suite

67 tests currently pass. All tests live in `tests/` and import from `core.*` and `api.*` packages.

### Test Configuration

```ini
# pyproject.toml
[tool.pytest.ini_options]
testpaths = ["tests"]
asyncio_mode = "auto"
```

### Test Dependencies

```toml
[project.optional-dependencies]
test = [
    "pytest>=8.0",
    "pytest-asyncio>=0.24",
    "httpx>=0.28.0",        # For TestClient / ASGI transport
]
```

Run with: `pytest` or `python -m pytest`

### Current Tests

**`tests/test_router.py`** — 7 tests
- Resolve returns correct group/key/entry
- Returns None for unknown model
- `get_model_names()` returns configured models
- Picks next key on cooldown (429)
- Returns None when all keys cooled
- Picks other group when circuit is OPEN
- Circuit breaker recovers after timeout

**`tests/test_ratelimit.py`** — 10 tests
- RPM allows/rejects/separate keys/reservation
- TPM reconcile
- User + model levels separate
- ASH allows/rejects
- ASD allows/rejects

**`tests/test_retry_loop.py`** — 5 tests
- 5xx retry, exhaustion, non-retryable, failover, no-chain

**`tests/test_cost.py`** — 5 tests
- Cost computation with both input/output rates
- Blended rate fallback (only output set, only input set)
- No cost when neither rate is set
- Zero-token edge case

**`tests/test_events.py`** — 3 tests
- Emit + subscribe receives event
- Multiple subscribers each receive events
- Slow consumer drops oldest (backpressure)

**`tests/test_teams.py`** — 11 tests
- Create team, create user under team
- Update team limits, update user limits/budget
- User budget exceeded → 429
- User budget not set → no error
- Merge limits (most restrictive wins)
- Merge allowlist (intersection)
- Cascade: deactivate team → all users + keys blocked
- Team/user month spend aggregation
- Auth resolves user/team hierarchy

**`tests/test_cache.py`** — 4 tests
- Cache set and get
- Cache key uniqueness (different bodies → different keys)
- Cache expiry (negative TTL → no hit)
- Clear all cache entries

**`tests/test_admin_api.py`** — 22 tests
- HTTP-level tests for all admin endpoints
- Auth enforcement (missing key, wrong key, master key required)
- CRUD lifecycle: create key → update limits → assign to user → revoke
- Team CRUD: create team → add user → set limits → check usage
- Usage endpoint: aggregate stats, top models, cost breakdown
- Config reload: graceful drain + restart
- SSE stream: connect, receive events, disconnect

---

## 17. Implementation Status

All 5 phases **complete**. 67 tests, all passing.

### Phase 0: Test Infrastructure

| Task | Status |
|---|---|
| Move tests from `temp/tests/` to `tests/` | Done |
| Add `tests/test_admin_api.py` — 22 HTTP-level tests | Done |
| Add pytest config to `pyproject.toml` | Done |
| Add test dependencies section | Done |

### Phase 1: Security

| Task | Status |
|---|---|
| `${ENV_VAR}` config syntax for secrets | Done |
| `hmac.compare_digest()` for timing-safe key comparison | Done |
| FastAPI `Depends()` for auth — single injection point | Done |
| Remove duplicated auth blocks from route handlers | Done |

### Phase 2: Performance

| Task | Status |
|---|---|
| Shared httpx client per provider (not per request) | Done |
| SQLite connection pool (2 connections) + PRAGMAs | Done |
| In-memory LRU cache replaces SQLite BLOB cache | Done |
| Rate limiter optimization — all windows in-memory | Done |
| Batched single-transaction flush for daily counters | Done |

### Phase 3: Polish

| Task | Status |
|---|---|
| Usage log 30-day auto-prune | Done |
| `X-RateLimit-*` headers on every response | Done |
| Budget page N+1 query fix | Done |
| Gemini streaming fix | Done |

### Remaining Polish

| Task | Priority | Notes |
|---|---|---|
| `--version` CLI flag | Low | Click CLI, straightforward |
| `docker-compose.yml` | Low | Single-service compose for local dev |
| Documentation updates | Low | Update understand.md, README.md |
| End-to-end integration tests | Medium | Requires live provider keys |
| Token-based audio rate limiting | Low | ash/asd count requests; could estimate from audio duration |
