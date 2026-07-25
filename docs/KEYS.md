# Key Management

llm-pico supports three ways to set API keys, with automatic rotation for backup keys.

## Key Reference Types

### `KEYS/` — Multi-key rotation (recommended)

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
```

Keys are stored in `keys.yaml`:

```yaml
OPENAI_API_KEY:
  - "sk-key1"
  - "sk-key2"
  - "sk-key3"
```

**How it works:**
- Requests rotate through keys in order: key1 → key2 → key3 → key1 → ...
- If a key gets rate-limited (429), it's skipped for 10s (first 3 times) or 30s (after that)
- If all keys are rate-limited, the proxy returns `429` with a `Retry-After` header
- If a key is invalid (401/403) or has no funds (402), it's marked invalid for 1 year

**Adding backup keys:**

```yaml
OPENAI_API_KEY:
  - "sk-key1"      # primary
  - "sk-key2"      # backup
  - "sk-key3"      # backup
```

### `ENV/` — Single key from environment

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "ENV/OPENAI_API_KEY"
```

Set the env var:

```bash
export OPENAI_API_KEY="sk-..."
```

**How it works:**
- Single key, no rotation
- Good for simple setups or when keys are managed externally (Docker secrets, Kubernetes, etc.)

### Literal string — Direct key

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "sk-abc123..."
```

**How it works:**
- Single key, no rotation
- Not recommended for production (key visible in config)

## Rotation Behavior

### Round-Robin

Keys are served in order, cycling through the list:

```
Request 1 → key[0]
Request 2 → key[1]
Request 3 → key[2]
Request 4 → key[0]  (back to start)
```

### Rate Limit Handling (429)

When a key gets rate-limited:

1. Key enters cooldown: **10s** for first 3 failures, **30s** after that
2. Next request skips the cooled-down key, picks the next one
3. When cooldown expires, key rejoins the rotation

### All Keys Exhausted

When all keys for a provider are on cooldown:

- Proxy returns `429 Too Many Requests`
- Response includes `Retry-After: N` header (seconds until first key recovers)
- Client should wait and retry

Example response:

```json
{
  "error": {
    "message": "All keys for model 'gpt-4' are rate-limited. Retry after 29s.",
    "type": "rate_limit_exceeded",
    "code": 429
  }
}
```

### Invalid Keys (401/403/402)

| Status | Meaning | Behavior |
|--------|---------|----------|
| 401 | Invalid/expired key | Marked invalid for 1 year |
| 403 | No access to model | Marked invalid for 1 year |
| 402 | Insufficient funds | Marked invalid for 1 year |

Invalid keys are never used again until the proxy restarts.

## keys.yaml Format

```yaml
OPENAI_API_KEY:
  - "sk-key1"
  - "sk-key2"

ANTHROPIC_API_KEY:
  - "sk-ant-key1"

GEMINI_API_KEY:
  - "AIza..."
```

**Rules:**
- Top-level keys are environment variable names (uppercase, underscored)
- Values are lists of strings (even for single keys)
- Lists can have any length (1 = no rotation, 2+ = rotation)
- The proxy reads `keys.yaml` at startup (not hot-reloaded)

## Team & User Hierarchy

Keys can be assigned to users, who belong to teams. Limits and allowlists merge across all three levels:

```
Team → User → Key → Effective
```

**Limits:** `min()` across all non-null values (most restrictive wins)

```
Team:  rpm=200
User:  rpm=100
Key:   rpm=50
Final: rpm=50
```

**Allowlists:** `intersection()` of all non-null lists

```
Team:  [gpt-4, claude-3, gemini]
User:  [gpt-4, gemini]
Key:   [gpt-4, claude-3]
Final: [gpt-4]
```

Set any level to `null` to mean "unrestricted" (doesn't constrain).

## Budget Tracking

Per-user and per-team monthly budgets in USD:

```yaml
# Via admin API
PUT /admin/users/{user_id}/budget
{"monthly_budget_usd": 100.00}

PUT /admin/teams/{team_id}/limits
{"monthly_budget_usd": 500.00}
```

When `current_spend + estimated_cost > budget`, the request is rejected with 429.

## IP Allowlist

Keys can be restricted to specific IPs or CIDR ranges:

```json
{
  "ip_allowlist": ["192.168.1.0/24", "10.0.0.1"]
}
```

Requests from IPs not in the list are rejected with 403. Set to `null` or `[]` to allow all.

## Migration from `${VAR}` Syntax

Old configs using `${OPENAI_API_KEY}` still work (resolved from `.env`).

To migrate:

1. Run `llm-pico init` to generate new config
2. Or manually change `api_key: "${OPENAI_API_KEY}"` to `api_key: "KEYS/OPENAI_API_KEY"`
3. Create `keys.yaml` with your keys

## Examples

### Basic setup with rotation

**config.yaml:**
```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
      api_base: "https://api.openai.com/v1"
```

**keys.yaml:**
```yaml
OPENAI_API_KEY:
  - "sk-primary"
  - "sk-backup1"
  - "sk-backup2"
```

### Mixed approach

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"      # rotation
  - model_name: claude-3
    model_params:
      model: anthropic/claude-3
      api_key: "ENV/ANTHROPIC_API_KEY"    # single env var
  - model_name: llama-3
    model_params:
      model: groq/llama-3
      api_key: "sk-groq-key"              # literal
```

## File Structure

```
.
├── config.yaml      # Model config (references KEYS/XXX or ENV/XXX)
├── keys.yaml        # API keys for rotation (KEYS/XXX)
├── users.yaml       # User API keys
├── .env             # Environment variables (ENV/XXX)
└── docker-compose.yml
```
