# Plan: `v0.5.0-AGENTIC-AGE` (code-audited revision)

**NEXT TAG: `0.5.0-AGENTIC-AGE`** — this plan (all workstreams below) is scoped to be released as the next version tag, cut when the tag gate at the bottom passes.

## What the audit found (changes the plan)

| # | Finding | Consequence for agents |
|---|---------|------------------------|
| 1 | **Anthropic adapter drops tools entirely** — `toAnthropic` (`anthropic.go:155-166`) has no `tools`/`tool_calls`/`tool_result` fields; stream translation only handles `text_delta` (`anthropic.go:509-520`), so `input_json_delta`/`content_block_start` tool blocks vanish; maps `stop_reason` but calls never materialize | Claude slots **cannot run a tool loop at all**, silently. Biggest single blocker |
| 2 | **Upstream 429 aborts the slot walk** — `isClientRejection` (`proxy.go:840-846`) treats any `TypeRateLimited` as caller fault → `Dispatch` breaks the walk (`proxy.go:231-233`) after the first slot's upstream 429 exhausts retries | Agent burst hits one provider's 429 wall and dies instead of failing over to other slots |
| 3 | **`estimateTokens = len(body)/4 + max_tokens`** (`proxy.go:360-366`) and `slotsWithinContext` uses it pre-walk (`proxy.go:322-336`) | 1 MB tool-loop body ≈ 250k fake tokens → rejected against ctx or eats huge TPM reservation; ~4 large reqs/min per slot on a 1M TPM cell |
| 4 | **No per-request context timeout; no stream inactivity timeout** — `http.Server` has no WriteTimeout by design (`main.go:195-198`), transport `ResponseHeaderTimeout: 60s` (`providers.go:167-175`) only guards until first header; stalled SSE hangs forever | 30-min streams work, *but* a stalled upstream hangs the agent indefinitely |
| 5 | **Anthropic default `max_tokens: 4096`** when unset (`anthropic.go:175`) | Agent replies silently capped at 4096 output tokens on Anthropic slots |
| 6 | Only **3 tries/slot, no backoff** (`maxTries` at `proxy.go:437`, `NumRetries=2` default); all retry knobs global-only (`RouterSettings`, `config.go:33-37`) | No tuning per model/provider for bursty agent loads |
| 7 | **Provider metadata destroyed** at `upstreamError` (`proxy.go:799-820`): `EndpointError.Provider` + upstream `Retry-After` / `x-ratelimit-*` headers dropped (`httpx/errors.go:32-41` supports it — it's just never populated) | Agents can't pace themselves; no per-provider attribution in error paths |
| 8 | **No cached-token concept anywhere** — adapters ignore `cached_tokens`/`cache_read_input_tokens`/`cachedContentTokenCount`; `Reconcile` charges face value (`ratelimit.go:318-333`); `usage.Cost` ignores cache rates (`usage.go:25-37`) | Prompt caching (the whole point of "keep the context") is invisible: over-billed TPM, no cache routing signal |
| 9 | Memory amplification: body fully buffered (`io.ReadAll`, `server.go:382-388`), re-validated 2-3× (`server.go:153/166/440`), Go-remarshalled per attempt (`providers.go:256-267`), `string(body)` copies per attempt (`openai.go:166`) | ~4× per-attempt memory on big agent payloads; 500 sequential calls amplify GC pressure |
| 10 | 4 MiB SSE scanner cap (`providers.go:359-365`) — single SSE line >4 MiB kills stream (`bufio.Scanner: token too long`); unbounded `finish` read on Complete (`openai.go:306`) | Edge case: huge tool outputs can break streams |
| 11 | `Capability` struct has no `Tools/Reasoning/Cache` fields (`providers.go:24-31`); `ModelCapability.SupportsTools` is probed but **never consulted in dispatch** (`providers.go:124`) | `/v1/models` can't honestly advertise agent-readiness; no tool-aware slot skipping |
| 12 | Fixed-window rate buckets (`ratelimit.go:208-227`); `ProxyKeys` auth = O(N) hash scan per request (`server.go:251-257`) | Minute-boundary double-bursts; minor auth overhead at agent call rates |

(Good news: 64 MiB body cap is fine for 1 MB contexts; no server WriteTimeout so long streams aren't killed; Gemini's tool/thinking translation is solid and is the template for Anthropic.)

---

## Additions (post-write review)

1. **Embeddings batch gap** — Gemini adapter sends one `requests[]` element per call (`batchEmbedContents` never batches): an agent indexing a codebase sends N texts and pays N upstream calls + N rate-limit charges. Add batching of the OpenAI `input` array for embeddings (Gemini + openai-compat), matched to per-model `rpm` behavior in `models.yaml`.
2. **413 vs 400** — oversized bodies currently return `400 "request body too large"` (`server.go:117-118`); agents misread 400 as a schema problem. Return `413` (keep the same message) so agent frameworks retry-with-smaller-context instead of erroring out the run.
3. **Tool-call ID space** — adapters mint IDs (`call_<hex>`); ensure IDs minted in one turn round-trip correctly for later `tool` turns within the same request body (id→function map is rebuilt per request, so N parallel tool calls in one turn must map uniquely — add a test).
4. **Smoke harness** — the tag gate needs repeatable proof: add `tools/smoke-agent.sh` running the 10-turn tool loop, long stream, 200k context, and 429-failover per adapter against the deployed instance, so the tag gate is one command, not ad-hoc curls.
5. **`git` hygiene for the feature work** — HAND-OFF says the tree already carries uncommitted Gemini thinking/audio/media work; land/commit that cleanly *before* W1 starts so the agent-release diffs stay reviewable.

### W1 — Make the walk survive agent bursts *(P0, smallest diff, biggest win)*
1. Fix `isClientRejection` (`proxy.go:840-846`): only the proxy's **own** limiter rejections (`quotaRejection`) are client faults; **upstream 429s become quota-shaped errors that continue the walk**. Add `Source: "upstream" | "proxy"` on `httpx.Error` at classify time in `usage.Classify` (`usage.go:42-62`).
2. Per-model attempt knobs: add `retries:`/`attempt_budget:` to `ModelEntry` (`config/config.go:57-81`) overriding `RouterSettings.NumRetries` in `maxTries` (`proxy.go:437`).
3. Add backoff between per-slot tries (exponential ~100ms→2s, capped), currently none.
4. Fix `ErrModelNotFound` from model-cooldown (`router.go:521-535` via `proxy.go:457-460`) to be quota-shaped so it falls through to next slot.
5. Populate `httpx.Error.RetryAfter` + passthrough of upstream `x-ratelimit-*`/`retry-after` in `upstreamError` (`proxy.go:799-820`); add `X-Pico-Provider` on error responses.

### W2 — Anthropic agent parity *(P0 — without this, "across many providers" is false)*
1. Add `tools`, `tool_choice`, assistant `tool_calls`, and `tool`-role messages to `toAnthropic`/`convertContent` (`anthropic.go:137-268`), mirroring Gemini's lossless-JSON-schema approach (`gemini.go:371-415`).
2. Stream translation: handle `content_block_start` (tool_use), `input_json_delta` stitching, emit OpenAI `delta.tool_calls` with index tracking — port the `geminiStream` pattern (`gemini.go:682-804`) to Anthropic SSE.
3. `thinking`/`reasoning_effort` → Anthropic `thinking: {budget_tokens...}` translation.
4. Fix the 4096 default cap: `max_output:` knob in `ModelEntry`, defaulting much higher.
5. Inject `cache_control` on the stable prefix (system + first message) by **default** when the client doesn't already set it (cache reads ~0.1× cost). Per-model opt-out flag for relays that reject it.

### W3 — Token estimation & cache-aware accounting *(P1)*
1. Replace `len(body)/4` with a JSON-aware two-band estimator (within ~20%), raw estimate as fallback.
2. Thread provider-reported `cached_tokens` (OpenAI) / `cache_read_input_tokens` (Anthropic) / `cachedContentTokenCount` (Gemini) through `extractUsage` into shared `Usage{CachedTokens}`; `Reconcile` (`ratelimit.go:318-333`) charges `uncached = prompt - cached` toward TPM/TPD; add cached rate to `usage.Cost`.
3. Context-slot prefilter (`proxy.go:322-336`) uses the new estimator; skip-slot-but-continue-walk stays (`TestContextTooLongFallsToWiderSlot`); don't poison TPM reservation for slots never tried.

### W4 — Long-request hygiene *(P1)*
1. **Upstream inactivity watchdog** in `callStreaming` (`proxy.go:685-776`): per-chunk deadline (`stream_idle_timeout`, default ~300s) surfacing upstream error instead of hanging forever. Server stays without WriteTimeout (correct for streams).
2. SSE scanner cap 4 MiB → configurable (`providers.go:359-365`); bound unbounded `finish` read (`openai.go:306`) via `readAllLimited`.
3. Cut redundant body work: single `json.Valid` + one `decodeMap` peek reused downstream (kills 2 of ~4 copies per attempt).
4. Graceful shutdown drain aware of stream idle deadline rather than fixed 30s (`main.go:190`).

### W5 — Capability honesty & observability *(P2)*
1. Extend `Capability` (`providers.go:24-31`) with `Tools, Reasoning, Cache` (Gemini: all; OpenAI: tools+cache+reasoning; Anthropic after W2: tools+cache; compat: unknown→true, proved live).
2. `/v1/models` lists `supports_tools`/`supports_reasoning` as intersection-walk truth, not union; wire the dead `SupportsTools` probe in.
3. Add `X-Pico-Attempts`; always emit `X-Pico-Provider` (success paths too).
4. O(keys) auth scan → map lookup (`server.go:251-257`).

### Non-goals
No auth/HMAC redesign; no sliding-window ratelimit rewrite; no streaming request-body forwarding this tag; no dashboard.

---

## Sequencing
1. **W1** first (error taxonomy + walk fix).
2. **W2** in parallel (adapter-test-driven like `gemini_tools_test.go`).
3. **W3**, then **W4** (W4.3 shares paths with W3.1 — sequence).
4. **W5** last (needs W2's Capability fields).

## Tag gate
Cut `v0.5.0-AGENTIC-AGE` when the agent-shaped smoke passes per adapter (10-turn streaming tool loop, 200k-token context round trip, 5-min stream, cross-slot 429 failover) on **OpenAI, Gemini, Anthropic, openai-compat**. New tests: Anthropic tool round-trip + `input_json_delta` streaming, upstream-429-fails-over-walk, estimator accuracy on a ~1 MB tool-loop fixture, cached-token reconcile, stream-idle watchdog, oversized SSE frame.

## Open questions — status
1. Anthropic `tool_result` pairing: treated as validation item during W2; preserve ids exactly as sent, investigate only if the upstream complains.
2. `cache_control` injection: decided by **default** — inject by default, per-model opt-out.
3. `usage.Classify` change vs `TestRouterCooldownFallsThroughToNextSlot` (`proxy_test.go:396`): accepted, verified as a tag gate.


---
---

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
