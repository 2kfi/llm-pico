# llm-pico — Full Implementation Plan

> **Version**: v2.0 Roadmap
> **Last Updated**: 2025-07-20
> **Philosophy**: Perfection over options, knife not Swiss Army knife. Zero external dependencies by design.

---

## Current State Summary (v1.0 Complete)

### Architecture
- `api/` — FastAPI routes, HTTP layer
- `core/` — Business logic (no HTTP imports)
- `providers/` — Lazy-loaded provider adapters
- `website/` — SPA dashboard (single index.html)
- `tests/` — 67 tests, all passing

### Features Implemented
- OpenAI-compatible API (chat, completions, embeddings, audio STT/TTS)
- Round-robin key rotation with circuit breaker (3 states: CLOSED → OPEN → HALF_OPEN)
- SQLite config (no YAML at runtime, `${ENV_VAR}` interpolation)
- Teams → Users → Keys hierarchy
- 6-window rate limiting (RPM, TPM, RPD, TPD, ASH, ASD) — all in-memory with periodic flush
- Per-user monthly budgets (hard block)
- In-memory LRU cache (256 entries, SHA-256 body key)
- Streaming support (SSE)
- Provider adapters: OpenAI, Anthropic, Gemini, Cloudflare (+ fallback for OpenAI-compat)
- Cost tracking (per-model input/output rates)
- Live SSE log stream
- Graceful drain + restart

### Provider Matrix
| Slug | Adapter | Images | Embeddings | STT | TTS |
|------|---------|--------|------------|-----|-----|
| `openai` | OpenAIAdapter | ✅ | ✅ | ✅ | ✅ |
| `anthropic` | AnthropicAdapter | ✅ | ❌ | ❌ | ❌ |
| `gemini` | GeminiAdapter | ✅ | ✅ | ❌ | ❌ |
| `cloudflare` | CloudflareAdapter | ❌ | ✅ | ❌ | ❌ |
| Unknown slug | OpenAIAdapter (fallback) | - | - | - | - |

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

## v2.0 Improvement Plan

### Category 1: Routing & Model Intelligence

#### 1.1 Model Alias Resolution
**Priority**: P0 (Must Have)
**Effort**: Small

**Problem**: Users must configure exact model names (`openai/gpt-4`). Adding a new model requires knowing the provider slug.

**Solution**: Support alias resolution. `gpt-4` → auto-resolves to best available provider (OpenAI/Groq/Azure) based on latency/cost/availability.

```python
# New: model_aliases table
CREATE TABLE IF NOT EXISTS model_aliases (
    alias       TEXT PRIMARY KEY,
    model_name  TEXT NOT NULL,
    priority    INTEGER DEFAULT 0
);

# Resolution: alias → model_name → router.resolve()
def resolve_alias(alias: str) -> str:
    # Check aliases table first
    # Fuzzy match if exact not found
    return alias if alias in model_names else fuzzy_match(alias)
```

**UX Impact**: Users just say "gpt-4" and the router picks the optimal backend.

---

#### 1.2 Semantic Model Matching
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Typo `gpt4`, alias `gpt-4-turbo`, shorthand `claude3` don't resolve.

**Solution**: Fuzzy match using edit distance + common aliases map.

```python
import difflib

MODEL_ALIASES = {
    "gpt4": "gpt-4",
    "gpt4o": "gpt-4o",
    "gpt-4-turbo": "gpt-4-turbo-2024-04-09",
    "claude3": "claude-3-sonnet-20240229",
    "claude3haiku": "claude-3-haiku-20240307",
    "gemini-pro": "gemini-1.5-pro",
    "gemini-flash": "gemini-1.5-flash",
}

def fuzzy_match_model(query: str, available: list[str]) -> str | None:
    """Match query to model name using aliases and fuzzy matching."""
    # Exact match
    if query in available:
        return query
    
    # Alias lookup
    if query.lower() in MODEL_ALIASES:
        alias = MODEL_ALIASES[query.lower()]
        if alias in available:
            return alias
    
    # Fuzzy match
    matches = difflib.get_close_matches(query, available, n=1, cutoff=0.6)
    return matches[0] if matches else None
```

**UX Impact**: Handles typos, aliases, and shorthand. "gpt4" → "gpt-4", "claude3" → "claude-3-sonnet".

---

#### 1.3 Smart Fallback Chains
**Priority**: P0 (Must Have)
**Effort**: Medium

**Problem**: Current `failover_model` is a single model. If that model's provider is also down, no fallback.

**Solution**: Replace single failover with priority-ordered fallback list.

```yaml
# Before (v1)
model_list:
  - model_name: gpt-4
    failover_model: claude-3  # single failover

# After (v2)
model_list:
  - model_name: gpt-4
    fallbacks:  # ordered list, tries each until success
      - model: groq/llama-3-70b
        priority: 1
      - model: anthropic/claude-3-sonnet
        priority: 2
```

**Router change**: `_proxy_request` loops through `fallbacks[]` instead of single `failover_model`.

---

#### 1.4 Latency-Aware Routing
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Round-robin doesn't account for provider latency. Fast provider gets same traffic as slow one.

**Solution**: Track p50/p95 latency per provider-group. Route to fastest healthy provider.

```python
# Add to ProviderGroup
@dataclass
class ProviderGroup:
    # ... existing fields ...
    latency_p50: float = 0.0
    latency_p95: float = 0.0
    latency_samples: list[float] = field(default_factory=list)  # last 100

    def record_latency(self, ms: float):
        self.latency_samples.append(ms)
        if len(self.latency_samples) > 100:
            self.latency_samples.pop(0)
        sorted_samples = sorted(self.latency_samples)
        self.latency_p50 = sorted_samples[len(sorted_samples) // 2]
        self.latency_p95 = sorted_samples[int(len(sorted_samples) * 0.95)]
```

**Expose**: `/admin/stats/latency` endpoint, dashboard heatmap.

---

#### 1.5 Cost-Aware Routing
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: Multiple providers may serve same model (OpenAI vs Azure GPT-4). No cost optimization.

**Solution**: When multiple provider-groups serve same model, route to cheapest healthy provider. User config sets `max_cost_per_1k_tokens` budget.

