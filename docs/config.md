# llm-pico Configuration Reference

Complete reference for all configuration options. Covers `config.yaml`, `users.yaml`, CLI flags, environment variable substitution, and runtime behavior.

---

## 1. Overview

llm-pico is configured via two YAML files and CLI arguments:

| File | Purpose | Default path |
|------|---------|-------------|
| `config.yaml` | Models, routing, general settings | `./config.yaml` |
| `users.yaml` | User API keys and per-user limits | `./users.yaml` (auto-detected in same dir as config) |

Both files support `${ENV_VAR}` syntax for secrets. All values are resolved at startup.

---

## 2. CLI Flags

```
llm-pico [OPTIONS]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config` | path | `config.yaml` | Path to the main config file. Must exist. |
| `--users` | path | auto-detected | Path to users YAML file. If unset, looks for `users.yaml` or `users.yml` in the config file's directory. |
| `--db` | path | auto-detected | Path to SQLite database file. If unset, uses `llm-pico.db` in the config file's directory. |
| `--host` | string | `0.0.0.0` | Listen address. |
| `--port` | int | `4000` | Listen port. |
| `--verbose` / `-v` | flag | off | Enable debug logging. Silences httpx/aiosqlite/uvicorn access logs. |
| `--version` | flag | — | Print version and exit. |

**Auto-detection order for `--users`:**
1. Explicit `--users` path
2. `<config_dir>/users.yaml`
3. `<config_dir>/users.yml`
4. No users loaded (admin API only)

**Auto-detection order for `--db`:**
1. Explicit `--db` path
2. `<config_dir>/llm-pico.db`

---

## 3. Config File Location

The config file is loaded via `--config` (default: `config.yaml` in the working directory). The file must exist; the process exits if it's missing.

The config directory (parent of the config file) is used to resolve:
- Default users file path
- Default database path

Example with explicit paths:
```bash
llm-pico --config /etc/llm-pico/config.yaml --users /etc/llm-pico/users.yaml --db /var/lib/llm-pico/llm-pico.db
```

---

## 4. Environment Variable Syntax

Any string value in `config.yaml` or `users.yaml` can reference environment variables.

### Syntax

| Pattern | Behavior |
|---------|----------|
| `${VAR_NAME}` | Replace with env var. **Error** if unset and no default. |
| `${VAR_NAME:-default}` | Replace with env var. Falls back to `default` if unset. |

### Rules

