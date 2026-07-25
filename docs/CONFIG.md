# Configuration

`config.yaml` controls routing, rate limits, and provider settings.

## Structure

```yaml
general_settings:     # Global settings
router_settings:      # Routing and retry behavior
model_list:           # Array of model definitions (required, >=1)
```

## general_settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `master_key` | string | **required** | Admin API key. Full key, not hashed. Used for admin endpoints and admin auth. |
| `db_path` | string \| null | `null` | Override SQLite database path. Default: `llm-pico.db` next to config. |
| `usage_log_retention_days` | int | `30` | Auto-prune `usage_log` entries older than N days. |
| `admin_log_retention_days` | int | `90` | Auto-prune `admin_log` entries older than N days. |
| `degradation_mode` | string | `normal` | Global degradation mode: `normal`, `reject`, `queue`, `fallback_only`. Overridden per-request via admin API. |

```yaml
general_settings:
  master_key: "sk-pico-master-abc123..."
  usage_log_retention_days: 30
  admin_log_retention_days: 90
  degradation_mode: normal
```

## router_settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `num_retries` | int | `2` | Retries per provider before failover. Total attempts = num_retries + 1. |
| `cooldown_time` | int | `45` | Seconds for provider cooldown (used as fallback; actual cooldown is progressive: 10s × 3, then 30s). |
| `circuit_breaker` | object | see below | Circuit breaker configuration. |

### circuit_breaker

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Enable/disable circuit breaker. |
| `failure_threshold` | int | `3` | Consecutive 5xx errors before circuit opens. |
| `recovery_timeout` | int | `30` | Seconds before circuit transitions to HALF_OPEN for probing. |

```yaml
router_settings:
  num_retries: 2
  cooldown_time: 45
  circuit_breaker:
    enabled: true
    failure_threshold: 3
    recovery_timeout: 30
```

## model_list

Array of model definitions. **At least one entry required.**

### ModelEntry fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model_name` | string | **required** | User-facing model name (what clients send in requests). |
| `model_params` | object | **required** | Provider routing parameters (see below). |
| `rpm` | int \| null | `null` | Requests per minute limit. |
| `rpd` | int \| null | `null` | Requests per day limit. |
| `tpm` | int \| null | `null` | Tokens per minute limit. |
| `tpd` | int \| null | `null` | Tokens per day limit. |
| `ash` | int \| null | `null` | Audio seconds per hour limit (STT/TTS). |
| `asd` | int \| null | `null` | Audio seconds per day limit (STT/TTS). |
| `images` | bool | `false` | Model supports image input. |
| `embeddings` | bool | `false` | Model supports embeddings. |
| `stt` | bool | `false` | Model supports speech-to-text. |
| `tts` | bool | `false` | Model supports text-to-speech. |
| `failover_model` | string \| null | `null` | Model name to failover to after all retries exhausted. Single level only. |
| `can_cache` | bool | `false` | Enable in-memory LRU response caching. |
| `cost_per_1m_input` | float \| null | `null` | Cost per 1M input tokens (USD). Used for budget tracking. |
| `cost_per_1m_output` | float \| null | `null` | Cost per 1M output tokens (USD). Used for budget tracking. |
| `chain_budget_usd` | float \| null | `null` | Budget cap for chain-of-LLMs requests on this model. |
| `chain_rewrites_response` | string \| null | `null` | Team-level instruction for rewriting the user prompt when `model_chain` is active. |

### model_params

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model` | string | **required** | Provider-prefixed model string: `<provider>/<model-id>`. |
| `api_key` | string \| list \| null | `null` | API key source. See [Key Management](KEYS.md). |
| `api_base` | string \| null | `null` | API base URL override. Provider default used if null. |

```yaml
model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
      api_base: "https://api.openai.com/v1"
    rpm: 50
    rpd: 5000
    tpm: 150000
    images: true
    cost_per_1m_input: 0.03
    cost_per_1m_output: 0.06

  - model_name: claude-3
    model_params:
      model: anthropic/claude-3-sonnet-20240229
      api_key: "KEYS/ANTHROPIC_API_KEY"
    failover_model: gpt-4
```

## Config Resolution Pipeline

When `config.yaml` is loaded, values are resolved in this order:

1. **YAML parse** — `yaml.safe_load()`
2. **KEYS/ENV resolution** — `KEYS/XXX` references load from `keys.yaml`; `ENV/XXX` references load from `os.environ`
3. **Environment variable resolution** — `${VAR}` or `${VAR:-default}` syntax resolved from `os.environ`

### Key reference types

| Syntax | Source | Result | Rotation? |
|--------|--------|--------|-----------|
| `"KEYS/OPENAI_API_KEY"` | `keys.yaml` | `list[str]` | Yes (round-robin) |
| `"ENV/OPENAI_API_KEY"` | `os.environ` | `str` | No (single key) |
| `"sk-abc123..."` | Literal | `str` | No (single key) |
| `"${OPENAI_API_KEY}"` | `os.environ` | `str` | No (legacy syntax) |

See [Key Management](KEYS.md) for details.

## Validation Rules

- Config must be a YAML dictionary
- `model_list` must have at least 1 entry
- `general_settings.master_key` is required (non-empty)
- `api_base` containing "UNSET" raises an error (Cloudflare check)
- STT/TTS on unsupported providers (Anthropic, Gemini) logs a warning

## Environment Variables

Use `${VAR_NAME}` or `${VAR_NAME:-default}` syntax anywhere in config:

```yaml
general_settings:
  master_key: "${LLM_PICO_MASTER_KEY}"

model_list:
  - model_params:
      api_key: "${OPENAI_API_KEY}"
```

Resolution order: `KEYS/` → `ENV/` → `${VAR}`

## Example: Minimal Config

```yaml
general_settings:
  master_key: "sk-pico-master-abc123"

model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
```

## Example: Multi-Provider with Failover

```yaml
general_settings:
  master_key: "sk-pico-master-abc123"

router_settings:
  num_retries: 2
  circuit_breaker:
    failure_threshold: 3
    recovery_timeout: 30

model_list:
  - model_name: gpt-4
    model_params:
      model: openai/gpt-4
      api_key: "KEYS/OPENAI_API_KEY"
    rpm: 100
    cost_per_1m_input: 0.03
    cost_per_1m_output: 0.06
    failover_model: claude-3

  - model_name: claude-3
    model_params:
      model: anthropic/claude-3-sonnet-20240229
      api_key: "KEYS/ANTHROPIC_API_KEY"
    rpm: 50

  - model_name: gemini-flash
    model_params:
      model: gemini/gemini-3-flash-preview
      api_key: "KEYS/GEMINI_API_KEY"
    rpm: 200
```