```python
def resolve(self, model_name: str, max_cost: float | None = None):
    groups = self._model_map.get(model_name, [])
    healthy = [g for g in groups if g.circuit_breaker.is_request_allowed()]
    
    if max_cost:
        # Filter to providers within budget
        healthy = [g for g in healthy if g.cost_per_1k <= max_cost]
    
    # Sort by cost (cheapest first) or latency (fastest first)
    healthy.sort(key=lambda g: g.cost_per_1k)
    
    # Round-robin within cheapest tier
    return self._pick_from_groups(healthy)
```

---

#### 1.6 Weighted Round-Robin
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Simple round-robin gives equal traffic to all keys/providers regardless of performance.

**Solution**: Weight-based routing: `weight = 1 / (latency_p95 * cost_per_token)`. Keys with better perf/cost get more traffic.

```python
def _pick_weighted(self, group: ProviderGroup) -> KeyState:
    """Pick key with probability proportional to weight."""
    weights = []
    for key in group.keys:
        if key.cooldown_until < time.monotonic():
            # Weight based on recent success rate
            w = 1.0 / (1.0 + key.fails * 0.1)
            weights.append((key, w))
        else:
            weights.append((key, 0))
    
    total = sum(w for _, w in weights)
    if total == 0:
        return None
    
    r = random.random() * total
    for key, w in weights:
        r -= w
        if r <= 0:
            return key
    return weights[-1][0]
```

---

#### 1.7 Model Capability Registry
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Capabilities (tools, vision, json_mode) must be manually configured per model.

**Solution**: Auto-detect via probe requests on startup. Store in DB.

```sql
CREATE TABLE IF NOT EXISTS model_capabilities (
    model_id        INTEGER REFERENCES models(id),
    supports_tools  INTEGER DEFAULT 0,
    supports_vision INTEGER DEFAULT 0,
    supports_json   INTEGER DEFAULT 0,
    supports_stream INTEGER DEFAULT 1,
    max_context     INTEGER,
    max_output      INTEGER,
    probed_at       TEXT,
    PRIMARY KEY (model_id)
);
```

**Probe logic**: On startup, send minimal request to each model to detect capabilities.

---

#### 1.8 Provider Health Scoring
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Circuit breaker is binary (OPEN/CLOSED). No gradient of health.

**Solution**: Composite score: `availability * (1/error_rate) * (1/latency) * cost_factor`.

```python
@dataclass
class ProviderGroup:
    # ... existing fields ...
    health_score: float = 1.0
    
    def update_health(self):
        avail = 1.0 - (self.error_count / max(1, self.total_requests))
        latency_factor = 1000.0 / max(1.0, self.latency_p50)
        self.health_score = avail * latency_factor * self.cost_factor
```

**Dashboard**: Health heatmap showing provider status at a glance.

---

### Category 2: Streaming & Real-Time

#### 2.1 True Streaming Proxy (Zero-Copy)
**Priority**: P0 (Must Have)
**Effort**: Large

**Problem**: Current implementation buffers chunks in `proxy_stream()`, then yields from list. Adds memory overhead and latency.

**Solution**: Stream bytes directly from upstream to client without buffering.

```python
async def _handle_streaming_direct(adapter, body_bytes, model_string, ...):
    upstream = await adapter.proxy_request_stream(body_bytes, model_string)
    
    async def generate():
        async for chunk in upstream.aiter_bytes():
            # Parse usage from chunk if present
            if b"usage" in chunk:
                _parse_usage_from_chunk(chunk)
            yield chunk  # Forward directly, no buffer
        yield b"data: [DONE]\n\n"
    
    return StreamingResponse(generate(), media_type="text/event-stream")
```

**Trade-off**: Slightly more complex usage tracking, but 10x lower memory for large streams.

---

#### 2.2 Stream Cancellation Propagation
**Priority**: P0 (Must Have)
**Effort**: Medium

**Problem**: When client disconnects mid-stream, upstream keeps generating tokens wastefully.

**Solution**: Detect client disconnect, cancel upstream request immediately.

```python
async def _handle_streaming(adapter, body_bytes, ...):
    upstream = await adapter.proxy_request_stream(body_bytes, ...)
    
    async def generate():
        try:
            async for chunk in upstream.aiter_bytes():
                yield chunk
        except (asyncio.CancelledError, GeneratorExit):
            await upstream.aclose()  # Cancel upstream
            raise
    
    return StreamingResponse(generate(), ...)
```

**Impact**: Saves provider quota & money on abandoned streams.

---

#### 2.3 Streaming Usage Reconciliation
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Current: estimates tokens, reconciles after stream ends. Inaccurate for long streams.

**Solution**: Parse usage from stream chunks in real-time (Anthropic/Gemini send usage in stream).

```python
async def _parse_stream_usage(chunk: bytes) -> dict | None:
    """Extract usage from SSE chunk if present."""
    if b'"usage"' not in chunk:
        return None
    try:
        text = chunk.decode("utf-8", errors="replace")
        for line in text.split("\n"):
            if line.startswith("data: ") and "[DONE]" not in line:
                data = json.loads(line[6:])
                if "usage" in data:
                    return data["usage"]
    except (json.JSONDecodeError, UnicodeDecodeError):
        pass
    return None
```

**Update rate limit counters incrementally during stream.**

---

#### 2.4 SSE Heartbeat / Keepalive
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: Long streams (30s+) get cut off by load balancers/proxies that expect data.

**Solution**: Send `: keepalive\n\n` every 15s during idle periods.

```python
async def _stream_with_heartbeat(adapter, body_bytes, ...):
    upstream = await adapter.proxy_request_stream(body_bytes, ...)
    
    async def generate():
        async def _stream_chunks():
            async for chunk in upstream.aiter_bytes():
                yield chunk
        
        # Merge stream with heartbeat
        async for chunk in merge(
            _stream_chunks(),
            heartbeat(interval=15)
        ):
            yield chunk
    
    return StreamingResponse(generate(), ...)
```

---

#### 2.5 Partial Response Caching
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Streaming responses aren't cached. Repeated identical requests hit provider.

**Solution**: Cache by prompt hash. On cache hit, replay stream from cache.

