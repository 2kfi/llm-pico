# Authentication

llm-pico uses Bearer token authentication. Two types of keys exist:

## Key Types

### Master Key

Set in `general_settings.master_key` in `config.yaml`. Used for:

- Admin API access
- Bypasses all rate limits
- Bypasses model allowlists
- Full access to all endpoints

```yaml
general_settings:
  master_key: "sk-pico-master-abc123..."
```

### User Keys

Defined in `users.yaml` or created via admin API. Used for:

- Normal API access
- Subject to rate limits
- Subject to model allowlists
- Scoped to specific models (if configured)

Format: `sk-pico-` + 32 random hex characters

## Authentication Flow

```
Client sends: Authorization: Bearer sk-pico-abc123...
                        ↓
              Extract token from header
                        ↓
              Is it the master key?
              (hmac.compare_digest)
                   /        \
                 Yes          No
                  ↓            ↓
           Return admin    SHA-256 hash
           role            the token
                              ↓
                    Lookup hash in user_keys table
                              ↓
                    Check is_active + expires_at
                              ↓
                    If has user_id → resolve user + team
                              ↓
                    Merge limits (most restrictive)
                    Merge allowlists (intersection)
                              ↓
                    Return key metadata
```

## Master Key Auth

The master key is compared using `hmac.compare_digest` for constant-time comparison. This prevents timing side-channel attacks.

```python
if hmac.compare_digest(provided_key, master_key):
    return {"role": "admin"}
```

## User Key Auth

User keys are stored as SHA-256 hashes in the database:

```python
key_hash = hashlib.sha256(raw_key.encode()).hexdigest()
```

The raw key is never stored. Only the hash is used for lookup.

## Key Expiration

Keys can have an optional `expires_at` timestamp:

```json
{
  "expires_at": "2024-12-31T23:59:59"
}
```

If `expires_at` is in the past, the key is rejected with 401.

## Model Allowlists

### Per-Key

```yaml
users:
  - key: "sk-pico-abc..."
    models:
      - gpt-4
      - claude-3
```

### Per-User

Via admin API:

```bash
PUT /admin/users/{user_id}/models
{"models": ["gpt-4"]}
```

### Per-Team

Via admin API:

```bash
PUT /admin/teams/{team_id}/models
{"models": ["gpt-4", "claude-3", "gemini"]}
```

### Merging

When a key is assigned to a user who belongs to a team:

- **Allowlists:** `intersection()` of all non-null lists
- `null` = unrestricted (doesn't constrain)

Example:

```
Key allows:   [gpt-4, claude-3]
User allows:  [gpt-4, gemini]
Team allows:  null (unrestricted)
Effective:    [gpt-4] (intersection)
```

## Rate Limit Merging

Limits are merged across key, user, and team:

- **Limits:** `min()` of all non-null values (most restrictive wins)

Example:

```
Key limits:   rpm=100
User limits:  rpm=50
Team limits:  rpm=200
Effective:    rpm=50 (most restrictive)
```

## Error Responses

### Missing/Invalid Key

```json
{
  "error": {
    "message": "Invalid API key",
    "type": "invalid_request_error",
    "code": 401
  }
}
```

### Model Not Allowed

```json
{
  "error": {
    "message": "Model 'gpt-4' is not allowed for this key",
    "type": "invalid_request_error",
    "code": 403
  }
}
```

### Key Expired

```json
{
  "error": {
    "message": "API key has expired",
    "type": "invalid_request_error",
    "code": 401
  }
}
```

## Security Notes

- Master key is compared with constant-time comparison (`hmac.compare_digest`)
- User keys are stored as SHA-256 hashes (never plaintext)
- CORS allows all origins (configure reverse proxy for production)
- Admin API requires master key (not user keys)
- Config reload drains in-flight requests before restart