- Resolution happens **once at startup** — no runtime re-evaluation.
- Only **string values** are resolved. Integers, booleans, lists, and dicts pass through unchanged.
- The `${}` pattern must match the **entire string**. Partial substitution is not supported (`"prefix-${VAR}-suffix"` won't work — use `"prefix-${VAR}"` or construct at runtime).
- Env vars are resolved **after** YAML parsing, so the YAML structure is intact.

### Examples

```yaml
general_settings:
  master_key: "${LLM_PICO_MASTER_KEY}"
  db_path: "${DATA_DIR:-/data}/llm-pico.db"

model_list:
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openai/gpt-5.4-mini"
      api_key: "${OPENAI_API_KEY}"

  - model_name: "gemini-flash"
    model_params:
      model: "gemini/gemini-3-flash-preview"
      api_key: "${GEMINI_API_KEY:-}"
```

**Users file:**
```yaml
users:
  - key: "${USER_KEY_1}"
    label: "team-alpha"
  - key: "${USER_KEY_2}"
    models: ["gpt-5.4-mini"]
```

---

## 5. General Settings

```yaml
general_settings:
  master_key: "sk-pico-master-CHANGE-ME"    # REQUIRED
  db_path: null                               # optional
  usage_log_retention_days: 30                # optional
  admin_log_retention_days: 90                # optional
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `master_key` | string | **required** | Admin authentication key. Used for all `/admin/*` endpoints. Use `${ENV_VAR}` in production. Must be non-empty. |
| `db_path` | string \| null | `null` | Path to SQLite database. When `null`, defaults to `llm-pico.db` in the config directory (or `--db` override). |
| `usage_log_retention_days` | int | `30` | Prune usage log entries older than this many days. Runs periodically. |
| `admin_log_retention_days` | int | `90` | Prune admin audit log entries older than this many days. Runs periodically. |

**Validation:** The process exits at startup if `master_key` is empty or missing.

---

## 6. Router Settings

Controls request routing, retries, and failure handling.

```yaml
router_settings:
  routing_strategy: "simple-shuffle"   # optional
  num_retries: 2                        # optional
  cooldown_time: 45                     # optional
  allowed_fails: 1                      # optional
  circuit_breaker:                      # optional
    enabled: true
    failure_threshold: 3
    recovery_timeout: 30
```

### Top-level Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `routing_strategy` | string | `"simple-shuffle"` | How to select a key from a pool of available keys for the same model. |
| `num_retries` | int | `2` | Retries per request. Total attempts = `num_retries + 1` (initial + retries). |
| `cooldown_time` | int | `45` | Seconds a failed key is removed from the rotation before retry. |
| `allowed_fails` | int | `1` | Consecutive failures before a key enters cooldown. |

### Circuit Breaker

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `circuit_breaker.enabled` | bool | `true` | Enable the circuit breaker pattern. |
| `circuit_breaker.failure_threshold` | int | `3` | Failures within the window to trip the breaker. |
| `circuit_breaker.recovery_timeout` | int | `30` | Seconds before a tripped breaker enters half-open state. |

**Behavior:** When the circuit breaker trips, all keys for a model are temporarily unavailable. After `recovery_timeout` seconds, one probe request is allowed through. If it succeeds, the breaker resets; if it fails, the timer restarts.

---

## 7. Model List

Each entry in `model_list` defines a model the proxy can serve. At least one entry is required.

```yaml
model_list:
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openai/gpt-5.4-mini"
      api_key: "${OPENAI_API_KEY}"
      api_base: null
    rpm: null
    rpd: null
    tpm: null
    tpd: null
    ash: null
    asd: null
    images: false
    embeddings: false
    stt: false
    tts: false
    failover_model: null
    can_cache: false
    cost_per_1m_input: null
    cost_per_1m_output: null
```

### Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model_name` | string | **required** | User-facing model name. This is what clients send in `model:`. |
| `model_params.model` | string | **required** | Upstream model string with provider prefix (see [Provider Prefixes](#9-provider-prefixes)). |
| `model_params.api_key` | string \| null | `null` | Provider API key. Use `${ENV_VAR}`. |
| `model_params.api_base` | string \| null | `null` | Custom base URL. Required for some providers (OpenRouter, NVIDIA NIM, Cloudflare). |
| `rpm` | int \| null | `null` | Requests per minute limit for this model. `null` = unlimited. |
| `rpd` | int \| null | `null` | Requests per day limit. |
| `tpm` | int \| null | `null` | Tokens per minute limit. |
| `tpd` | int \| null | `null` | Tokens per day limit. |
| `ash` | int \| null | `null` | Audio seconds per hour (for STT/TTS models). |
| `asd` | int \| null | `null` | Audio seconds per day. |
| `images` | bool | `false` | Model accepts image inputs (multimodal). |
| `embeddings` | bool | `false` | Model is an embedding model (not chat). |
| `stt` | bool | `false` | Model is speech-to-text. |
| `tts` | bool | `false` | Model is text-to-speech. |
| `failover_model` | string \| null | `null` | Model name to fall back to if all retries for this model fail. Must exist in `model_list`. |
| `can_cache` | bool | `false` | Enable in-memory response caching for non-streaming requests. |
| `cost_per_1m_input` | float \| null | `null` | Cost per 1M input tokens (for budget tracking). |
| `cost_per_1m_output` | float \| null | `null` | Cost per 1M output tokens. |

---

## 8. Key Pooling

Multiple `model_list` entries with the **same `model_name`** but **different `api_key`** values form a load-balanced pool. The router distributes requests across available keys using the configured `routing_strategy`.

### Example

```yaml
model_list:
  # Key 1
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openai/gpt-5.4-mini"
      api_key: "${OPENAI_KEY_1}"
    rpm: 500

  # Key 2 — same model_name, different key
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openai/gpt-5.4-mini"
      api_key: "${OPENAI_KEY_2}"
    rpm: 500

  # Key 3 — different provider, same user-facing name
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openrouter/openai/gpt-5.4-mini"
      api_key: "${OPENROUTER_KEY}"
      api_base: "https://openrouter.ai/api/v1"
    rpd: 100
```

Clients see a single `gpt-5.4-mini` model. The proxy round-robins across the three keys. If one key hits rate limits or fails, the others absorb traffic.

**Rate limits are per-entry.** Each pooled entry can have its own `rpm`/`rpd`/`tpm`/`tpd`. The router tracks counters independently per key.

---

## 9. Provider Prefixes

The `model_params.model` field uses a provider prefix to route to the correct upstream API. The prefix determines which adapter handles the request.

| Prefix | Provider | Example | `api_base` needed? |
|--------|----------|---------|-------------------|
| `openai/` | OpenAI | `openai/gpt-5.4-mini` | No |
| `anthropic/` | Anthropic | `anthropic/claude-opus-4-5` | No |
| `gemini/` | Google Gemini | `gemini/gemma-4-31b-it` | No |
| `groq/` | Groq | `groq/qwen/qwen3-32b` | No (set explicitly for OpenAI-compat models) |
| `openrouter/` | OpenRouter | `openrouter/nvidia/nemotron-3-ultra-550b-a55b:free` | Yes (`https://openrouter.ai/api/v1`) |
| `nvidia_nim/` | NVIDIA NIM | `nvidia_nim/deepseek-ai/deepseek-v4-pro` | Yes (`https://integrate.api.nvidia.com/v1`) |
| `cloudflare/` | Cloudflare Workers AI | `cloudflare/@cf/zai-org/glm-5.2` | Yes (`https://api.cloudflare.com/client/v4/accounts/ACCOUNT_ID/ai/v1`) |
| `zai/` | Zhipu AI | `zai/glm-4.5-flash` | Yes (`https://open.bigmodel.cn/api/paas/v4`) |

**Notes:**
- `groq/` models using OpenAI-compatible endpoints (e.g., `groq/openai/gpt-oss-120b`) require `api_base: "https://api.groq.com/openai/v1"`.
- `cloudflare/` models use the `@cf/` namespace in the model string. The `api_base` must include your Cloudflare account ID.
- Unknown prefixes fall back to the OpenAI adapter.

---

## 10. YAML Users File

The users file pre-seeds API keys that can authenticate requests. Loaded at startup via `--users` or auto-detected.

```yaml
users:
  - key: "sk-pico-abc123..."
    label: "team-alpha"
    models: null
    rpm: null
    rpd: null
    tpm: null
    tpd: null

  - key: "sk-pico-def456..."
    label: "contractor-bob"
    models: ["gpt-5.4-mini", "gemma-4-31b-it"]
    rpm: 10
    rpd: 100
    tpm: 50000
    tpd: 500000
```

### Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `key` | string | **required** | The API key clients send in `Authorization: Bearer <key>`. |
| `label` | string \| null | `null` | Human-readable label for this key (shown in admin UI). |
| `models` | list \| null | `null` | Model allowlist. `null` = access to all models. A list restricts to only those models. |
| `rpm` | int \| null | `null` | Requests per minute for this key. |
| `rpd` | int \| null | `null` | Requests per day. |
| `tpm` | int \| null | `null` | Tokens per minute. |
| `tpd` | int \| null | `null` | Tokens per day. |

### Seeding Behavior

- Users file is **read-only at startup**. Changes require a restart.
- Keys are stored in the database with SHA-256 hashes. The raw key is only needed for the initial load.
- If the users file doesn't exist, llm-pico starts in **admin API only** mode (no user key authentication).
- You can also create/manage users via the admin API (`POST /admin/keys`) — these are stored in the database and persist across restarts.

---

## 11. Rate Limits

Rate limits are enforced at four levels, evaluated independently per request window. Limits cascade: the **most restrictive** (minimum) value wins.

### Levels

| Level | Source | Where configured |
|-------|--------|-----------------|
| **Model** | `model_list` entry | `rpm`, `rpd`, `tpm`, `tpd`, `ash`, `asd` on the model entry |
| **Key** | Users file or admin API | `rpm`, `rpd`, `tpm`, `tpd` on the key |
| **User** | Admin API | `PUT /admin/users/{id}/limits` |
| **Team** | Admin API | `PUT /admin/teams/{id}/limits` |

### Cascading Rules

For each window type (`rpm`, `rpd`, `tpm`, `tpd`, `ash`, `asd`):

```
effective_limit = min(key_limit, user_limit, team_limit)
```

- If a level has no limit set (`null`), it's ignored in the min calculation.
- If **all** levels are `null`, the window is unlimited.
- Model-level limits are checked **separately** — they apply per-key in the pool, not per-user.

**Example:**

| Level | RPM |
|-------|-----|
| Key | 50 |
| User | 100 |
| Team | 200 |
| **Effective** | **50** (min) |

If the key limit is removed (`null`), effective becomes `min(100, 200) = 100`.

### Response Headers

Rate limit headers are returned on every response:

```
X-RateLimit-Limit-RPM: 50
X-RateLimit-Remaining-RPM: 47
X-RateLimit-Reset-RPM: 1234567890
```

---

## 12. Model Capability Flags

These boolean flags control how the proxy routes and validates requests for a model.

| Flag | Default | Purpose |
|------|---------|---------|
| `images` | `false` | Model accepts `image_url` content parts in messages. Set for multimodal models (GPT-4o, Gemini Pro Vision, Claude 3.5 Sonnet, etc.). |
| `embeddings` | `false` | Model is used for `/embeddings` endpoint, not chat completions. |
| `stt` | `false` | Model is speech-to-text (audio input → text output). |
| `tts` | `false` | Model is text-to-speech (text input → audio output). |

**When to set each:**

- `images: true` — any model that handles image inputs. Without this flag, image content parts may be stripped or rejected.
- `embeddings: true` — dedicated embedding models (e.g., `gemini/gemini-embedding-2`). Routes to the embeddings endpoint.
- `stt: true` — speech recognition models (e.g., `groq/whisper-large-v3`). Used for audio transcription requests.
- `tts: true` — speech synthesis models (e.g., `groq/canopylabs/orpheus-v1-english`). Used for text-to-audio requests.

---

## 13. Fallback / Failover

The `failover_model` field specifies a backup model when all retries for the primary model are exhausted.

```yaml
model_list:
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openai/gpt-5.4-mini"
      api_key: "${OPENAI_API_KEY}"
    failover_model: "gemma-4-31b-it"   # ← fallback

  - model_name: "gemma-4-31b-it"
    model_params:
      model: "gemini/gemma-4-31b-it"
      api_key: "${GEMINI_API_KEY}"
```

**Behavior:**
1. Request comes in for `gpt-5.4-mini`.
2. Router tries up to `num_retries + 1` attempts across pooled keys.
3. If all attempts fail (rate limit, 5xx, timeout), the request is retried against `gemma-4-31b-it`.
4. The failover model must exist in `model_list`.

**Chaining:** Failover models can have their own `failover_model`, but avoid deep chains — they add latency on failure paths.

---

## 14. Caching

When `can_cache: true` is set on a model entry, non-streaming responses are cached in memory.

### Configuration

```yaml
model_list:
  - model_name: "gpt-5.4-mini"
    model_params:
      model: "openai/gpt-5.4-mini"
      api_key: "${OPENAI_API_KEY}"
    can_cache: true
```

### Behavior

| Property | Value |
|----------|-------|
| Storage | In-memory `OrderedDict` |
| Max entries | 256 |
| TTL | 1 hour (3600 seconds) |
| Key | SHA-256 hash of the raw request body |
| Eviction | LRU (least recently used) |
| Scope | Non-streaming requests only (`stream: false`) |

**How it works:**
1. Before sending to upstream, the proxy hashes the request body.
2. If a matching, non-expired entry exists, the cached response is returned immediately — no upstream call.
3. On successful upstream response, the body is stored in the cache.
4. Streaming requests always bypass the cache.

**Cache is per-process.** If you run multiple llm-pico instances, each has its own 256-entry cache. Cache is lost on restart.

---

## 15. Complete Annotated Example

```yaml
# ──────────────────────────────────────────────
# model_settings — passed through to litellm
# ──────────────────────────────────────────────
model_settings:
  drop_params: true       # drop unsupported params instead of erroring
  modify_params: true     # auto-adapt params for provider compatibility

# ──────────────────────────────────────────────
# router_settings — retry, cooldown, circuit breaker
# ──────────────────────────────────────────────
router_settings:
  routing_strategy: simple-shuffle   # round-robin across pooled keys
  num_retries: 2                     # 2 retries = 3 total attempts
  cooldown_time: 45                  # failed key cools down for 45s
  allowed_fails: 1                   # 1 failure triggers cooldown
  circuit_breaker:
    enabled: true
    failure_threshold: 3             # 3 failures trips the breaker
    recovery_timeout: 30             # 30s before half-open probe

# ──────────────────────────────────────────────
# general_settings — admin key, database, retention
# ──────────────────────────────────────────────
general_settings:
  master_key: "${LLM_PICO_MASTER_KEY}"   # REQUIRED — admin auth
  db_path: null                            # null → llm-pico.db in config dir
  usage_log_retention_days: 30             # prune usage logs after 30 days
  admin_log_retention_days: 90             # prune audit logs after 90 days

# ──────────────────────────────────────────────
# model_list — all available models
# ──────────────────────────────────────────────
model_list:

  # ── OpenAI ──────────────────────────────────
  - model_name: gpt-5.4-mini
    model_params:
      model: openai/gpt-5.4-mini
      api_key: "${OPENAI_API_KEY}"
    images: false
    can_cache: true                        # cache non-streaming responses
    cost_per_1m_input: 15.00               # $15 / 1M input tokens
    cost_per_1m_output: 60.00              # $60 / 1M output tokens
    failover_model: "gemma-4-31b-it"       # fallback if OpenAI fails

  # ── Google Gemini ───────────────────────────
  - model_name: gemma-4-31b-it
    model_params:
      model: gemini/gemma-4-31b-it
      api_key: "${GEMINI_API_KEY}"
    rpm: 15
    rpd: 1500
    images: false

  - model_name: gemini-3-flash-preview
    model_params:
      model: gemini/gemini-3-flash-preview
      api_key: "${GEMINI_API_KEY}"
    rpm: 5
    rpd: 20
    tpm: 250000
    images: false

  # ── Gemini Embeddings ───────────────────────
  - model_name: gemini-embedding-2
    model_params:
      model: gemini/gemini-embedding-2
      api_key: "${GEMINI_API_KEY}"
    rpm: 100
    rpd: 1000
    tpm: 30000
    images: false
    embeddings: true                       # embedding model, not chat

  # ── Groq (STT) ─────────────────────────────
  - model_name: groq-whisper-large-v3-stt
    model_params:
      model: groq/whisper-large-v3
      api_key: "${GROQ_API_KEY}"
      api_base: "https://api.groq.com/openai/v1"
    ash: 7200                              # 2 hours audio per hour
    asd: 2880                              # 48 minutes audio per day
    rpm: 20
    rpd: 2000
    stt: true                              # speech-to-text model

  # ── Groq (TTS) ─────────────────────────────
  - model_name: groq-orpheus-english-tts
    model_params:
      model: groq/canopylabs/orpheus-v1-english
      api_key: "${GROQ_API_KEY}"
      api_base: "https://api.groq.com/openai/v1"
    rpd: 100
    rpm: 10
    tpm: 1200
    tpd: 3600
    tts: true                              # text-to-speech model

  # ── OpenRouter (with key pooling) ───────────
  - model_name: nvidia-nemotron-ultra
    model_params:
      model: openrouter/nvidia/nemotron-3-ultra-550b-a55b:free
      api_key: "${OPENROUTER_KEY_1}"
      api_base: "https://openrouter.ai/api/v1"
    rpd: 50

  - model_name: nvidia-nemotron-ultra       # same name → pooled
    model_params:
      model: openrouter/nvidia/nemotron-3-ultra-550b-a55b:free
      api_key: "${OPENROUTER_KEY_2}"
      api_base: "https://openrouter.ai/api/v1"
    rpd: 20

  # ── NVIDIA NIM ─────────────────────────────
  - model_name: nvidia-deepseek-v4-pro
    model_params:
      model: nvidia_nim/deepseek-ai/deepseek-v4-pro
      api_key: "${NVIDIA_API_KEY}"
      api_base: "https://integrate.api.nvidia.com/v1"

  # ── Cloudflare Workers AI ──────────────────
  - model_name: cf-glm-5.2
    model_params:
      model: cloudflare/@cf/zai-org/glm-5.2
      api_key: "${CF_API_TOKEN}"
      api_base: "https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/ai/v1"

  # ── Zhipu AI ───────────────────────────────
  - model_name: zai-glm-4.5-flash
    model_params:
      model: zai/glm-4.5-flash
      api_key: "${ZAI_API_KEY}"
      api_base: "https://open.bigmodel.cn/api/paas/v4"

# ──────────────────────────────────────────────
# users (loaded separately via --users flag)
# ──────────────────────────────────────────────
# users.yaml:
#
# users:
#   - key: "${USER_KEY_ALPHA}"
#     label: "team-alpha"
#     models: null                          # all models
#     rpm: 50
#     rpd: 1000
#
#   - key: "${USER_KEY_BETA}"
#     label: "contractor"
#     models: ["gpt-5.4-mini"]             # restricted to one model
#     rpm: 10
#     rpd: 100
#     tpm: 50000
#     tpd: 500000
```

---

## 16. Security Best Practices

### Secrets

- **Never hardcode API keys or master key in `config.yaml`.** Use `${ENV_VAR}` syntax:
  ```yaml
  master_key: "${LLM_PICO_MASTER_KEY}"
  api_key: "${OPENAI_API_KEY}"
  ```
- Set environment variables in your process manager (systemd, Docker, etc.), not in shell profiles.
- In Docker: use `--env-file` or secrets, not `ENV` in Dockerfile for production keys.

### Master Key

- Use a long, random string (32+ characters). Example generation:
  ```bash
  openssl rand -hex 32
  ```
- Never reuse the master key as an API key or for other services.
- Rotate periodically. Update `config.yaml` and restart.

### Database

- Set restrictive permissions on the SQLite file:
  ```bash
  chmod 600 /var/lib/llm-pico/llm-pico.db
  chown llm-pico:llm-pico /var/lib/llm-pico/llm-pico.db
  ```
- Store the database outside the config directory if the config is world-readable.
- Back up the database regularly — it contains user keys (hashed), usage logs, and admin audit trails.

### Network

- Bind to `127.0.0.1` if running behind a reverse proxy:
  ```bash
  llm-pico --host 127.0.0.1 --port 4000
  ```
- Use TLS termination at the reverse proxy (nginx, Caddy, etc.). llm-pico serves plain HTTP.

### Users File

- Set `chmod 600` on `users.yaml` since it contains raw API keys.
- Consider using the admin API to manage keys instead of the file — database storage hashes keys at rest.

### Filesystem

```
/etc/llm-pico/
├── config.yaml          # chmod 640 (readable by service user)
├── users.yaml           # chmod 600 (contains raw keys)
/var/lib/llm-pico/
└── llm-pico.db          # chmod 600 (contains hashed keys + audit logs)
```