```python
async def _handle_streaming_cached(adapter, body_bytes, model_name, ...):
    cache_key = hashlib.sha256(body_bytes).hexdigest()
    
    if cache_key in _stream_cache:
        # Replay cached stream
        async def replay():
            for chunk in _stream_cache[cache_key]:
                yield chunk
        return StreamingResponse(replay(), ...)
    
    # Cache the stream
    _stream_cache[cache_key] = []
    async def generate():
        async for chunk in upstream.aiter_bytes():
            _stream_cache[cache_key].append(chunk)
            yield chunk
    
    return StreamingResponse(generate(), ...)
```

---

#### 2.6 Stream Multiplexing
**Priority**: P2 (Nice to Have)
**Effort**: Large

**Problem**: Popular prompts (e.g., "What is 2+2?") hit provider for every client independently. Wastes quota.

**Solution**: Single upstream connection serves multiple downstream clients requesting same model+params. Fan-out SSE.

```python
class StreamMultiplexer:
    """Share upstream stream across multiple clients."""
    
    def __init__(self):
        self._active: dict[str, asyncio.Event] = {}  # prompt_hash → Event
        self._subscribers: dict[str, list[asyncio.Queue]] = {}
    
    async def subscribe(self, prompt_hash: str) -> asyncio.Queue:
        q = asyncio.Queue(maxsize=256)
        
        if prompt_hash not in self._active:
            # First subscriber — start upstream
            self._active[prompt_hash] = asyncio.Event()
            self._subscribers[prompt_hash] = [q]
        else:
            # Join existing stream
            self._subscribers[prompt_hash].append(q)
        
        return q
    
    async def fan_out(self, prompt_hash: str, chunk: bytes):
        """Forward chunk to all subscribers."""
        for q in self._subscribers.get(prompt_hash, []):
            try:
                q.put_nowait(chunk)
            except asyncio.QueueFull:
                pass  # Drop slow consumers
    
    async def unsubscribe(self, prompt_hash: str, q: asyncio.Queue):
        """Remove subscriber. If last, close upstream."""
        subs = self._subscribers.get(prompt_hash, [])
        if q in subs:
            subs.remove(q)
        if not subs:
            del self._active[prompt_hash]
            del self._subscribers[prompt_hash]
```

**Impact**: Reduces provider costs for repeated queries. Popular prompts cached in-flight.

---

### Category 3: Security & Auth

#### 3.1 API Key Scopes/Permissions
**Priority**: P0 (Must Have)
**Effort**: Medium

**Problem**: All keys have full access. CI keys can do admin operations.

**Solution**: Keys get scopes: `chat:read`, `chat:write`, `embeddings`, `audio:stt`, `audio:tts`, `admin:read`, `admin:write`.

```sql
CREATE TABLE IF NOT EXISTS api_key_scopes (
    key_id  INTEGER REFERENCES user_keys(id),
    scope   TEXT NOT NULL,
    PRIMARY KEY (key_id, scope)
);

-- Enforce at route level
def require_scope(scope: str):
    async def checker(user_key: dict = Depends(require_api_key)):
        if scope not in user_key.get("scopes", []):
            raise HTTPException(status_code=403, detail="Missing scope")
        return user_key
    return checker

# Usage
@app.post("/v1/chat/completions")
async def chat(user_key: dict = Depends(require_scope("chat:write"))):
    ...
```

---

#### 3.2 IP Allowlist per Key
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: Leaked key = full access from anywhere.

**Solution**: Optional CIDR list per API key.

```sql
ALTER TABLE user_keys ADD COLUMN ip_allowlist TEXT;  -- JSON array of CIDRs
```

```python
def check_ip(key: dict, client_ip: str):
    if not key.get("ip_allowlist"):
        return True  # No restriction
    allowed = json.loads(key["ip_allowlist"])
    return any(ipaddress.ip_address(client_ip) in ipaddress.ip_network(cidr) 
               for cidr in allowed)
```

---

#### 3.3 Key Rotation with Grace Period
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Key rotation = downtime unless all clients update simultaneously.

**Solution**: Create new key, keep old valid for N days (configurable). Auto-expire old.

```sql
CREATE TABLE IF NOT EXISTS key_rotation (
    old_key_id  INTEGER REFERENCES user_keys(id),
    new_key_id  INTEGER REFERENCES user_keys(id),
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    status      TEXT DEFAULT 'active'
);
```

**Dashboard**: Shows "rotating" status with countdown.

---

#### 3.4 Request Signing (HMAC)
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: No MITM/replay protection without TLS (reverse proxy handles TLS, but extra layer helps).

**Solution**: Optional `X-Signature: sha256=...` header.

```python
import hmac
import hashlib

def verify_signature(key: str, body: bytes, signature: str):
    expected = hmac.new(key.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(f"sha256={expected}", signature)
```

---

#### 3.5 Audit Log Enhancements
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Current `admin_log` is basic. No structured audit trail.

**Solution**: Structured JSON audit log with before/after values.

```python
def audit_log(action: str, actor: str, ip: str, before: dict, after: dict):
    """Structured audit log entry."""
    entry = {
        "ts": datetime.utcnow().isoformat(),
        "action": action,
        "actor": actor,
        "ip": ip,
        "before": before,
        "after": after,
    }
    _write_to_audit_table(entry)
    _write_to_audit_file(entry)  # Optional: append to JSONL file
```

---

### Category 4: Cost & Budget

#### 4.1 Per-Key Budgets
**Priority**: P0 (Must Have)
**Effort**: Medium

**Problem**: Current: user-level monthly budget only. CI key gets same budget as dev key.

**Solution**: Budget per key + per user + per team (hard limit).

```sql
ALTER TABLE user_keys ADD COLUMN monthly_budget_usd REAL;
ALTER TABLE user_keys ADD COLUMN hard_limit INTEGER DEFAULT 1;
```

```python
async def check_budget(key: dict, estimated_cost: float):
    # Check key-level budget
    if key.get("monthly_budget_usd"):
        spend = await get_key_spend(key["key_id"])
        if spend + estimated_cost > key["monthly_budget_usd"]:
            return "Key budget exceeded"
    
    # Check user-level budget (existing)
    if key.get("user_id") and key.get("monthly_budget_usd"):
        spend = await get_user_spend(key["user_id"])
        if spend + estimated_cost > key["monthly_budget_usd"]:
            return "User budget exceeded"
    
    return None
```

