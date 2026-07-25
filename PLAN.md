# llm-pico — Implementation Plan

> **Version**: v2.0 Complete + Improvements
> **Last Updated**: 2026-07-25
> **Philosophy**: Perfection over options, knife not Swiss Army knife. Zero external dependencies by design.

---

## Current State Summary (v2.0 Complete)

### Architecture
```
api/          — FastAPI routes, HTTP layer, streaming, admin metrics
core/         — Business logic (routing, auth, streaming, usage, profiler, sampling, migrations, degradation)
providers/    — Lazy-loaded provider adapters (OpenAI, Anthropic, Gemini, Cloudflare, OpenAI-compat, Cohere)
website/      — Alpine.js SPA dashboard (dark mode, playground, import/export, keyboard shortcuts)
tests/        — 169 tests, all passing
```

### Features Implemented (v2.0)
- **OpenAI-compatible API**: chat, completions, embeddings, audio STT/TTS
- **Round-robin key rotation** with circuit breaker (3 states: CLOSED → OPEN → HALF_OPEN)
- **Model alias resolution** — fuzzy matching, capability registry, smart fallback chains
- **Latency-aware routing** — percentile-based selection from rolling window
- **Cost-aware routing** — preflight cost estimation, budget enforcement
- **Weighted round-robin** — EMA-based failure weighting
- **Zero-copy streaming** — direct pipe, backpressure, cancellation propagation
- **Stream usage reconciliation** — extract token counts from streaming deltas
- **SSE heartbeat** — keepalive during long generations
- **API key scopes** — read/write/admin with RBAC
- **IP allowlist per key** — CIDR support
- **HMAC request signing** — optional request verification
- **Audit log** — structured logging with IP attribution
- **Per-key budgets** — monthly spend limits with hard block
- **Real-time cost projection** — pre-generation cost estimate
- **Provider cost comparison** — cross-provider pricing
- **Token budget reservations** — atomic reserve-reconcile pattern
- **Error taxonomy** — retryable vs permanent classification
- **Slow request profiler** — timing breakdown per stage
- **Prompt/response sampling** — configurable capture for debugging
- **Prometheus metrics** — `/admin/metrics` endpoint
- **Alpine.js dashboard** — modern SPA with dark mode, playground, import/export, bulk ops, keyboard shortcuts
- **Universal OpenAI-compatible adapter** — any OpenAI-compatible provider
- **Provider capability probing** — auto-detect features on startup
- **Hot config reload** — no restart needed
- **Database migration system** — versioned schema upgrades
- **Graceful degradation modes** — continue on provider failures
- **Multi-process workers** — `--workers N` flag

### Provider Matrix
| Slug | Adapter | Images | Embeddings | STT | TTS | Custom |
|------|---------|--------|------------|-----|-----|--------|
| `openai` | OpenAIAdapter | ✅ | ✅ | ✅ | ✅ | - |
| `anthropic` | AnthropicAdapter | ✅ | ❌ | ❌ | ❌ | - |
| `gemini` | GeminiAdapter | ✅ | ✅ | ❌ | ❌ | - |
| `cloudflare` | CloudflareAdapter | ❌ | ✅ | ❌ | ❌ | - |
| `openai-compat` | OpenAICompatAdapter | auto | auto | auto | auto | ✅ |
| `cohere` | CohereAdapter | ❌ | ✅ | ❌ | ❌ | - |
| Unknown slug | OpenAIAdapter (fallback) | - | - | - | - | - |

### Resource Budget
| Component | Idle | 10 concurrent | 50 concurrent |
|-----------|------|---------------|---------------|
| Python + FastAPI + core | ~54MB | ~54MB | ~54MB |
| SQLite pool + cache + rate limit | ~3.5MB | ~20.5MB | ~31MB |
| httpx clients | ~2MB | ~5MB | ~10MB |
| Request buffers | 0 | ~10MB | ~30MB |
| **Total** | **~60MB** | **~91MB** | **~127MB** |

---

## What We Deliberately Reject (v1.0 — No Change)

