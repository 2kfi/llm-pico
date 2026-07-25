# llm-pico

> Lightweight LLM proxy. Zero external dependencies by design.

---

## Architecture

```
api/          FastAPI routes, HTTP layer, streaming, admin API
core/         Business logic: routing, auth, streaming, usage, profiler, migrations
providers/    Lazy-loaded provider adapters (OpenAI, Anthropic, Gemini, Cloudflare, Cohere, openai-compat)
website/      Vanilla JS SPA dashboard (no build step, no frameworks)
tests/        169 tests, all passing
```

---

## Features

### Routing
- **Round-robin key rotation** with circuit breaker (CLOSED → OPEN → HALF_OPEN)
- **Weighted round-robin** by EMA health score
- **Latency-aware routing** — percentile-based selection from rolling window
- **Cost-aware routing** — preflight cost estimation, budget enforcement
- **Model alias resolution** — fuzzy matching, capability registry, smart fallback chains
- **Chain-of-LLMs (COLLM)** — per-team ordered model chain with per-link budgets, hop tracking

### Streaming
- **Zero-copy streaming** — direct pipe, backpressure, cancellation propagation
- **Stream usage reconciliation** — extract token counts from streaming deltas
- **SSE heartbeat** — keepalive during long generations

### Auth & Security
- **API key scopes** — read/write/admin with RBAC
- **IP allowlist per key** — CIDR support
- **HMAC request signing** — optional request verification
- **Audit log** — structured JSONL logging with IP attribution

### Budgets & Cost
- **Per-key budgets** — monthly spend limits with hard block
- **Real-time cost projection** — pre-generation cost estimate
- **Provider cost comparison** — cross-provider pricing
- **Token budget reservations** — atomic reserve-reconcile pattern
- **Budget alert webhooks** — threshold-based HTTP POST notifications

### Observability
- **Request tracing** — per-request span logging with waterfall UI
- **Error taxonomy** — retryable vs permanent classification
- **Slow request profiler** — timing breakdown per stage
- **Prompt/response sampling** — configurable capture for debugging
- **Prometheus metrics** — `/admin/metrics` endpoint

### Dashboard
- **Vanilla JS SPA** — dark mode, keyboard shortcuts, command palette
- **Pages**: Overview, Models, Keys, Teams, Usage, Playground, Settings, Tracing, Routing Graph
- **Init wizard** — auto-discover providers, probe model capabilities

### Provider Ecosystem
- **Universal OpenAI-compatible adapter** — any OpenAI-compatible provider
- **Custom provider SDK** — drop Python files in `providers/custom/`, auto-loaded on startup
- **Provider model registry sync** — admin endpoint to sync model lists from providers
- **Capability probing** — auto-detect tools, vision, JSON mode on startup

### Ops
- **Hot config reload** — no restart needed
- **Database migration system** — versioned schema upgrades
- **Graceful degradation modes** — reject/queue/fallback_only strategies
- **Multi-process workers** — `--workers N` flag

---

## Provider Matrix

| Slug | Adapter | Images | Embeddings | STT | TTS | Custom |
|------|---------|--------|------------|-----|-----|--------|
| `openai` | OpenAIAdapter | yes | yes | yes | yes | - |
| `anthropic` | AnthropicAdapter | yes | - | - | - | - |
| `gemini` | GeminiAdapter | yes | yes | - | - | - |
| `cloudflare` | CloudflareAdapter | - | yes | - | - | - |
| `openai-compat` | OpenAICompatAdapter | auto | auto | auto | auto | yes |
| `cohere` | CohereAdapter | - | yes | - | - | - |
| Unknown slug | OpenAIAdapter (fallback) | - | - | - | - | - |

---

## API Endpoints

### User-facing (requires API key)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/chat/completions` | Chat completions |
| POST | `/v1/completions` | Text completions |
| POST | `/v1/embeddings` | Embeddings |
| POST | `/v1/audio/transcriptions` | Speech-to-text |
| POST | `/v1/audio/speech` | Text-to-speech |
| GET | `/v1/models` | List available models |

