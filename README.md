# llm-pico

![llm-pico](./assets/banner.jpg)

**Every provider. Round-robin keys. One SQLite database.**

Hyper-lightweight LLM proxy with an OpenAI-compatible API. No config files — everything lives in SQLite and is managed from the web dashboard or admin API.

```bash
pip install llm-pico
llm-pico --master-key sk-pico-my-secret    # first boot sets the master key
# open http://localhost:4000/admin/dashboard  → add models + API keys
```

## Quick Start

```bash
# 1. Install
pip install llm-pico

# 2. Start with a master key (first boot only)
llm-pico --master-key sk-pico-change-me

# 3. Open the dashboard
open http://localhost:4000/admin/dashboard

# 4. Add models and provider keys via the Models page
# 5. Create user API keys via the Keys page
```

Then use it like any OpenAI endpoint:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-pico-<user-key>" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4", "messages": [{"role": "user", "content": "hello"}]}'
```

## v2.0 Features

### Smart Routing
- **Model alias resolution** — use `gpt-4` instead of `openai/gpt-4`
- **Latency-aware routing** — picks the fastest provider automatically
- **Cost-aware routing** — respects budget limits, preflight cost estimates
- **Weighted round-robin** — EMA-based failure weighting
- **Smart fallback chains** — cascading failover across providers

### Streaming
- **Zero-copy streaming** — direct pipe from provider to client
- **Stream cancellation** — abort propagates upstream
- **Usage reconciliation** — token counts extracted from streaming deltas
- **SSE heartbeat** — keepalive during long generations

### Security
- **API key scopes** — `read`, `write`, `admin` permissions
- **IP allowlist per key** — CIDR notation support
- **HMAC request signing** — optional request verification
- **Structured audit logging** — IP attribution, nullable columns

### Cost & Budget
- **Per-key monthly budgets** — hard block when exceeded
- **Real-time cost projection** — estimate before sending
- **Provider cost comparison** — cross-provider pricing
- **Token budget reservations** — atomic reserve-reconcile

### Observability
- **Error taxonomy** — retryable vs permanent classification
- **Slow request profiler** — timing breakdown per stage
- **Prompt/response sampling** — configurable capture
- **Prometheus metrics** — `/admin/metrics` endpoint

### Dashboard
- **Alpine.js SPA** — modern reactive UI
- **Dark mode** — toggle with system preference
- **Model playground** — test models directly
- **Import/export config** — JSON backup/restore
- **Bulk operations** — multi-select actions
- **Keyboard shortcuts** — `?` for help

### Architecture
- **Universal OpenAI-compat adapter** — any OpenAI-compatible provider
- **Hot config reload** — no restart needed
- **Database migrations** — versioned schema upgrades
- **Graceful degradation** — continue on provider failures
- **Multi-process workers** — `--workers N` flag

## First Boot

On first start, `llm-pico` needs a master key. Provide it one of:

```bash
# Option A: CLI flag
llm-pico --master-key sk-pico-my-secret

# Option B: Environment variable
export LLM_PICO_MASTER_KEY=sk-pico-my-secret
llm-pico
```

The master key is stored in SQLite and never needs to be set again. All subsequent configuration is done via the admin API or dashboard.

## Configuration

Everything is in SQLite. No YAML, no JSON files.

| What | Where |
|------|-------|
| Master key, router settings | `settings` table → Config page in dashboard |
| Models + rate limits + costs | `models` table → Models page in dashboard |
| Provider API keys (rotation) | `provider_keys` table → per-model key list |
| User API keys | `user_keys` table → Keys page |
| Teams + users | `teams` / `users` tables → Teams page |

### Hot Reload

After changing config via the admin API, click **Reload Router** in the dashboard or call:

```bash
curl -X POST http://localhost:4000/admin/config/reload \
  -H "Authorization: Bearer sk-pico-my-secret"
```

No restart needed — the router rebuilds in-memory.

## Docker

```bash
docker build -t llm-pico .
docker run -p 4000:4000 \
  -e LLM_PICO_MASTER_KEY=sk-pico-change-me \
  -v ./data:/app/data \
  llm-pico
```

## Admin API

All endpoints require `Authorization: Bearer <master-key>`.

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/admin/config` | Read all settings |
| PUT | `/admin/config/settings` | Update settings |
| GET | `/admin/config/models` | List models + provider keys |
| POST | `/admin/config/models` | Create model |
| PUT | `/admin/config/models/{id}` | Update model |
| DELETE | `/admin/config/models/{id}` | Delete model |
| POST | `/admin/config/models/{id}/keys` | Add provider key |
| DELETE | `/admin/config/keys/{id}` | Remove provider key |
| POST | `/admin/config/reload` | Hot-reload router |
| GET/POST | `/admin/keys` | List / create user API keys |
| DELETE | `/admin/keys/{prefix}` | Revoke user key |
| GET/POST | `/admin/teams` | List / create teams |
| POST | `/admin/teams/{id}/users` | Create user in team |
| GET | `/admin/usage` | Usage stats |
| GET | `/admin/stats/costs` | Cost breakdown |
| GET | `/admin/metrics` | Prometheus-format metrics |
| GET | `/admin/errors` | Error taxonomy breakdown |
| GET | `/admin/sampling` | Recent prompt/response samples |
| GET | `/admin/logs` | Structured audit log stream |

## Documentation

| Topic | Description |
|-------|-------------|
| [API Endpoints](docs/ENDPOINTS.md) | Every proxy endpoint with examples |
| [Admin API](docs/ADMIN.md) | Full admin API reference |
| [Providers](docs/PROVIDERS.md) | Built-in providers, custom adapters |
| [Authentication](docs/AUTH.md) | Master key, API keys, model allowlists |
| [Rate Limiting](docs/RATE_LIMITING.md) | 6-window limits, budgets, headers |
| [Routing](docs/ROUTING.md) | Key rotation, circuit breaker, failover |
| [Deployment](docs/DEPLOYMENT.md) | Docker, production, graceful shutdown |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | Common errors and fixes |

## Contributing

```bash
git clone https://github.com/your-org/llm-pico
cd llm-pico
python -m venv .venv && source .venv/bin/activate
pip install -e ".[dev]"
pytest
```

## License

MIT