---

#### 4.2 Real-time Cost Projection
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: No visibility into monthly burn rate until it's too late.

**Solution**: `/admin/stats/cost-projection` → extrapolates current burn rate to month-end.

```python
async def cost_projection():
    """Project month-end cost based on current burn rate."""
    today = datetime.utcnow().day
    days_in_month = calendar.monthrange(
        datetime.utcnow().year, datetime.utcnow().month
    )[1]
    
    current_spend = await get_month_spend()
    daily_rate = current_spend / max(1, today)
    projected = daily_rate * days_in_month
    
    return {
        "current_spend": current_spend,
        "days_elapsed": today,
        "daily_rate": daily_rate,
        "projected_total": projected,
        "days_remaining": days_in_month - today,
    }
```

---

#### 4.3 Provider Cost Comparison
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: No visibility into cost differences across providers for same model.

**Solution**: Dashboard table: Model × Provider → cost/1M tokens, latency, availability.

```sql
SELECT 
    m.model_name,
    pk.provider_slug,
    AVG(ul.cost_usd / NULLIF(ul.total_tokens, 0)) * 1000000 as cost_per_m,
    AVG(ul.latency_ms) as avg_latency,
    COUNT(*) as request_count
FROM usage_log ul
JOIN models m ON ul.model_name = m.model_name
GROUP BY m.model_name, pk.provider_slug
ORDER BY m.model_name, cost_per_m;
```

---

#### 4.4 Budget Alerts (Webhook)
**Priority**: P2 (Nice to Have)
**Effort**: Small

**Problem**: Admin only sees budget status in dashboard. No proactive alerts.

**Solution**: Configurable webhook at 80%/90%/100% budget thresholds.

```yaml
# In settings
budget_alerts:
  enabled: true
  webhook_url: "https://hooks.slack.com/..."
  thresholds: [0.8, 0.9, 1.0]
```

```python
async def check_budget_alerts(user_id: int, spend: float, budget: float):
    if not budget or budget <= 0:
        return
    
    percentage = spend / budget
    for threshold in _alert_thresholds:
        if percentage >= threshold:
            await _send_webhook({
                "user_id": user_id,
                "spend": spend,
                "budget": budget,
                "threshold": threshold,
            })
            break
```

---

#### 4.5 Token Budget Reservations
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Current: reserve `prompt + max_tokens` upfront for streaming. Extend to model-level + team-level budgets.

**Solution**: Multi-level token budget reservations with reconciliation.

```python
async def reserve_tokens(
    key_hash: str,
    model_name: str,
    user_id: int | None,
    team_id: int | None,
    reservation: int,
) -> str | None:
    """Reserve tokens across all budget levels. Returns error if exceeded."""
    
    # Key-level budget
    key_budget = await get_key_budget(key_hash)
    if key_budget:
        key_spend = await get_key_spend(key_hash)
        if key_spend + reservation > key_budget:
            return f"Key budget exceeded: {key_spend + reservation}/{key_budget}"
    
    # User-level budget
    if user_id:
        user_budget = await get_user_budget(user_id)
        if user_budget:
            user_spend = await get_user_spend(user_id)
            if user_spend + reservation > user_budget:
                return f"User budget exceeded: {user_spend + reservation}/{user_budget}"
    
    # Team-level budget
    if team_id:
        team_budget = await get_team_budget(team_id)
        if team_budget:
            team_spend = await get_team_spend(team_id)
            if team_spend + reservation > team_budget:
                return f"Team budget exceeded: {team_spend + reservation}/{team_budget}"
    
    return None  # All budgets OK

async def reconcile_tokens(key_hash: str, model_name: str, user_id: int | None, 
                           team_id: int | None, actual_tokens: int, reserved: int):
    """Reconcile actual vs reserved tokens across all levels."""
    delta = actual_tokens - reserved
    if delta != 0:
        await update_spend(key_hash, model_name, delta)
        if user_id:
            await update_user_spend(user_id, delta)
        if team_id:
            await update_team_spend(team_id, delta)
```

**Impact**: Prevents overspend across all hierarchy levels. Streaming requests reserve upfront, reconcile after.

---

### Category 5: Observability & Debugging

#### 5.1 OpenTelemetry Integration
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: No distributed tracing. Hard to debug latency issues across providers.

**Solution**: Optional OTel exporter (stdout/OTLP/Jaeger). No deps if disabled.

```python
# core/telemetry.py
_TRACER = None

def init_tracing(endpoint: str | None = None):
    global _TRACER
    if not endpoint:
        return  # No-op if not configured
    
    from opentelemetry import trace
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
    
    provider = TracerProvider()
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint)))
    trace.set_tracer_provider(provider)
    _TRACER = trace.get_tracer("llm-pico")

def trace_request(model: str, provider: str):
    """Context manager for request tracing."""
    if not _TRACER:
        return contextlib.nullcontext()
    return _TRACER.start_as_current_span("llm_request", attributes={
        "model": model,
        "provider": provider,
    })
```

---

#### 5.2 Request Tracing UI
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Debugging requires checking logs, DB, provider responses separately.

**Solution**: Dashboard: search by request_id, see full waterfall.

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

---

#### 5.3 Provider Error Taxonomy
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: Upstream errors are just HTTP status codes. No classification.

**Solution**: Classify errors: `rate_limit`, `invalid_key`, `quota_exceeded`, `model_overloaded`, `content_filter`, `timeout`, `unknown`.

```python
def classify_error(status_code: int, body: dict) -> str:
    if status_code == 429:
        return "rate_limit"
    if status_code == 401:
        return "invalid_key"
    if status_code == 402:
        return "quota_exceeded"
    if status_code == 503:
        return "model_overloaded"
    if status_code == 504:
        return "timeout"
    # Check response body for content filter
    if "content_filter" in str(body):
        return "content_filter"
    return "unknown"
```

**Expose**: `/admin/stats/errors` endpoint with counts per provider/model.

---

#### 5.4 Slow Request Profiler
**Priority**: P2 (Nice to Have)
**Effort**: Small

**Problem**: No visibility into performance regressions.