| Feature | Why Rejected | Revisit When |
|---------|--------------|--------------|
| **Redis** | Zero-dep design. In-memory + SQLite handles 50 concurrent fine. | 500+ concurrent clients |
| **PostgreSQL** | SQLite correct for single-process ≤50 clients. WAL + pool covers write contention. | 1000+ clients, multi-process writes |
| **JWT / OAuth2 / SSO** | Master key sufficient for single-team. JWT adds token expiry, refresh flows, crypto verification. | Multi-org federated identity |
| **Multi-level failover chains** | 4 providers. Single failover level covers realistic failures. N² complexity for negligible gain. | 10+ providers |
| **Key rotation grace periods** | `${ENV_VAR}` solves rotation without code changes. Grace periods = two valid keys = security liability. | Compliance-mandated rotation |
| **Soft budgets** | Hard block simpler and safer. Predictable > "helpful" behavior. | Enterprise graduated throttling |
| **Provider transformation abstraction** | Premature with 4 providers. Each has 1-2 unique quirks. | 10+ providers with shared patterns |

---

## Bugs Found & Fixed

### 1. Streaming Buffering in `_handle_streaming` (api/server.py:459)
**Issue**: `proxy_stream()` in base adapter buffers all chunks into a list before yielding, adding memory overhead and latency.
```python
# Current (buffers entire stream):
stream_chunks, stream_usage = await adapter.proxy_stream(upstream)
for chunk in stream_chunks:
    yield chunk
```
**Fix**: Stream directly from upstream without intermediate buffering. Yield chunks as they arrive.

### 2. Failover Chain Depth Limited to 1 (api/server.py:393)
**Issue**: Only tries `failover_model` once. If that model's provider is also down, request fails.
```python
# Current: single failover only
if not _is_failover and model_entry and model_entry.failover_model:
    return await _proxy_request(..., model_name=model_entry.failover_model, _is_failover=True)
```
**Fix**: Implement full fallback chain (already in config model `fallbacks` list). Router has `resolve_with_fallbacks()` but not wired into request flow.

### 3. Retry Loop Doesn't Clear Rate Limit Reservation on Failover
**Issue**: When retries exhausted and failover triggers, the original key's rate limit reservation isn't released/reconciled.
**Fix**: Call `limiter.reconcile()` with actual=0 before failover attempt.

### 4. Audio Routes Missing Stream Usage Reconciliation
**Issue**: `/audio/speech` and `/audio/transcriptions` don't reconcile token reservations after completion.
**Fix**: Add `limiter.reconcile()` in `_proxy_audio_request` and `_proxy_audio_speech` finally blocks.

### 5. Race Condition in Rate Limiter Window Roll-over
**Issue**: In `check_and_reserve()`, when window rolls over, stale entry may not flush if `dirty=False` but `window_start` outdated.
**Fix**: In `_load_from_db()` and window transition, always check `window_start` against current.

---

## Improvement Plan

### Category A: Critical Fixes (Do First)

#### A1. Fix Streaming Buffering → True Zero-Copy
**File**: `api/server.py:_handle_streaming`, `providers/base.py:proxy_stream`
**Effort**: Small
**Impact**: 10x lower memory for large streams, lower latency

```python
# New approach - stream directly:
async def generate():
    async for chunk in upstream.aiter_bytes():
        usage = await parse_stream_usage(chunk)
        if usage: update_usage(usage)
        yield chunk
    yield b"data: [DONE]\n\n"
```

#### A2. Wire Full Fallback Chain
**File**: `api/server.py:_proxy_request` (line ~393)
**Effort**: Small
**Impact**: Resilience — survives multiple provider outages

```python
# Replace single failover with:
result = router.resolve_with_fallbacks(model_name)
if result:
    provider_group, key_state, model_entry, resolved_name = result
```

#### A3. Release Rate Limit Reservation on Failover
**File**: `api/server.py:_proxy_request` (before failover call)
**Effort**: Tiny
**Impact**: Prevents phantom rate limit exhaustion

```python
await limiter.reconcile(key_hash=..., actual_tokens=0, reserved_tokens=reservation)
```

#### A4. Add Reconciliation to Audio Routes
**File**: `api/server.py:_proxy_audio_request`, `_proxy_audio_speech`
**Effort**: Tiny
**Impact**: Accurate rate limiting for audio

---

### Category B: Routing Intelligence

#### B1. Weighted Round-Robin by Health Score
**File**: `core/router.py:resolve` (line 167)
**Status**: Partially implemented — health_score exists but weight not used
**Effort**: Small
**Impact**: Better traffic distribution to healthy providers

```python
# Current: sort by health_score, pick best
# Better: weighted pick proportional to health_score
weights = [g.health_score for g, _ in group_picks]
pick = random.choices(group_picks, weights=weights)[0]
```

