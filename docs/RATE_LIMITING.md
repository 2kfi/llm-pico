# Rate Limiting

llm-pico enforces rate limits at two levels: **user-level** (per-key) and **model-level** (per-model). Both are checked atomically on every request.

## Window Types

| Window | Scope | Reset Period | Reserve Unit |
|--------|-------|-------------|--------------|
| `rpm` | Requests per minute | Next minute boundary (UTC) | 1 per request |
| `rpd` | Requests per day | Next midnight UTC | 1 per request |
| `tpm` | Tokens per minute | Next minute boundary (UTC) | `prompt_tokens + max_tokens` |
| `tpd` | Tokens per day | Next midnight UTC | `prompt_tokens + max_tokens` |
| `ash` | Audio seconds per hour | Next hour boundary (UTC) | 1 per request |
| `asd` | Audio seconds per day | Next midnight UTC | 1 per request |

## Configuration

### Per-Model Limits

Set in `config.yaml`:

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
    rpm: 100
    rpd: 10000
    tpm: 150000
    tpd: 10000000
    ash: 7200
    asd: 28800
```

### Per-Key Limits

Set in `users.yaml`:

```yaml
users:
  - key: "sk-pico-abc..."
    rpm: 50
    rpd: 5000
    tpm: 75000
    tpd: 5000000
```

### Per-User Limits

Via admin API:

```bash
PUT /admin/users/{user_id}/limits
{"rpm": 100, "rpd": 10000, "tpm": 150000, "tpd": 10000000}
```

### Per-Team Limits

Via admin API:

```bash
PUT /admin/teams/{team_id}/limits
{"rpm": 1000, "rpd": 100000, "tpm": 1500000, "tpd": 100000000}
```

## Merging

When a key is assigned to a user who belongs to a team:

- **Limits:** `min()` across key, user, and team (most restrictive wins)

```
Key:   rpm=100
User:  rpm=50
Team:  rpm=200
Final: rpm=50
```

## How It Works

### Two-Phase Check

Every request goes through a two-phase atomic check:

1. **Phase 1 (Validate):** Check ALL windows without committing
2. **Phase 2 (Commit):** Increment all counters

If any window is exceeded in Phase 1, the request is rejected immediately. No partial reservations.

### In-Memory Counters

Rate limit counters are stored in-memory for speed:

- Sharded by `hash(key_hash + "::" + model_name) % shard_count`
- Daily windows (`rpd`, `tpd`, `asd`) are flushed to SQLite every 60 seconds
- Minutely/hourly windows are purely in-memory (lost on restart)

### Streaming Reconciliation

For streaming responses:

1. Initial reservation uses estimated tokens (`prompt_tokens + max_tokens`)
2. After streaming completes, actual token count is extracted from SSE chunks
3. Delta is applied to `tpm`/`tpd` counters

This ensures accurate token counting even when the actual usage differs from the estimate.

## Token Budget Reservation

For streaming responses, the proxy reserves tokens before the request is forwarded:

1. **Reserve** `prompt_tokens + max_tokens` in `tpm`/`tpd` windows
2. **Forward** the request to the upstream provider
3. **Reconcile** after streaming completes — adjust `tpm`/`tpd` by the delta between estimated and actual tokens

This prevents over-committing tokens while ensuring accurate billing. If `max_tokens` is not set, a default reservation of 4096 is used.

## Budget Tracking

Per-user monthly budgets in USD:

### Configuration

Via admin API:

```bash
PUT /admin/users/{user_id}/budget
{"monthly_budget_usd": 100.00}
```

### How It Works

1. Before each request, estimate cost: `(prompt_tokens / 1M * cost_input) + (completion_tokens / 1M * cost_output)`
2. Query `usage_log` for total spend this month across all user's keys
3. If `current_spend + estimated_cost > budget`: reject with 429

### Cost Configuration

Set in `config.yaml`:

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
    cost_per_1m_input: 0.03
    cost_per_1m_output: 0.06
```

## Response Headers

All responses include rate limit headers:

```
X-RateLimit-rpm-Limit: 100
X-RateLimit-rpm-Remaining: 95
X-RateLimit-rpm-Reset: 1234567890

X-RateLimit-tpm-Limit: 150000
X-RateLimit-tpm-Remaining: 145000
X-RateLimit-tpm-Reset: 1234567890
```

| Header | Description |
|--------|-------------|
| `X-RateLimit-{window}-Limit` | Configured limit |
| `X-RateLimit-{window}-Remaining` | Remaining count |
| `X-RateLimit-{window}-Reset` | Unix timestamp of window reset |

## Error Responses

### Rate Limit Exceeded

```json
{
  "error": {
    "message": "Rate limit exceeded: rpm limit of 100 reached",
    "type": "rate_limit_exceeded",
    "code": 429
  }
}
```

With header: `Retry-After: 45`

### Budget Exceeded

```json
{
  "error": {
    "message": "Monthly budget of $100.00 exceeded. Current spend: $102.50",
    "type": "budget_exceeded",
    "code": 429
  }
}
```

## Retry-After Values

| Window | Retry-After |
|--------|-------------|
| rpm | 60 seconds |
| rpd | 86400 seconds (24 hours) |
| tpm | 60 seconds |
| tpd | 86400 seconds (24 hours) |
| ash | 3600 seconds (1 hour) |
| asd | 86400 seconds (24 hours) |

## Database Persistence

Daily windows are flushed to SQLite:

```sql
CREATE TABLE rate_counters (
    id INTEGER PRIMARY KEY,
    key_hash TEXT,
    model_name TEXT,
    level TEXT,        -- 'user' or 'model'
    window_type TEXT,  -- 'rpd', 'tpd', 'asd'
    window_start TEXT,
    count INTEGER,
    UNIQUE(key_hash, model_name, level, window_type, window_start)
);
```

Minutely/hourly windows are purely in-memory and lost on restart.

## Example: Complete Rate Limiting Setup

```yaml
# config.yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
    rpm: 100          # model-level
    rpd: 10000
    tpm: 150000
    tpd: 10000000
    cost_per_1m_input: 0.03
    cost_per_1m_output: 0.06

# users.yaml
users:
  - key: "sk-pico-abc..."
    rpm: 50           # key-level (more restrictive)
    rpd: 5000
    tpm: 75000
    tpd: 5000000

# Effective limits: rpm=50, rpd=5000, tpm=75000, tpd=5000000
```