**Solution**: Flag requests > p99 latency. Log: model, provider, tokens, prompt hash (truncated).

```python
async def log_slow_request(request_id: str, latency_ms: int, model: str, provider: str):
    p99 = await get_p99_latency(model, provider)
    if latency_ms > p99 * 1.5:  # 50% over p99
        _log.warning("slow request %s: %dms (p99=%dms) model=%s provider=%s",
                     request_id, latency_ms, p99, model, provider)
        await _store_slow_request(request_id, latency_ms, model, provider)
```

---

#### 5.5 Prompt/Response Sampling
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: No visibility into model quality/output.

**Solution**: Optional: sample N% of requests, store truncated prompt+response in DB.

```python
async def maybe_sample_request(request_id: str, prompt: str, response: str, model: str):
    sampling_rate = _settings.get("sampling_rate", 0.0)  # 0.0 to 1.0
    if random.random() > sampling_rate:
        return
    
    await _store_sample({
        "request_id": request_id,
        "model": model,
        "prompt_hash": hashlib.sha256(prompt.encode()).hexdigest()[:16],
        "prompt_preview": prompt[:200],  # First 200 chars
        "response_preview": response[:200],
        "created_at": datetime.utcnow().isoformat(),
    })
```

**GDPR**: Configurable retention (default 7 days), auto-prune.

---

#### 5.6 WebSocket Dashboard
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: SSE log stream is unidirectional. No filtering.

**Solution**: WebSocket with bidirectional filtering.

```python
@app.websocket("/admin/logs/ws")
async def logs_ws(websocket: WebSocket):
    await websocket.accept()
    filters = await websocket.receive_json()  # {"model": "gpt-4", "status": 200}
    
    q = subscribe()
    try:
        while True:
            payload = await q.get()
            event = json.loads(payload)
            if _matches_filters(event, filters):
                await websocket.send_json(event)
    finally:
        unsubscribe(q)
```

---

### Category 6: UX & Dashboard

#### 6.1 Modern Dashboard (Alpine.js)
**Priority**: P1 (High Value)
**Effort**: Large

**Problem**: Single 70KB HTML file is hard to maintain. No component reuse.

**Solution**: Alpine.js (~15KB) for reactivity. No build step required.

```html
<!-- Before: inline state management -->
<script>
  function loadModels() {
    fetch('/admin/config/models').then(r => r.json()).then(data => {
      document.getElementById('models').innerHTML = data.map(m => `...`).join('');
    });
  }
</script>

<!-- After: Alpine.js declarative -->
<div x-data="{ models: [], loading: true }" x-init="models = await fetch('/admin/config/models').then(r => r.json())">
  <template x-for="model in models" :key="model.id">
    <div class="model-card" x-text="model.model_name"></div>
  </template>
</div>
```

**Benefits**: Declarative, reactive, no build step, 15KB bundle.

---

#### 6.2 Model Playground
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Testing requires external tools (curl, Postman, etc.).

**Solution**: In-dashboard chat UI to test any configured model.

```
┌─────────────────────────────────────────┐
│ Model: [gpt-4        ▼] Temperature: 0.7│
├─────────────────────────────────────────┤
│ User: Hello, how are you?               │
│ Assistant: I'm doing well, thanks!      │
│                                         │
│ User: What's 2+2?                       │
│ Assistant: 4                            │
├─────────────────────────────────────────┤
│ Latency: 1,234ms | Tokens: 45 | $0.004 │
│ [Copy as curl] [Export as markdown]     │
└─────────────────────────────────────────┘
```

---

#### 6.3 Visual Routing Graph
**Priority**: P2 (Nice to Have)
**Effort**: Large

**Problem**: Mental model of routing is hard. No visualization.

**Solution**: Canvas-based graph: Models → Provider Groups → Keys. Color-coded health.

```
┌─────────────────────────────────────────────────────┐
│                    Models                            │
│  ┌──────┐  ┌──────┐  ┌──────┐                       │
│  │gpt-4 │  │claude│  │gemini│                       │
│  └──┬───┘  └──┬───┘  └──┬───┘                       │
│     │         │         │                            │
│  ┌──┴─────────┴─────────┴──┐                        │
│  │     Provider Groups     │                        │
│  └──────────┬──────────────┘                        │
│         ┌───┴───┐                                   │
│         ▼       ▼                                   │
│    ┌────────┐ ┌────────┐                           │
│    │OpenAI  │ │Groq    │                           │
│    │✓ Healthy│ │⚠ Slow  │                           │
│    └────────┘ └────────┘                           │
└─────────────────────────────────────────────────────┘
```

---

#### 6.4 Import/Export Config
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: No backup/migration path for config.

**Solution**: YAML/JSON export of all models, keys, teams, limits. Import with diff preview.

```python
@app.get("/admin/export")
async def export_config():
    """Export full config as YAML."""
    models = await get_all_models()
    keys = await get_all_keys()
    teams = await get_all_teams()
    return Response(
        content=yaml.dump({"models": models, "keys": keys, "teams": teams}),
        media_type="text/yaml"
    )

@app.post("/admin/import")
async def import_config(file: UploadFile, dry_run: bool = False):
    """Import config with optional diff preview."""
    data = yaml.safe_load(await file.read())
    diff = compute_diff(data)
    if dry_run:
        return {"diff": diff}
    return await apply_import(data)
```

---

#### 6.5 Bulk Operations
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: No way to select multiple items for batch actions.

**Solution**: Select multiple keys/models → bulk enable/disable, set limits, assign team.

```python
@app.post("/admin/bulk/keys")
async def bulk_key_operation(operation: dict):
    """Bulk operations on multiple keys."""
    key_ids = operation["key_ids"]
    action = operation["action"]  # "enable", "disable", "set_limits", "assign_team"
    
    if action == "enable":
        await db.execute("UPDATE user_keys SET is_active = 1 WHERE id IN ({})".format(
            ",".join("?" * len(key_ids))), key_ids)
    elif action == "disable":
        await db.execute("UPDATE user_keys SET is_active = 0 WHERE id IN ({})".format(
            ",".join("?" * len(key_ids))), key_ids)
    # ... etc
```

---

#### 6.6 Dark Mode + Accessibility
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: No dark mode, poor keyboard navigation.