#### B2. Model Capability Auto-Probe on Startup
**File**: `providers/__init__.py`, `core/router.py:_build_index`
**Effort**: Medium
**Impact**: Zero-config capability detection (tools, vision, json_mode)

```python
async def _probe_all_models(self):
    for entry in self._model_entries.values():
        if not entry.capabilities_probed:
            caps = await adapter.probe_capabilities(entry.model_params.model)
            # Update DB with caps
```

#### B3. Semantic Model Matching (Fuzzy + Aliases)
**File**: `core/aliases.py:resolve_alias`
**Status**: Implemented but could expand alias map
**Effort**: Small
**Impact**: Users type "gpt4", "claude3", "gemini-pro" → auto-resolves

---

### Category C: Observability & Debugging

#### C1. Request Tracing UI (Waterfall View)
**File**: `website/static/js/dashboard.js` (new)
**Effort**: Medium
**Impact**: Debug latency issues visually

```
Request: req_abc123 (gpt-4, user: dev-bot)
├── Auth: 2ms ✓
├── Rate Limit: 1ms ✓
├── Router: 0ms → openai/gpt-4
├── Upstream: 1,234ms ✓
│   ├── TTFB: 450ms
│   └── Stream: 784ms
├── Cost: $0.0045
└── Total: 1,237ms
```

#### C2. Structured Audit Log (JSONL)
**File**: `core/auth.py:audit_log` (new), `core/db.py` schema
**Effort**: Small
**Impact**: Compliance, debugging config changes

```json
{"ts": "2026-07-25T10:30:00Z", "action": "key.create", "actor": "admin", "ip": "10.0.0.1", "before": null, "after": {"key_prefix": "sk-abc...", "scopes": ["chat:write"]}}
```

#### C3. Budget Alert Webhooks
**File**: `core/usage.py:check_budget_alerts` (new)
**Effort**: Small
**Impact**: Proactive cost control

```yaml
budget_alerts:
  enabled: true
  webhook_url: "https://hooks.slack.com/..."
  thresholds: [0.8, 0.9, 1.0]
```

---

### Category D: Dashboard UX

#### D1. Visual Routing Graph
**File**: `website/static/js/dashboard.js` (new canvas component)
**Effort**: Medium
**Impact**: Mental model of routing

```
Models          Provider Groups         Keys
┌──────┐        ┌───────────┐          ┌────────┐
│gpt-4 │ ────── │ OpenAI ✓  │ ──────── │ sk-... │
└──────┘        └───────────┘          └────────┘
┌────────┐       ┌───────────┐          ┌────────┐
│claude3 │ ────── │ Anthropic ⚠ │ ────── │ sk-... │
└────────┘       └───────────┘          └────────┘
```

#### D2. Dark Mode + Accessibility (WCAG AA)
**File**: `website/static/index.html`, CSS
**Effort**: Medium
**Impact**: Usability

#### D3. Keyboard Shortcuts
**File**: `website/static/js/app.js`
**Effort**: Small
**Impact**: Power user productivity

| Shortcut | Action |
|----------|--------|
| `Cmd+K` | Command palette |
| `g m` | Go to Models |
| `g k` | Go to Keys |
| `g t` | Go to Teams |
| `/` | Focus search |

---

### Category E: Provider Ecosystem

#### E1. Custom Provider SDK (Hot-Reload)
**File**: `providers/custom/` (new directory), watcher in `core/config.py`
**Effort**: Medium
**Impact**: Extensibility without forking

```python
# providers/custom/my_provider.py
from providers.base import BaseAdapter
from providers import register

@register("my_provider")
class MyProviderAdapter(BaseAdapter):
    provider = "my_provider"
    supports_images = True
    
    def _set_auth_headers(self, headers):
        super()._set_auth_headers(headers)
        headers["X-API-Key"] = self.api_key
```

#### E2. Provider Model Registry Sync
**File**: `core/migrations.py` (new migration), admin endpoint
**Effort**: Medium
**Impact**: Auto-discover new models from OpenAI, Anthropic, OpenRouter

---

### Category F: Architecture & Ops

#### F1. Graceful Degradation Modes
**File**: `core/degradation.py` (new)
**Effort**: Medium
**Impact**: Survive overload gracefully

```python
mode = "reject" | "queue" | "fallback_only"
# reject: return 503 immediately
# queue: wait for slot (bounded queue)
# fallback_only: only serve models marked as fallbacks
```

