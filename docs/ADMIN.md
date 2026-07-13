# Admin API

All admin endpoints require the master key in the `Authorization: Bearer <master_key>` header.

## Authentication

```bash
curl -H "Authorization: Bearer sk-pico-master-..." http://localhost:4000/admin/keys
```

The master key is set in `general_settings.master_key` in `config.yaml`.

## Key Management

### List Keys

```
GET /admin/keys
```

Returns all user keys (metadata only, no raw key values).

**Response:**

```json
[
  {
    "key_prefix": "sk-pico-abc...",
    "label": "dev-bot",
    "is_active": true,
    "created_at": "2024-01-01T00:00:00",
    "expires_at": null,
    "model_allowlist": null,
    "rpm_limit": 100,
    "rpd_limit": 10000,
    "tpm_limit": null,
    "tpd_limit": null,
    "user_id": 1
  }
]
```

### Create Key

```
POST /admin/keys
```

**Request:**

```json
{
  "label": "new-key",
  "models": ["gpt-4", "claude-3"],
  "rpm": 100,
  "rpd": 10000,
  "user_id": 1
}
```

**Response:**

```json
{
  "key": "sk-pico-abc123...",
  "key_prefix": "sk-pico-abc...",
  "label": "new-key"
}
```

**Important:** The full key is only shown once. Store it securely.

### Deactivate Key

```
DELETE /admin/keys/{prefix}
```

Soft-deactivates a key. The key can be reactivated by updating `is_active` in the database.

### Set Model Allowlist

```
PUT /admin/keys/{prefix}/models
```

**Request:**

```json
{
  "models": ["gpt-4", "claude-3"]
}
```

Set to `null` or `[]` to allow all models.

### Set Rate Limits

```
PUT /admin/keys/{prefix}/limits
```

**Request:**

```json
{
  "rpm": 100,
  "rpd": 10000,
  "tpm": 150000,
  "tpd": 10000000
}
```

### Assign Key to User

```
PUT /admin/keys/{prefix}/user
```

**Request:**

```json
{
  "user_id": 1
}
```

## Team Management

### List Teams

```
GET /admin/teams
```

### Create Team

```
POST /admin/teams
```

**Request:**

```json
{
  "name": "engineering",
  "description": "Engineering team",
  "model_allowlist": ["gpt-4", "claude-3"],
  "rpm_limit": 1000,
  "rpd_limit": 100000
}
```

### Get Team

```
GET /admin/teams/{team_id}
```

Returns team details + current month spend.

### Update Team Limits

```
PUT /admin/teams/{team_id}/limits
```

### Update Team Models

```
PUT /admin/teams/{team_id}/models
```

### Deactivate Team

```
DELETE /admin/teams/{team_id}
```

Cascades deactivation to all users and keys in the team.

### Team Usage

```
GET /admin/teams/{team_id}/usage
```

Returns usage stats scoped to the team.

### Team Users

```
POST /admin/teams/{team_id}/users
GET /admin/teams/{team_id}/users
```

## User Management

### Get User

```
GET /admin/users/{user_id}
```

Returns user details + current month spend.

### Update User Limits

```
PUT /admin/users/{user_id}/limits
```

### Update User Budget

```
PUT /admin/users/{user_id}/budget
```

**Request:**

```json
{
  "monthly_budget_usd": 100.00
}
```

Set to `null` for unlimited.

### Update User Models

```
PUT /admin/users/{user_id}/models
```

### User Usage

```
GET /admin/users/{user_id}/usage
```

## Usage & Stats

### Global Usage

```
GET /admin/usage
```

**Query parameters:**

| Param | Description |
|-------|-------------|
| `key_hash` | Filter by key hash |
| `from` | Start date (ISO format) |
| `to` | End date (ISO format) |
| `limit` | Max results (default 100) |

### Top Models

```
GET /admin/usage/top-models
```

Returns models ranked by token usage with cost.

### Cost Stats

```
GET /admin/stats/costs
```

Returns costs grouped by user/model/day.

### Budgets Summary

```
GET /admin/budgets
```

Returns all users across all teams with budget vs current spend.

## Admin Log

### View Log

```
GET /admin/log
```

Returns admin audit log (action, actor, details, timestamp).

### Live Log Stream

```
GET /admin/logs/stream
```

SSE stream of live proxy events. Requires master key via query param or Bearer header.

```bash
curl -N "http://localhost:4000/admin/logs/stream?token=sk-pico-master-..."
```

### Log Dashboard

```
GET /admin/logs
```

HTML dashboard for the live log stream.

## Config Reload

```
POST /admin/config/reload
```

Gracefully drains in-flight requests, then restarts the process with `os.execve`.

**Behavior:**

1. Sets `_is_draining` flag
2. New requests return 503 with `Retry-After: 30`
3. Waits for in-flight requests to complete (up to 120s)
4. Restarts process with same arguments

## Example: Full Admin Workflow

```bash
MASTER_KEY="sk-pico-master-abc123"

# Create a team
curl -X POST http://localhost:4000/admin/teams \
  -H "Authorization: Bearer $MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "engineering"}'

# Create a user under the team
curl -X POST http://localhost:4000/admin/teams/1/users \
  -H "Authorization: Bearer $MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email": "dev@example.com", "name": "Developer"}'

# Create a key for the user
curl -X POST http://localhost:4000/admin/keys \
  -H "Authorization: Bearer $MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"label": "dev-key", "user_id": 1}'

# Check usage
curl http://localhost:4000/admin/usage \
  -H "Authorization: Bearer $MASTER_KEY"
```