### Admin (requires master key)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/models` | List all models |
| POST | `/admin/models` | Create model |
| PUT | `/admin/models/{id}` | Update model |
| DELETE | `/admin/models/{id}` | Delete model |
| POST | `/admin/providers/probe` | Probe provider capabilities |
| POST | `/admin/providers/sync` | Sync model registry from providers |
| GET | `/admin/keys` | List API keys |
| POST | `/admin/keys` | Create API key |
| DELETE | `/admin/keys/{id}` | Delete API key |
| GET | `/admin/teams` | List teams |
| POST | `/admin/teams` | Create team |
| PUT | `/admin/teams/{id}/chain` | Set model chain |
| GET | `/admin/teams/{id}/chain` | Get model chain |
| PUT | `/admin/teams/{id}/chain/rewrites` | Set chain rewrites |
| GET | `/admin/traces/{request_id}` | Get request trace |
| POST | `/admin/degradation` | Set degradation mode |
| GET | `/admin/degradation` | Get degradation mode |
| GET | `/admin/usage` | Usage statistics |
| GET | `/admin/stats/costs` | Cost breakdown |
| GET | `/admin/stats/errors` | Error statistics |
| GET | `/admin/metrics` | Prometheus metrics |
| POST | `/admin/config/reload` | Hot reload config |

---

## Chain-of-LLMs (COLLM)

Per-team ordered model chain. Client asks for one model, the chain routes to best available.

```
Client → resolve team chain → try model 0 → fail → try model 1 → success
Response headers:
  X-Actual-Model: gpt-5.5
  X-Chain-Hops: 1
  X-Chain-Tried: claude-fable-5,gpt-5.5
```

---

## Degradation Modes

| Mode | Behavior |
|------|----------|
| `normal` | All requests processed normally |
| `reject` | Return 503 immediately |
| `queue` | Wait for slot (bounded queue, depth 1000) |
| `fallback_only` | Only serve models marked as fallbacks |

---

## Configuration

Config stored in SQLite. Hot-reloadable via `/admin/config/reload`.

Key settings:
- `general_settings.master_key` — admin authentication
- `general_settings.usage_log_retention_days` — log retention
- `router_settings.num_retries` — retry count per request
- `router_settings.num_failover_retries` — failover retry count

---

## File Structure

```
llm-pico/
├── api/
│   ├── admin.py          Admin endpoints
│   ├── cli.py            CLI entry point
│   ├── dependencies.py   FastAPI dependencies
│   └── server.py         Main app, routing, streaming
├── core/
│   ├── aliases.py        Model alias resolution
│   ├── auth.py           Key verification, audit log
│   ├── cache.py          Response caching
│   ├── config.py         Config models
│   ├── db.py             SQLite connection pool
│   ├── degradation.py    Graceful degradation
│   ├── events.py         Event emission (SSE)
│   ├── migrations.py     Schema versioning
│   ├── profiler.py       Latency tracking
│   ├── ratelimit.py      Multi-window rate limiter
│   ├── router.py         Routing, circuit breaker, health
│   ├── sampling.py       Prompt/response sampling
│   ├── streaming.py      SSE parsing, heartbeat
│   ├── teams.py          User/team hierarchy, budgets
│   └── usage.py          Usage logging, cost tracking
├── providers/
│   ├── __init__.py       Registry, lazy loading, custom loader
│   ├── base.py           BaseAdapter, shared httpx clients
│   ├── openai.py         OpenAI adapter
│   ├── anthropic.py      Anthropic adapter
│   ├── gemini.py         Gemini adapter
│   ├── cloudflare.py     Cloudflare adapter
│   ├── cohere.py         Cohere adapter
│   └── openai_compat.py  Universal OpenAI-compatible adapter
├── website/static/
│   ├── index.html        SPA shell
│   ├── css/              tokens, layout, components, pages
│   └── js/               app, api, dashboard, models, keys, teams,
│                         usage, settings, init, trace, graph, crypto, utils
├── tests/                169 tests
└── PLAN.md               This file
```

---

## Tests

```bash
python -m pytest tests/ -x -q
```

169 tests covering: admin API, adapters, budget, cache, cost, events, migrations, observability, rate limiting, retry loop, router, security, streaming, teams.

---

## What We Deliberately Reject

| Feature | Why | Revisit When |
|---------|-----|--------------|
| Redis | Zero-dep design. In-memory + SQLite handles 50 concurrent. | 500+ concurrent |
| PostgreSQL | SQLite correct for single-process <=50 clients. | 1000+ clients |
| JWT / OAuth2 / SSO | Master key sufficient for single-team. | Multi-org federated identity |
| Multi-level failover chains | 4 providers. Single failover covers realistic failures. | 10+ providers |
| Soft budgets | Hard block simpler and safer. | Enterprise graduated throttling |