#### F2. Database Migration Versioning
**File**: `core/migrations.py`
**Status**: Implemented (2 migrations)
**Effort**: Done

#### F3. Multi-Process Workers (SO_REUSEPORT)
**File**: `api/cli.py:main`
**Status**: Implemented (`--workers N`)
**Effort**: Done
**Note**: Requires Redis for cross-process rate limit sync (optional, off by default)

---

## Prioritization Matrix

| Priority | Items | Rationale |
|----------|-------|-----------|
| **P0 (Critical)** | A1, A2, A3, A4 | Core correctness, streaming integrity, resilience |
| **P1 (High Value)** | B1, B2, B3, C2, C3, D3, E1 | Smart routing, observability, extensibility |
| **P2 (Nice to Have)** | C1, D1, D2, E2, F1 | UX polish, advanced features |

---

## Key Decisions Needed

### 1. Dashboard Tech Stack
| Option | Size | Pros | Cons |
|--------|------|------|------|
| **Alpine.js** (current) | ~15KB | Declarative, reactive, no build | Slight learning curve |
| htmx | ~14KB | Server-driven, simple | Less interactive |
| Preact | ~3KB | React-like, tiny | Need build step |
| Vanilla (current v1) | ~70KB | Zero deps | Hard to maintain |

**Recommendation**: Keep Alpine.js — best balance of simplicity and power.

### 2. OTel vs Custom Metrics
| Option | Size | Pros | Cons |
|--------|------|------|------|
| **OpenTelemetry** | ~2MB | Industry standard, Jaeger/Zipkin | Large dep |
| **Custom `/metrics`** | ~50 lines | Zero deps, Prometheus format | Non-standard |

**Recommendation**: Custom `/metrics` for zero-dep, OTel as optional extra.

### 3. Multi-Process
| Option | Complexity | Pros | Cons |
|--------|------------|------|------|
| Single process (current) | Low | Simple, SQLite works | Single core only |
| Multi-process | High | Multi-core | Requires Redis for rate limits |

**Recommendation**: Keep single process default. Target hardware is single-core. Multi-process only if moving to multi-core.

---

## File Tree (for reference)
```
llm-pico/
├── api/
│   ├── __init__.py
│   ├── admin.py         # Admin endpoints, dashboard API
│   ├── cli.py           # CLI entry point (--workers)
│   ├── dependencies.py  # FastAPI dependencies
│   └── server.py        # Main FastAPI app, routing logic
├── core/
│   ├── __init__.py
│   ├── aliases.py       # Model alias resolution
│   ├── auth.py          # API key verification, scopes, IP allowlist, HMAC
│   ├── cache.py         # Response caching
│   ├── config.py        # Config models (Pydantic)
│   ├── db.py            # SQLite connection pool
│   ├── degradation.py   # Graceful degradation modes
│   ├── events.py        # Event emission (SSE)
│   ├── migrations.py    # Schema versioning
│   ├── models.py        # Pydantic models for requests
│   ├── profiler.py      # Latency tracking
│   ├── ratelimit.py     # Multi-window rate limiter
│   ├── router.py        # Model routing, circuit breaker, health scoring
│   ├── sampling.py      # Prompt/response sampling
│   ├── streaming.py     # SSE parsing, heartbeat, merge
│   ├── teams.py         # User/team hierarchy, budgets
│   └── usage.py         # Usage logging, cost tracking, budgets
├── providers/
│   ├── __init__.py      # Registry, lazy loading
│   ├── base.py          # BaseAdapter, shared httpx clients
│   ├── openai.py        # OpenAI adapter
│   ├── anthropic.py     # Anthropic adapter
│   ├── gemini.py        # Gemini adapter
│   ├── cloudflare.py    # Cloudflare adapter
│   ├── cohere.py        # Cohere adapter
│   └── openai_compat.py # Universal OpenAI-compatible adapter
├── tests/
│   ├── conftest.py
│   ├── test_admin_api.py
│   ├── test_adapters.py
│   ├── test_budget.py
│   ├── test_cache.py
│   ├── test_cost.py
│   ├── test_events.py
│   ├── test_migrations.py
│   ├── test_new_endpoints.py
│   ├── test_observability.py
│   ├── test_ratelimit.py
│   ├── test_retry_loop.py
│   ├── test_router.py
│   ├── test_security.py
│   ├── test_streaming.py
│   └── test_teams.py
├── website/
│   ├── __init__.py
│   ├── routes.py        # Static file serving
│   └── static/
│       ├── index.html   # Alpine.js SPA
│       └── js/
│           ├── app.js
│           ├── dashboard.js
│           ├── init.js
│           ├── api.js
│           ├── crypto.js
│           ├── keys.js
│           ├── providers.js
│           ├── usage.js
│           ├── teams.js
│           ├── models.js
│           └── utils.js
├── docs/
│   ├── CONFIG.md
│   ├── DEPLOYMENT.md
│   ├── KEYS.md
│   └── TROUBLESHOOTING.md
├── config.example.yaml
└── PLAN.md
```