**Solution**: WCAG AA compliant, keyboard navigation, screen reader support.

```css
/* Dark mode via prefers-color-scheme */
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #1a1a2e;
    --text: #eee;
    --primary: #e94560;
  }
}

/* Keyboard navigation */
:focus-visible {
  outline: 2px solid var(--primary);
  outline-offset: 2px;
}

/* Skip to main content */
.skip-link {
  position: absolute;
  left: -10000px;
}
.skip-link:focus {
  left: 0;
}
```

---

#### 6.7 Keyboard Shortcuts
**Priority**: P2 (Nice to Have)
**Effort**: Small

**Problem**: No power-user shortcuts.

**Solution**: `Cmd+K` command palette, `g m` → models, `g k` → keys, `/` search.

```javascript
// Keyboard shortcut system
const shortcuts = {
  'Cmd+k': () => openCommandPalette(),
  'g m': () => navigateTo('/admin/config/models'),
  'g k': () => navigateTo('/admin/keys'),
  'g t': () => navigateTo('/admin/teams'),
  '/': () => focusSearch(),
};

document.addEventListener('keydown', (e) => {
  const combo = `${e.metaKey ? 'Cmd+' : ''}${e.key}`;
  if (shortcuts[combo]) {
    e.preventDefault();
    shortcuts[combo]();
  }
});
```

---

### Category 7: Provider Ecosystem

#### 7.1 Universal OpenAI-Compatible Adapter
**Priority**: P1 (High Value)
**Effort**: Small

**Problem**: Each provider needs a dedicated adapter. Many providers speak OpenAI-compatible API.

**Solution**: Single adapter that works with ANY OpenAI-compatible endpoint.

```python
@register("openai-compat")
class OpenAICompatAdapter(BaseAdapter):
    """Universal adapter for OpenAI-compatible endpoints.
    
    Works with: Groq, Together, Fireworks, DeepInfra, vLLM, Ollama, LM Studio,
    Anyscale, Replicate, SambaNova, and 50+ others.
    """
    
    def __init__(self, provider_slug: str = "openai-compat", 
                 api_key: str | None = None, api_base: str | None = None):
        super().__init__(provider_slug, api_key, api_base)
    
    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        super()._set_auth_headers(headers)
        headers["Authorization"] = f"Bearer {self.api_key}"
    
    def _base_url(self) -> str:
        return (self.api_base or "https://api.openai.com/v1").rstrip("/")
    
    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        url = f"{self._base_url()}/chat/completions"
        return await self.client.post(url, content=body_bytes, headers=self._headers())
    
    # Inherits all other methods from BaseAdapter
```

**Usage**: Add any OpenAI-compatible provider with just `api_base` and `api_key`.

---

#### 7.2 Provider Capability Probing
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: Capabilities must be manually configured per model.

**Solution**: Auto-probe on model add. Detect: `supports_tools`, `supports_vision`, `supports_json_mode`, `max_context`, `max_output`.

```python
async def probe_model_capabilities(adapter, model: str) -> dict:
    """Probe model capabilities via minimal requests."""
    capabilities = {
        "supports_tools": False,
        "supports_vision": False,
        "supports_json_mode": False,
        "supports_stream": True,
        "max_context": None,
        "max_output": None,
    }
    
    # Test tools support
    try:
        response = await adapter.proxy_request(
            json.dumps({
                "model": model,
                "messages": [{"role": "user", "content": "test"}],
                "tools": [{"type": "function", "function": {"name": "test", "parameters": {}}}],
            }).encode(),
            model
        )
        if response.status_code == 200:
            capabilities["supports_tools"] = True
    except:
        pass
    
    # Test JSON mode
    try:
        response = await adapter.proxy_request(
            json.dumps({
                "model": model,
                "messages": [{"role": "user", "content": "test"}],
                "response_format": {"type": "json_object"},
            }).encode(),
            model
        )
        if response.status_code == 200:
            capabilities["supports_json_mode"] = True
    except:
        pass
    
    return capabilities
```

---

#### 7.3 Model Registry Sync
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Adding new models requires manual entry.

**Solution**: Periodic sync from provider model lists (OpenAI, Anthropic, Gemini, OpenRouter). Auto-suggest in dashboard.

```python
async def sync_provider_models(provider: str):
    """Sync available models from provider."""
    adapter = get_adapter(provider)()
    try:
        response = await adapter.client.get(f"{adapter._base_url()}/models")
        if response.status_code == 200:
            models = response.json()["data"]
            for model in models:
                await suggest_model(provider, model["id"])
    except:
        pass

# Dashboard shows "Add" button for suggested models
```

---

#### 7.4 Custom Provider SDK
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Adding a new provider requires modifying core code. No easy extensibility.

**Solution**: Simple Python interface for custom providers. Drop-in file in `providers/custom/`. Hot-reload on change.

```python
# providers/custom/my_provider.py
from providers.base import BaseAdapter
from providers import register

@register("my_provider")
class MyProviderAdapter(BaseAdapter):
    provider = "my_provider"
    supports_images = True
    supports_embeddings = False
    
    def _set_auth_headers(self, headers: dict[str, str]) -> None:
        super()._set_auth_headers(headers)
        headers["X-API-Key"] = self.api_key
    
    def _base_url(self) -> str:
        return (self.api_base or "https://api.myprovider.com/v1").rstrip("/")
    
    async def proxy_request(self, body_bytes: bytes, model_string: str) -> httpx.Response:
        url = f"{self._base_url()}/chat/completions"
        return await self.client.post(url, content=body_bytes, headers=self._headers())
```

**Hot-reload**: Monitor `providers/custom/` directory. On file change, reload module:

```python
import importlib
import watchfiles

async def watch_custom_providers():
    """Hot-reload custom providers on file change."""
    async for changes in watchfiles.awatch("providers/custom/"):
        for change_type, path in changes:
            if path.endswith(".py"):
                module_name = f"providers.custom.{path.stem}"
                if module_name in sys.modules:
                    importlib.reload(sys.modules[module_name])
                    _log.info("reloaded custom provider: %s", module_name)
```

**Impact**: Extensibility without forking. Users drop a file, restart optional.

---

