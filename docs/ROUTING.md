# Routing

llm-pico routes requests to upstream providers using round-robin key rotation with automatic failover.

## Request Flow

```
Client Request
      ↓
  Router.resolve(model_name)
      ↓
  Pick ProviderGroup (provider + api_base)
      ↓
  Round-robin through keys
      ↓
  Skip cooled-down keys
      ↓
  Return (ProviderGroup, KeyState, ModelEntry)
      ↓
  Forward to upstream API
      ↓
  On success: record_success
  On failure: record_failure → retry with next key
```

## Key Rotation

### Round-Robin

Keys are served in order, cycling through the list:

```
Request 1 → key[0]
Request 2 → key[1]
Request 3 → key[2]
Request 4 → key[0]  (back to start)
```

### Implementation

Each `ProviderGroup` has a `next_key_index` counter:

```python
idx = group.next_key_index % len(group.keys)
group.next_key_index += 1
key = group.keys[idx]
```

If the selected key is on cooldown, the next key is tried (up to all keys).

## Rate Limit Handling

### Progressive Cooldown

When a key gets rate-limited (429):

| Failures | Cooldown |
|----------|----------|
| 1st | 10 seconds |
| 2nd | 10 seconds |
| 3rd | 10 seconds |
| 4th+ | 30 seconds |

After cooldown expires, the key rejoins the rotation.

### All Keys Exhausted

When all keys for a provider are on cooldown:

1. Calculate earliest recovery time
2. Raise `HTTPException(429)` with `Retry-After` header
3. Client should wait and retry

```json
{
  "error": {
    "message": "All keys for model 'gpt-4' are rate-limited. Retry after 29s.",
    "type": "rate_limit_exceeded",
    "code": 429
  }
}
```

## Invalid Keys

| Status | Meaning | Behavior |
|--------|---------|----------|
| 401 | Invalid/expired key | Marked invalid for 1 year |
| 403 | No access to model | Marked invalid for 1 year |
| 402 | Insufficient funds | Marked invalid for 1 year |

Invalid keys are never used again until the proxy restarts.

## Circuit Breaker

The circuit breaker protects against cascading failures.

### States

```
CLOSED → OPEN → HALF_OPEN → CLOSED
  ↑                           ↓
  └───────────────────────────┘
```

**CLOSED:** Normal operation. Failures are counted.

**OPEN:** After `failure_threshold` (default 3) consecutive 5xx errors. All requests rejected.

**HALF_OPEN:** After `recovery_timeout` (default 30 seconds). Allows one probe request.

- On success → CLOSED (circuit resets)
- On failure → OPEN (circuit re-opens)

### Configuration

```yaml
router_settings:
  circuit_breaker:
    enabled: true
    failure_threshold: 3
    recovery_timeout: 30
```

### Behavior

1. Request fails with 5xx → circuit records failure
2. After 3 consecutive failures → circuit opens
3. All requests rejected for 30 seconds
4. After 30 seconds → circuit enters HALF_OPEN
5. One probe request allowed
6. If probe succeeds → circuit closes
7. If probe fails → circuit re-opens

## Provider Grouping

Keys are grouped by `(provider_slug, api_base)`. This means:

- Multiple `model_list` entries with the same provider and base URL share a key pool
- Different base URLs create separate groups

Example:

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_KEY"
      api_base: "https://api.openai.com/v1"

  - model_name: gpt-4-alt
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_KEY_ALT"
      api_base: "https://my-proxy.com/v1"  # Different base = separate group
```

## Failover

If all retries are exhausted and `failover_model` is configured:

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_KEY"
    failover_model: claude-3
```

The proxy will:

1. Try `gpt-4` with all keys and retries
2. If all fail, try `claude-3` (single level, no chain)

**Only one level of failover** — no recursive failover chains.

## Retry Logic

For each attempt:

1. `router.resolve(model_name)` → pick a key
2. Forward request to upstream
3. On success → return response
4. On failure:
   - **400/401/403/404/501** → raise immediately (no retry)
   - **429** → cooldown key, retry with next key
   - **5xx** → circuit breaker records failure, retry with next key
   - **httpx errors** → retry as 502

Total attempts: `num_retries + 1` (default: 3 attempts)

## Configuration Reference

```yaml
router_settings:
  num_retries: 2                    # Retries per provider
  cooldown_time: 45                 # Fallback cooldown (actual is progressive)
  circuit_breaker:
    enabled: true
    failure_threshold: 3            # 5xx before circuit opens
    recovery_timeout: 30            # Seconds before probing
```

## Example: Multi-Provider Failover

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_KEY"
    rpm: 100
    failover_model: claude-3

  - model_name: claude-3
    model_params:
      model: anthropic/claude-3-sonnet-20240229
      api_key: "KEYS/ANTHROPIC_KEY"
    rpm: 50
```

**Flow:**

1. Request for `gpt-4`
2. Try OpenAI key 1 → 429 (cooldown 10s)
3. Try OpenAI key 2 → success
4. If all OpenAI keys fail → try `claude-3` (Anthropic)