---

## Category G: Chain-of-LLMs (COLLM)

> **Status**: Design Phase
> **Priority**: P1 (High Value)
> **Replaces**: `ModelEntry.failover_model` and `ModelEntry.fallbacks` (industry-standard pattern)

### Concept

Per-team ordered model chain. Client asks for one model, the chain routes to best available.
If a model hits budget/provider/rate-limit → next in chain. Response reflects which model actually answered.

### Why This Exists

- Current `failover_model` is single-level, per-model, not per-team
- Current `fallbacks[]` exists in config but isn't wired into `_proxy_request`
- Teams have budgets/rate-limits but no model routing preferences
- COLLM unifies all of this into one clean concept

### Data Model

```
teams table:
  +-- model_chain TEXT       -- JSON array: ["claude-fable-5", "gpt-5.5", "deepseek-chat"]
  +-- chain_rewrites_response INT DEFAULT 0  -- 1=rewrite body model field, 0=header only

models table (existing):
  +-- chain_budget_usd REAL  -- independent budget per chain link (NULL=no limit)
```

### Config (YAML / DB)

```yaml
teams:
  - name: engineering
    model_chain:
      - model: claude-fable-5
        chain_budget_usd: 50.00
      - model: gpt-5.5
        chain_budget_usd: 100.00
      - model: deepseek-chat
        chain_budget_usd: 0.00   # free fallback
```

### Request Flow

```
Client: POST /v1/chat/completions
Body: {"model": "claude-fable-5", "messages": [...]}

    │
    ├─→ Auth check (existing)
    │
    ├─→ Get user's team → resolve model_chain
    │   Chain: [claude-fable-5, gpt-5.5, deepseek-chat]
    │
    ├─→ Try claude-fable-5 (chain link 0)
    │   ├─ Chain budget OK? ──── No ──→ skip to next
    │   ├─ Model budget OK? ──── No ──→ skip to next
    │   ├─ Router resolve? ──── None ─→ skip to next
    │   ├─ Provider alive? ──── No ───→ skip to next
    │   └─ Route to provider
    │       ├─ Success → return response
    │       └─ Fail (5xx/timeout) → next key, then next model
    │
    ├─→ Try gpt-5.5 (chain link 1)
    │   └─ ... same flow ...
    │
    └─→ Try deepseek-chat (chain link 2)
        └─ Final fallback, fails → 502
```

### Response

```http
HTTP/1.1 200 OK
X-Request-Id: abc123
X-Actual-Model: gpt-5.5
X-Chain-Hops: 1
X-Chain-Tried: claude-fable-5,gpt-5.5

{
  "id": "chatcmpl-...",
  "model": "claude-fable-5",   ← or rewritten to "gpt-5.5" if chain_rewrites_response=1
  "choices": [...],
  "usage": {...}
}
```

### Headers

| Header | Description |
|--------|-------------|
| `X-Actual-Model` | Model that actually served the request (only if chain hop happened) |
| `X-Chain-Hops` | Number of models tried before success (0 = first model worked) |
| `X-Chain-Tried` | Comma-separated list of models attempted |

### Files to Change

| File | Change |
|------|--------|
| `core/config.py` | Add `chain_budget_usd` to `ModelEntry`, `model_chain` / `chain_rewrites_response` to team |
| `core/teams.py` | Add `get_team_chain(team_id)`, `check_chain_budget()`, `get_chain_for_user()` |
| `core/router.py` | New `resolve_chain(team_id, model_name)` method |
| `api/server.py` | Wire `_proxy_request` to try chain before failover |
| `core/migrations.py` | Migration: `ALTER TABLE teams ADD COLUMN model_chain TEXT` + `chain_rewrites_response INT` |
| `core/migrations.py` | Migration: `ALTER TABLE models ADD COLUMN chain_budget_usd REAL` |
| `api/admin.py` | Admin endpoints: GET/POST/DELETE chain per team |
| `website/static/js/teams.js` | Chain editor in dashboard (drag-to-reorder) |
| `tests/test_collm.py` | New test file: chain resolution, budget checks, hop tracking |