### Category 8: Architecture & Operations

#### 8.1 Hot Config Reload (No Restart)
**Priority**: P0 (Must Have)
**Effort**: Large

**Problem**: Current: graceful drain + `os.execve()` restart. Connections dropped.

**Solution**: Incremental reload — add/remove models, update keys, change limits without dropping connections.

```python
class ConfigReloader:
    """Incremental config reload without restart."""
    
    def __init__(self, config: Config, router: Router):
        self.config = config
        self.router = router
    
    async def reload_models(self):
        """Add/remove/update models without restart."""
        new_config = await load_config_from_db()
        
        # Diff models
        old_models = {m.model_name for m in self.config.model_list}
        new_models = {m.model_name for m in new_config.model_list}
        
        added = new_models - old_models
        removed = old_models - new_models
        # updated = models that exist in both but changed
        
        # Update router incrementally
        for model_name in added:
            self.router.add_model(new_config.get_model(model_name))
        
        for model_name in removed:
            self.router.remove_model(model_name)
        
        # Update existing models (keys, limits, etc.)
        for model_name in new_models - removed:
            self.router.update_model(new_config.get_model(model_name))
        
        self.config = new_config
```

---

#### 8.2 Database Migration System
**Priority**: P1 (High Value)
**Effort**: Medium

**Problem**: No versioned schema migrations. Manual ALTER TABLE in code.

**Solution**: Versioned SQL migrations (like Alembic but zero-dep).

```python
# core/migrations.py
MIGRATIONS = [
    # v1.0 → v1.1
    {
        "version": 1,
        "sql": """
            CREATE TABLE IF NOT EXISTS model_aliases (
                alias       TEXT PRIMARY KEY,
                model_name  TEXT NOT NULL,
                priority    INTEGER DEFAULT 0
            );
        """,
    },
    # v1.1 → v1.2
    {
        "version": 2,
        "sql": """
            ALTER TABLE user_keys ADD COLUMN monthly_budget_usd REAL;
            ALTER TABLE user_keys ADD COLUMN hard_limit INTEGER DEFAULT 1;
        """,
    },
    # ... more migrations
]

async def run_migrations():
    """Apply pending migrations on startup."""
    async with get_db() as db:
        # Get current version
        try:
            cursor = await db.execute("SELECT MAX(version) FROM schema_version")
            current_version = cursor.fetchone()[0] or 0
        except:
            current_version = 0
        
        # Apply pending
        for migration in MIGRATIONS:
            if migration["version"] > current_version:
                await db.executescript(migration["sql"])
                await db.execute(
                    "INSERT OR REPLACE INTO schema_version (version, applied_at) VALUES (?, ?)",
                    (migration["version"], datetime.utcnow().isoformat())
                )
                await db.commit()
                _log.info("Applied migration v%d", migration["version"])
```

---

#### 8.3 Graceful Degradation Modes
**Priority**: P2 (Nice to Have)
**Effort**: Medium

**Problem**: Under overload, all requests fail equally.

**Solution**: Configurable degradation: `reject` | `queue` | `fallback_only`.

```python
class DegradationManager:
    """Handle overload with configurable degradation."""
    
    def __init__(self, mode: str = "reject"):
        self.mode = mode
        self._queue: asyncio.Queue = asyncio.Queue(maxsize=1000)
    
    async def handle_request(self, request: Request, handler: Callable):
        if self.mode == "reject":
            return await handler(request)
        
        elif self.mode == "queue":
            try:
                self._queue.put_nowait((request, handler))
                return await self._queue.get()  # Wait for slot
            except asyncio.QueueFull:
                raise HTTPException(status_code=503, detail="Server overloaded")
        
        elif self.mode == "fallback_only":
            # Only serve fallback models
            model_name = request.json().get("model")
            if not self._is_fallback_model(model_name):
                raise HTTPException(status_code=503, detail="Primary models unavailable")
            return await handler(request)
```

---

#### 8.4 Multi-Process Workers
**Priority**: P2 (Nice to Have)
**Effort**: Large

**Problem**: Single process can only use 1 CPU core.

**Solution**: N worker processes behind internal load balancer (SO_REUSEPORT).

```python
# api/cli.py
@click.option("--workers", type=int, default=1, help="Number of worker processes")
def main(workers: int):
    if workers > 1:
        # Use uvicorn with workers
        uvicorn.run("api.server:app", host="0.0.0.0", port=4000, workers=workers)
    else:
        # Single process (current)
        uvicorn.run("api.server:app", host="0.0.0.0", port=4000)
```

**Trade-off**: Requires Redis for cross-process rate limit sync (optional). Single-process remains default.

---

## Prioritization Matrix

| Priority | Items | Rationale |
|----------|-------|-----------|
| **P0 (Must Have)** | 1.1, 1.2, 2.1, 2.2, 3.1, 4.1, 8.1 | Core routing intelligence, streaming correctness, security basics, zero-downtime ops |
| **P1 (High Value)** | 1.3, 1.4, 1.5, 1.6, 2.3, 2.4, 3.2, 3.3, 4.2, 4.3, 5.1, 5.2, 5.3, 6.1, 6.2, 6.4, 7.1, 7.2, 8.2 | Smart routing, cost control, dashboard UX, provider ecosystem |
| **P2 (Nice to Have)** | 1.7, 2.5, 3.4, 3.5, 4.4, 5.4, 5.5, 5.6, 6.3, 6.5, 6.6, 6.7, 7.3, 8.3, 8.4 | Advanced features, polish, scaling |

---

## Key Decisions Needed

### 1. Dashboard Tech Stack
| Option | Size | Pros | Cons |
|--------|------|------|------|
| **Vanilla HTML/JS** (current) | ~70KB | Zero deps, fast load | Hard to maintain, no component reuse |
| **Alpine.js** | ~15KB | Declarative, reactive, no build | Slight learning curve |
| **htmx** | ~14KB | Server-driven, simple | Less interactive |
| **Preact** | ~3KB | React-like, tiny | Need build step |

**Recommendation**: Alpine.js — best balance of simplicity and power.

---

