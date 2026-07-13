# Users

`users.yaml` defines user API keys, rate limits, and model permissions.

## Format

```yaml
users:
  - key: "sk-pico-abc123..."
    label: "dev-bot"
    models:
      - gpt-4
      - claude-3
    rpm: 100
    rpd: 10000
    tpm: 150000
    tpd: 10000000

  - key: "sk-pico-def456..."
    label: "production"
    models: null
    rpm: 500
```

## Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `key` | string | **required** | API key (full string, not hashed). Typically `sk-pico-*`. |
| `label` | string \| null | `null` | Human-readable label for display in admin UI. |
| `models` | list \| null | `null` | Model allowlist. `null` = all models allowed. |
| `rpm` | int \| null | `null` | Requests per minute limit. `null` = unlimited. |
| `rpd` | int \| null | `null` | Requests per day limit. |
| `tpm` | int \| null | `null` | Tokens per minute limit. |
| `tpd` | int \| null | `null` | Tokens per day limit. |

## How It Works

### Startup Loading

On proxy startup, all entries in `users.yaml` are inserted into the SQLite database using `INSERT OR IGNORE`. This means:

- Existing keys are not overwritten
- New keys are added
- Deleted keys remain in the database (use admin API to deactivate)

### Authentication Flow

1. Client sends `Authorization: Bearer sk-pico-...`
2. Proxy hashes the key with SHA-256
3. Looks up hash in `user_keys` table
4. Checks `is_active` and `expires_at`
5. If key has a `user_id`, resolves user and team limits
6. Returns key metadata (allowlist, rate limits, etc.)

### Rate Limit Merging

If a key is assigned to a user who belongs to a team:

- **Limits:** `min()` across key, user, and team (most restrictive wins)
- **Allowlists:** `intersection()` of all non-null lists (most restrictive wins)
- `null` = unrestricted (doesn't constrain)

Example:

```yaml
# Key limits: rpm=100
# User limits: rpm=50
# Team limits: rpm=200
# Effective: rpm=50 (most restrictive)
```

### Model Allowlist Merging

```yaml
# Key allows: [gpt-4, claude-3]
# User allows: [gpt-4, gemini]
# Team allows: null (unrestricted)
# Effective: [gpt-4] (intersection of key and user)
```

## Generating Keys

Keys are generated automatically by:

1. `llm-pico init` — creates first user with a random key
2. Admin API — `POST /admin/keys` generates `sk-pico-*` keys

Format: `sk-pico-` + 32 random hex characters

## Admin Management

Users can be managed via the admin API:

| Action | Endpoint |
|--------|----------|
| List keys | `GET /admin/keys` |
| Create key | `POST /admin/keys` |
| Deactivate key | `DELETE /admin/keys/{prefix}` |
| Set model allowlist | `PUT /admin/keys/{prefix}/models` |
| Set rate limits | `PUT /admin/keys/{prefix}/limits` |
| Assign to user | `PUT /admin/keys/{prefix}/user` |

See [Admin API](ADMIN.md) for details.

## Example: Minimal users.yaml

```yaml
users:
  - key: "sk-pico-abc123..."
```

This creates an unrestricted key with no rate limits.

## Example: Production users.yaml

```yaml
users:
  - key: "sk-pico-dev-abc..."
    label: "development"
    models:
      - gpt-4
      - claude-3
    rpm: 100
    rpd: 10000

  - key: "sk-pico-prod-def..."
    label: "production"
    models: null
    rpm: 1000
    rpd: 100000
    tpm: 500000
    tpd: 50000000
```