### Admin Endpoints

```
GET  /admin/teams/{id}/chain        → get chain for team
POST /admin/teams/{id}/chain        → set chain (replaces entire chain)
POST /admin/teams/{id}/chain/reorder → reorder chain items
DELETE /admin/teams/{id}/chain/{model} → remove model from chain
```

### Key Design Decisions

1. **Chain hops tracked** — `X-Chain-Hops` header for observability
2. **Budget per link** — each model in chain has independent `chain_budget_usd`
3. **Replaces `fallbacks[]`** — cleaner, one concept per team
4. **Admin configurable** — per-team in dashboard, not per-request
5. **Hot-reloadable** — chain changes apply without restart
6. **Model name rewriting optional** — admin sets `chain_rewrites_response` per team

### Budget Check Per Chain Link

```python
async def check_chain_budget(team_id: int, model_name: str) -> bool:
    """Check if this model in the chain has remaining budget."""
    chain = await get_team_chain(team_id)
    for link in chain:
        if link["model"] == model_name:
            budget = link.get("chain_budget_usd")
            if budget is None:
                return True  # No limit
            spend = await get_chain_model_spend(team_id, model_name)
            return spend < budget
    return True  # Model not in chain = no chain budget
```

### Observability

```sql
-- Chain hop stats
SELECT model_name, COUNT(*) as hops, AVG(latency_ms) as avg_latency
FROM usage_log
WHERE chain_hop = 1
GROUP BY model_name
ORDER BY hops DESC;

-- Chain success rate per link
SELECT
    team_id,
    model_name,
    COUNT(*) as total,
    SUM(CASE WHEN status_code = 200 THEN 1 ELSE 0 END) as successes,
    ROUND(100.0 * SUM(CASE WHEN status_code = 200 THEN 1 ELSE 0 END) / COUNT(*), 1) as success_rate
FROM usage_log ul
JOIN user_keys uk ON ul.key_hash = uk.key_hash
JOIN users u ON uk.user_id = u.id
WHERE chain_hop = 1
GROUP BY team_id, model_name
ORDER BY team_id, success_rate DESC;
```

### Migration SQL

```sql
-- Migration v3: COLLM
ALTER TABLE teams ADD COLUMN model_chain TEXT;
ALTER TABLE teams ADD COLUMN chain_rewrites_response INTEGER DEFAULT 0;
ALTER TABLE models ADD COLUMN chain_budget_usd REAL;
```

### Tests

```python
# tests/test_collm.py

async def test_chain_resolves_first_model():
    """Chain with 3 models resolves to first available."""

async def test_chain_skips_over_budget_exhausted():
    """Chain skips model with exhausted chain_budget_usd."""

async def test_chain_skips_over_provider_down():
    """Chain skips model whose provider circuit breaker is OPEN."""

async def test_chain_fallback_to_last_model():
    """Chain falls through to last model when all others fail."""

async def test_chain_hop_header_present():
    """X-Chain-Hops header present when chain hop occurred."""

async def test_chain_rewrite_model_in_response():
    """Response body model field rewritten when chain_rewrites_response=1."""

async def test_chain_budget_independent_per_link():
    """Each chain link has independent budget tracking."""

async def test_chain_hot_reload():
    """Chain changes applied without restart."""
```

---

## Prioritization Matrix (Updated)

| Priority | Items | Rationale |
|----------|-------|-----------|
| **P0 (Critical)** | A1, A2, A3, A4 | Core correctness, streaming integrity, resilience |
| **P1 (High Value)** | G1-G9 (COLLM), B1, B2, C2, C3, E1 | COLLM is new feature, smart routing, observability |
| **P2 (Nice to Have)** | B3, C1, D1, D2, D3, E2, F1 | UX polish, advanced features |

---

## Next Steps (Immediate)

1. **Fix A1-A4** — Critical bugs affecting streaming, failover, rate limits
2. **Implement COLLM (G1-G9)** — New feature: Chain-of-LLMs
3. **Run full test suite** — Ensure no regressions
4. **Implement B1-B3** — Smarter routing
5. **Add C2-C3** — Audit log + budget alerts
6. **Build D3** — Keyboard shortcuts (quick win)

Each item is independent and can be done in any order after P0 fixes.