### 2. OTel vs Custom Metrics
| Option | Size | Pros | Cons |
|--------|------|------|------|
| **OpenTelemetry** | ~2MB | Industry standard, Jaeger/Zipkin | Large dep |
| **Custom /metrics** | ~50 lines | Zero deps, Prometheus format | Non-standard |

**Recommendation**: Custom `/metrics` for zero-dep, OTel as optional extra.

---

### 3. Multi-Process
| Option | Complexity | Pros | Cons |
|--------|------------|------|------|
| **Single process** (current) | Low | Simple, SQLite works | Single core only |
| **Multi-process** | High | Multi-core | Requires Redis for rate limits |

**Recommendation**: Keep single process default. Target hardware is Atom D410 (1 core). Multi-process only if moving to multi-core.

---

### 4. Provider Strategy
| Option | Scope | Pros | Cons |
|--------|-------|------|------|
| **Universal adapter** | Everything OpenAI-compat | 50+ providers, zero code | May miss provider-specific features |
| **Deep integration** | Top 10 providers | Best support for each | More maintenance |

**Recommendation**: Universal adapter first, deep integration for Anthropic/Gemini (they have unique APIs).

---

### 5. Budget Granularity
| Option | Complexity | Pros | Cons |
|--------|------------|------|------|
| **Per-user only** (current) | Low | Simple | CI key gets same budget as dev |
| **Per-key + per-user + per-team** | Medium | Granular control | More config to manage |

**Recommendation**: Per-key budgets (P0). Users who need granular control get it; simple deployments stay simple.

---

### 6. Streaming Priority
| Option | Complexity | Pros | Cons |
|--------|------------|------|------|
| **Buffered** (current) | Low | Simple, works | Memory overhead, latency |
| **Zero-copy** | High | 10x lower memory, lower latency | More complex usage tracking |

**Recommendation**: Zero-copy streaming (P0). Essential for production at scale.

---

## v2.0 Milestones

| Milestone | Focus | Est. Effort | Items |
|-----------|-------|-------------|-------|
| **v2.0-alpha** | Routing intelligence, True streaming, Security, Budgets | 3-4 weeks | 1.1, 1.2, 2.1, 2.2, 3.1, 4.1, 8.1 |
| **v2.0-beta** | Dashboard, Universal adapter, Observability, Hot reload | 3-4 weeks | 6.1, 6.2, 6.4, 7.1, 7.2, 5.1, 5.2, 5.3 |
| **v2.0-rc** | Polish, Advanced features, Migration, Docs | 2 weeks | 1.3, 1.4, 1.5, 2.3, 2.4, 3.2, 3.3, 6.3, 6.5, 6.6, 6.7, 8.2 |
| **v2.0** | Hardening, Load testing, Migration guide from v1 | 1 week | Testing, documentation, release |

---

## Test Coverage Goals (v2.0)

| Component | v1.0 Tests | v2.0 Target | Notes |
|-----------|------------|-------------|-------|
| Router | 7 | 15 | Add: alias resolution, weighted round-robin, latency tracking |
| Streaming | 0 (manual) | 8 | Add: zero-copy, cancellation, usage reconciliation, heartbeat |
| Security | 5 | 12 | Add: scopes, IP allowlist, HMAC, key rotation |
| Cost | 5 | 8 | Add: per-key budgets, projection, alerts |
| Dashboard | 0 (manual) | 5 | Add: import/export, bulk ops, accessibility |
| Providers | 4 | 10 | Add: universal adapter, capability probing |

**Target**: 100+ tests, all passing, <500ms total runtime.

---

## Migration Guide (v1.0 → v2.0)

### Breaking Changes
1. **Config format**: `failover_model` → `fallbacks[]` (migration script provided)
2. **Database schema**: New tables added via migrations (auto-run on startup)
3. **API changes**: None — fully backward compatible

### Migration Steps
1. Backup SQLite database
2. Update `pip install llm-pico --upgrade`
3. Start `llm-pico` — migrations run automatically
4. Update config if using `failover_model` (optional — legacy still works)
5. Verify dashboard loads, keys work, models accessible

### Rollback Plan
1. Stop v2.0
2. Restore SQLite backup
3. Downgrade: `pip install llm-pico==1.0.0`
4. Start v1.0

---

## Appendix: File Changes Summary

### New Files
```
core/
├── migrations.py          # Database migration system
├── telemetry.py           # OTel integration (optional)
├── degradation.py         # Graceful degradation modes
├── aliases.py             # Model alias resolution
├── probing.py             # Provider capability probing
├── budget.py              # Budget management (per-key/user/team)
└── cost_projection.py     # Real-time cost projection

providers/
├── openai_compat.py       # Universal OpenAI-compatible adapter

api/
├── websocket.py           # WebSocket endpoints for dashboard
├── bulk.py                # Bulk operations API
├── export.py              # Config import/export
└── playground.py          # Model playground API

website/static/
├── index.html             # Alpine.js rewrite (or keep vanilla with Alpine.js)
├── components/            # Component templates (if using Alpine.js)
└── shortcuts.js           # Keyboard shortcuts
```

### Modified Files
```
core/
├── router.py              # Add: weighted round-robin, latency tracking, health scoring
├── config.py              # Add: fallbacks[], monthly_budget_usd per key
├── db.py                  # Add: migration runner, new tables
├── auth.py                # Add: scopes, IP allowlist
├── ratelimit.py           # Add: streaming reconciliation
├── usage.py               # Add: cost projection, sampling
└── events.py              # Add: WebSocket support

api/
├── server.py              # Add: zero-copy streaming, cancellation propagation
├── admin.py               # Add: bulk ops, export, metrics, errors endpoint
└── dependencies.py        # Add: require_scope, check_ip

providers/
├── base.py                # Add: probe_capabilities()
└── openai.py              # Minor: streaming improvements

tests/
├── test_router.py         # +8 tests: alias, weighted, latency, health
├── test_streaming.py      # +8 tests: zero-copy, cancellation, heartbeat
├── test_security.py       # +7 tests: scopes, IP, HMAC, rotation
├── test_cost.py           # +3 tests: per-key, projection, alerts
└── test_dashboard.py      # +5 tests: import/export, bulk, accessibility
```

---

**Total estimated effort**: 8-10 weeks (full-time)
**Minimum viable v2.0**: 3-4 weeks (P0 items only)
