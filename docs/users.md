# Users, Teams, Keys & Budgets — Complete Guide

Everything you need to manage who can use llm-pico, what they can access, how fast they can go, and how much they can spend.

---

## 1. Introduction

llm-pico has three layers of access control:

- **Teams** — logical groups of users (e.g., "engineering", "data-science")
- **Users** — individual people within a team, each with their own budget and limits
- **API Keys** — the actual tokens clients use to authenticate requests, assigned to users

Every request goes through an API key. The key's limits, its assigned user's limits, and the user's team limits all combine to determine what's actually enforced.

Budgets are per-user. When a user hits their monthly spend limit, their requests are hard-blocked — no exceptions.

---

## 2. Concepts

### Hierarchy

```
Team
 └── User (has monthly_budget_usd, rpm/rpd/tpm/tpd limits, model allowlist)
      └── API Key (has rpm/rpd/tpm/tpd limits, model allowlist, optional expiry)
```

A key **must** be assigned to a user to inherit user/team limits. Unassigned keys use only their own limits.

### Rate Limit Merging (Minimum Wins)

When a request comes in, the rate limiter checks three levels:
1. Key-level limits (set on the key)
2. User-level limits (set on the user)
3. Team-level limits (set on the team)

For each window type (RPM, RPD, TPM, TPD, ASH, ASD), the **minimum** non-null value wins.

Example:
- Key: RPM=100
- User: RPM=50
- Team: RPM=200

Effective RPM = **50** (the minimum).

If a level has no limit set (null), it's ignored. Only non-null values are compared.

### Model Access Intersection (AND Logic)

Model allowlists work the opposite way — the **intersection** wins.

Example:
- Key allowlist: `[gpt-5.4-mini, claude-sonnet-4]`
- User allowlist: `[gpt-5.4-mini, gemini-3-flash-preview]`
- Team allowlist: null (unrestricted)

Effective allowlist = `[gpt-5.4-mini]` (only model in both key and user lists).

If all three are null, the key can access any model. If any level sets a list, only models in **all** lists are accessible.

---

## 3. Quick Start

Here's the fastest way to get a working setup: create a team, add a user, create an API key, set a budget.

### Step 1: Create a team

```bash
curl -s -X POST http://localhost:4000/admin/teams \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "engineering", "description": "Core engineering team"}'
```

Response:
```json
{"id": 1, "name": "engineering", "description": "Core engineering team", "is_active": true, "created_at": "2026-07-11T12:00:00"}
```

Save the team id (1 in this example).

### Step 2: Create a user

```bash
curl -s -X POST http://localhost:4000/admin/teams/1/users \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email": "alice@example.com", "name": "Alice"}'
```

Response:
```json
{"id": 1, "team_id": 1, "email": "alice@example.com", "name": "Alice", "is_active": true, "created_at": "2026-07-11T12:00:00"}
```

Save the user id (1).

### Step 3: Set a monthly budget

```bash
curl -s -X PUT http://localhost:4000/admin/users/1/budget \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"monthly_budget_usd": 50.00}'
```

Response: `{"updated": true, "monthly_budget_usd": 50.0}`

### Step 4: Create an API key

```bash
curl -s -X POST http://localhost:4000/admin/keys \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"label": "alice-workstation", "user_id": 1}'
```

Response:
```json
{"key": "sk-pico-a1b2c3d4e5f6...", "key_prefix": "sk-pico-a1b2...", "label": "alice-workstation"}
```

**Save the key immediately.** It is only shown once — you cannot retrieve it later.

### Step 5: Use the key

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-pico-a1b2c3d4e5f6..." \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-5.4-mini", "messages": [{"role": "user", "content": "hello"}]}'
```

Done.

---

## 4. API Key Management

### Key Format

Keys look like `sk-pico-<64 hex characters>`. Example:
```
sk-pico-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2
```

The prefix (first 12 characters + `...`) is stored in the database and returned in API responses. The full key is hashed with SHA-256 and the original is discarded.

### Create a Key

```bash
curl -s -X POST http://localhost:4000/admin/keys \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "label": "my-app",
    "user_id": 1,
    "models": ["gpt-5.4-mini", "claude-sonnet-4"],
    "rpm_limit": 50,
    "rpd_limit": 5000,
    "tpm_limit": 100000,
    "tpd_limit": 5000000,
    "expires_at": "2026-12-31T23:59:59"
  }'
```

All fields are optional except the request must be valid JSON.

| Field | Type | Description |
|-------|------|-------------|
| `label` | string | Human-readable label |
| `user_id` | int | Assign to a user (links to user/team limits) |
| `models` | list or null | Model allowlist. null = all models |
| `rpm_limit` | int | Requests per minute |
| `rpd_limit` | int | Requests per day |
| `tpm_limit` | int | Tokens per minute |
| `tpd_limit` | int | Tokens per day |
| `expires_at` | string | ISO 8601 timestamp. Key rejected after this time |

The response includes the raw key exactly once:
```json
{"key": "sk-pico-...", "key_prefix": "sk-pico-a1b2...", "label": "my-app"}
```

### List All Keys

```bash
curl -s http://localhost:4000/admin/keys \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response:
```json
{
  "keys": [
    {
      "key_prefix": "sk-pico-a1b2...",
      "label": "my-app",
      "is_active": true,
      "created_at": "2026-07-11T12:00:00",
      "expires_at": null,
      "model_allowlist": ["gpt-5.4-mini"],
      "rpm_limit": 50,
      "rpd_limit": 5000,
      "tpm_limit": 100000,
      "tpd_limit": 5000000,
      "user_id": 1
    }
  ],
  "total": 1
}
```

### Revoke (Soft-Delete) a Key

Uses the key prefix (first 12 chars + `...`):

```bash
curl -s -X DELETE http://localhost:4000/admin/keys/sk-pico-a1b2... \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response: `{"revoked": 1}`

This sets `is_active=0`. The key immediately stops working. The hash stays in the database for audit purposes.

If the prefix doesn't match any active key, you get a 404:
```json
{"error": {"message": "No active key found with that prefix", "type": "not_found", "code": 404}}
```

### Update Model Allowlist

```bash
curl -s -X PUT http://localhost:4000/admin/keys/sk-pico-a1b2.../models \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"models": ["gpt-5.4-mini"]}'
```

To allow all models (remove restriction):
```bash
curl -s -X PUT http://localhost:4000/admin/keys/sk-pico-a1b2.../models \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"models": null}'
```

### Update Rate Limits

Only the fields you include are changed. Null or omitted fields stay as-is.

```bash
curl -s -X PUT http://localhost:4000/admin/keys/sk-pico-a1b2.../limits \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"rpm": 25, "tpm": 50000}'
```

### Assign Key to User

```bash
curl -s -X PUT http://localhost:4000/admin/keys/sk-pico-a1b2.../user \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"user_id": 2}'
```

Set `user_id` to null to unassign:
```bash
-d '{"user_id": null}'
```

---

## 5. Teams

### Create a Team

```bash
curl -s -X POST http://localhost:4000/admin/teams \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "data-science", "description": "ML research team"}'
```

Response:
```json
{"id": 2, "name": "data-science", "description": "ML research team", "is_active": true, "created_at": "2026-07-11T12:00:00"}
```

### List Teams

```bash
curl -s http://localhost:4000/admin/teams \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response:
```json
{
  "teams": [
    {"id": 1, "name": "engineering", "description": "...", "is_active": true, "created_at": "...",
     "rpm_limit": null, "rpd_limit": null, "tpm_limit": null, "tpd_limit": null, "model_allowlist": null}
  ],
  "total": 1
}
```

### Get Team Details (with Spend)

```bash
curl -s http://localhost:4000/admin/teams/1 \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response includes `month_spend_usd` (sum of all usage costs for the team this month):
```json
{"id": 1, "name": "engineering", "month_spend_usd": 12.34, ...}
```

### Set Team Rate Limits

```bash
curl -s -X PUT http://localhost:4000/admin/teams/1/limits \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"rpm": 500, "rpd": 50000, "tpm": 2000000, "tpd": 100000000}'
```

### Set Team Model Allowlist

```bash
curl -s -X PUT http://localhost:4000/admin/teams/1/models \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"models": ["gpt-5.4-mini", "claude-sonnet-4"]}'
```

Remove restriction:
```bash
-d '{"models": null}'
```

### Deactivate a Team

This cascades — it deactivates the team, all its users, and all their keys:

```bash
curl -s -X DELETE http://localhost:4000/admin/teams/1 \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response: `{"deactivated": true}`

All users and keys under this team immediately stop working.

### Team Usage Stats

```bash
curl -s "http://localhost:4000/admin/teams/1/usage?from=2026-07-01&to=2026-07-11&limit=100" \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Query parameters:
- `from` — start date (YYYY-MM-DD)
- `to` — end date (YYYY-MM-DD)
- `limit` — max rows returned (default: 100)

---

## 6. Users

Users belong to a team. Each user can have their own rate limits, model allowlist, and monthly budget.

### Create a User

```bash
curl -s -X POST http://localhost:4000/admin/teams/1/users \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email": "bob@example.com", "name": "Bob"}'
```

Both `email` and `name` are required.

Response:
```json
{"id": 2, "team_id": 1, "email": "bob@example.com", "name": "Bob", "is_active": true, "created_at": "2026-07-11T12:00:00"}
```

### List Users in a Team

```bash
curl -s http://localhost:4000/admin/teams/1/users \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

### Get User Details (with Spend)

```bash
curl -s http://localhost:4000/admin/users/1 \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response includes `month_spend_usd`:
```json
{"id": 1, "team_id": 1, "email": "alice@example.com", "name": "Alice",
 "monthly_budget_usd": 50.0, "month_spend_usd": 12.34, ...}
```

### Set Monthly Budget

```bash
curl -s -X PUT http://localhost:4000/admin/users/1/budget \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"monthly_budget_usd": 100.00}'
```

Set to null to remove budget (no spending cap):
```bash
-d '{"monthly_budget_usd": null}'
```

### Set User Rate Limits

```bash
curl -s -X PUT http://localhost:4000/admin/users/1/limits \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"rpm": 30, "rpd": 3000}'
```

### Set User Model Allowlist

```bash
curl -s -X PUT http://localhost:4000/admin/users/1/models \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"models": ["gpt-5.4-mini"]}'
```

### User Usage Stats

```bash
curl -s "http://localhost:4000/admin/users/1/usage?from=2026-07-01&to=2026-07-11" \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

---

## 7. Budgets

### How Budgets Work

- Budgets are **per-user** (monthly, calendar month UTC).
- The proxy sums all `cost_usd` values from `usage_log` for the current month.
- Before each request, the proxy checks if `current_spend + estimated_cost > monthly_budget_usd`.
- If yes, the request is **hard-blocked** with an error. No graceful degradation, no partial access.

### Set a Budget

See [Set Monthly Budget](#set-monthly-budget) above.

### Budget Overview (All Users)

```bash
curl -s http://localhost:4000/admin/budgets \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

Response:
```json
{
  "users": [
    {
      "id": 1, "name": "Alice", "email": "alice@example.com",
      "team_id": 1, "team_name": "engineering",
      "monthly_budget_usd": 50.0,
      "current_spend": 12.34
    },
    {
      "id": 2, "name": "Bob", "email": "bob@example.com",
      "team_id": 1, "team_name": "engineering",
      "monthly_budget_usd": null,
      "current_spend": 0.0
    }
  ]
}
```

Users with `monthly_budget_usd: null` have no spending cap.

### Hard Block Behavior

When a budget is exceeded, the proxy returns:
```json
{"error": {"message": "Monthly budget exceeded: $49.50 + $0.80 > $50.00", "type": "budget_exceeded", "code": 402}}
```

This is checked per-request. The proxy uses an estimated cost (prompt tokens × rate + completion tokens × rate) before the request is sent. After the response, the actual cost is reconciled.

---

## 8. Rate Limits

### Window Types

| Window | Period | Resets |
|--------|--------|--------|
| RPM | Per minute | Every minute boundary (UTC) |
| RPD | Per day | Every midnight UTC |
| TPM | Per minute | Every minute boundary (UTC) |
| TPD | Per day | Every midnight UTC |
| ASH | Per hour | Every hour boundary (UTC) |
| ASD | Per day | Every midnight UTC |

RPM/RPD = request counts. TPM/TPD = token counts. ASH/ASD = audio seconds (for STT/TTS models).

### Where Limits Can Be Set

| Level | Where | Fields |
|-------|-------|--------|
| Key | `POST /admin/keys` or `PUT /admin/keys/{prefix}/limits` | `rpm_limit`, `rpd_limit`, `tpm_limit`, `tpd_limit` |
| User | `PUT /admin/users/{user_id}/limits` | `rpm_limit`, `rpd_limit`, `tpm_limit`, `tpd_limit` |
| Team | `PUT /admin/teams/{team_id}/limits` | `rpm_limit`, `rpd_limit`, `tpm_limit`, `tpd_limit` |
| Model | `config.yaml` model_list entries | `rpm`, `rpd`, `tpm`, `tpd`, `ash`, `asd` |

### Cascade Logic

Limits cascade from key → user → team. For each window, the **minimum non-null value** wins.

```
effective_rpm = min(key.rpm, user.rpm, team.rpm)   # ignoring nulls
```

Model-level limits (in config.yaml) are checked **separately** — they apply per-key-model pair, independent of user/team limits. Both must pass.

### X-RateLimit-* Headers

Every response includes remaining quota headers:
```
X-RateLimit-Remaining-RPM: 48
X-RateLimit-Remaining-TPM: 99800
X-RateLimit-Remaining-RPD: 4980
X-RateLimit-Remaining-TPD: 4980000
```

Check these headers to know how much quota is left.

### Token Reservation

For streaming requests, the proxy reserves `prompt_tokens + max_tokens` upfront. After the response completes, it reconciles to the actual token count. This means:
- You might temporarily see higher usage than actual
- The reconciliation happens within the same minute, so it doesn't affect daily windows

---

## 9. Model Access

### Allowlist Levels

Model allowlists can be set at three levels:
- **Key**: `PUT /admin/keys/{prefix}/models`
- **User**: `PUT /admin/users/{user_id}/models`
- **Team**: `PUT /admin/teams/{team_id}/models`

### Intersection Logic

Effective models = intersection of all non-null allowlists.

```
key:  [gpt-5.4-mini, claude-sonnet-4]     # set
user: [gpt-5.4-mini, gemini-3-flash]       # set
team: null                                  # unrestricted

effective = [gpt-5.4-mini]                 # only common model
```

If all are null, the key can access any model in the system. If any level sets a list, only models in **all** lists are accessible.

### Null Means Unrestricted

Setting an allowlist to `null` (or omitting it) means "all models allowed." This is different from setting it to an empty list `[]`, which means "no models allowed."

### Model Names

Use the `model_name` from your config.yaml. For example:
```yaml
model_list:
  - model_name: gpt-5.4-mini          # use this name in allowlists
    litellm_params:
      model: openai/gpt-5.4-mini      # this is the upstream provider path
```

---

## 10. YAML Seeding

You can pre-create API keys at startup by passing a `users.yaml` file:

```bash
llm-pico --config config.yaml --users users.yaml
```

### YAML Format

```yaml
users:
  - key: "sk-pico-dev-abc123def456"
    label: "development-bot"
    models: null
    rpm: 100
    rpd: 10000

  - key: "sk-pico-ci-789ghi"
    label: "ci-pipeline"
    models:
      - gemini-3-flash-preview
      - groq-openai-gpt-oss-120b
    tpm: 500000
    tpd: 5000000

  - key: "sk-pico-test-xyz"
    label: "test-user"
```

### Fields

| Field | Required | Description |
|-------|----------|-------------|
| `key` | Yes | The full `sk-pico-*` key string |
| `label` | No | Human-readable label |
| `models` | No | List of allowed model names, or null for all |
| `rpm` | No | Requests per minute limit |
| `rpd` | No | Requests per day limit |
| `tpm` | No | Tokens per minute limit |
| `tpd` | No | Tokens per day limit |

### Behavior

- Uses `INSERT OR IGNORE` — if a key hash already exists in the database, it's skipped
- These keys are **not** assigned to any user (no `user_id`). They only have their own limits, no user/team inheritance
- They can be managed via the admin API after startup like any other key
- You can generate the key values yourself — they don't have to be random. Just prefix with `sk-pico-`

### Auto-Detection

If you don't pass `--users`, the proxy looks for `users.yaml` or `users.yml` in the same directory as your config file.

---

## 11. Key Pooling

Key pooling lets you load-balance across multiple upstream API keys for the same provider.

### How It Works

In `config.yaml`, create multiple `model_list` entries with the same `model_name` but different API keys:

```yaml
model_list:
  - model_name: gpt-5.4-mini
    litellm_params:
      model: openai/gpt-5.4-mini
      api_key: "sk-openai-key-1"
    rpm: 50
    rpd: 5000

  - model_name: gpt-5.4-mini
    litellm_params:
      model: openai/gpt-5.4-mini
      api_key: "sk-openai-key-2"
    rpm: 50
    rpd: 5000
```

Both entries share the same `model_name` and provider slug (`openai`). The router groups them into a single `ProviderGroup` and load-balances across them.

### Circuit Breaker Behavior

- If one key gets a 429, it's cooled down and the next key is tried
- If a provider returns multiple 5xx errors, the entire group's circuit breaker opens
- After `recovery_timeout` seconds, the group is tried again (half-open state)

### When to Use

- You have multiple API keys for the same provider (e.g., different accounts)
- You want higher total throughput than a single key allows
- You want automatic failover when one key hits rate limits

---

## 12. Authentication

### Header Format

Every request must include:
```
Authorization: Bearer <api-key>
```

### Key Types

| Type | Format | Scope |
|------|--------|-------|
| Master key | Set in `config.yaml` `general_settings.master_key` | Full admin access to all endpoints |
| User key | `sk-pico-<64 hex>` | Proxy endpoints only (`/v1/chat/completions`, etc.) |

### Master Key vs User Key

- **Master key** can access all `/admin/*` endpoints AND proxy endpoints
- **User key** can only access proxy endpoints. It gets a 401 on any `/admin/*` endpoint
- Master key is verified with `hmac.compare_digest()` (constant-time, no timing attacks)
- User key is looked up by SHA-256 hash in the database

### Proxy Endpoints (require user key or master key)

| Method | Path |
|--------|------|
| POST | `/v1/chat/completions` |
| POST | `/v1/completions` |
| POST | `/v1/embeddings` |
| GET | `/v1/models` |
| GET | `/v1/models/{id}` |
| GET | `/health` |

### Admin Endpoints (require master key)

All `/admin/*` routes — keys, teams, users, budgets, usage, logs, config reload.

### What Happens with Invalid Keys

```bash
curl -s http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer invalid-key" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-5.4-mini", "messages": [{"role": "user", "content": "hi"}]}'
```

Response:
```json
{"error": {"message": "Invalid API key", "type": "unauthorized", "code": 401}}
```

---

## 13. Error Handling

### Error Format

All errors follow this structure:
```json
{
  "error": {
    "message": "Human-readable description",
    "type": "error_type",
    "code": 401
  }
}
```

### Common Errors

| Code | Type | Meaning |
|------|------|---------|
| 400 | `bad_request` | Invalid JSON body, missing required fields |
| 401 | `unauthorized` | Missing/invalid API key or master key |
| 402 | `budget_exceeded` | User's monthly budget would be exceeded |
| 404 | `not_found` | Key prefix, team ID, or user ID not found |
| 429 | `rate_limited` | Rate limit exceeded (also sets `Retry-After` header) |
| 500 | `internal_error` | Something broke on the server side |
| 502 | `upstream_error` | The upstream provider returned an error |
| 503 | `service_unavailable` | Proxy is draining for config reload |

### Budget Exceeded

```json
{"error": {"message": "Monthly budget exceeded: $49.50 + $0.80 > $50.00", "type": "budget_exceeded", "code": 402}}
```

### Model Not Allowed

If you try to use a model not in your allowlist:
```json
{"error": {"message": "Model not allowed", "type": "forbidden", "code": 403}}
```

### Rate Limited

The response includes a `Retry-After` header telling you when to retry.

---

## 14. Dashboard

llm-pico includes a web-based admin dashboard at `/admin/dashboard`. It provides a GUI for:

- Viewing and creating API keys
- Managing teams and users
- Setting budgets and limits
- Viewing usage statistics
- Live SSE log stream

Open `http://localhost:4000/admin/dashboard` in your browser. Authentication works the same way — pass your master key as a Bearer token.

---

## 15. FAQ / Troubleshooting

### My key stopped working

Check:
1. Is the key active? `GET /admin/keys` — look at `is_active`
2. Is it expired? Check `expires_at` field
3. Was the team or user deactivated? `DELETE /admin/teams/{id}` cascades to users and keys
4. Was the key's prefix revoked? `DELETE /admin/keys/{prefix}`

### Requests are getting 403 (model not allowed)

Your key, user, or team has a model allowlist that doesn't include the model you're requesting. Check:
```bash
curl -s http://localhost:4000/admin/keys \
  -H "Authorization: Bearer YOUR_MASTER_KEY" | jq '.keys[].model_allowlist'
```

Or remove the restriction:
```bash
curl -s -X PUT http://localhost:4000/admin/keys/{prefix}/models \
  -H "Authorization: Bearer YOUR_MASTER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"models": null}'
```

### Requests are getting 429 (rate limited)

Check your effective limits. The minimum of key/user/team limits applies:
```bash
curl -s http://localhost:4000/admin/keys \
  -H "Authorization: Bearer YOUR_MASTER_KEY" | jq '.keys[] | {key_prefix, rpm_limit, rpd_limit}'
```

Also check model-level limits in your config.yaml — those are separate.

### How do I see who's using what?

```bash
# All usage stats
curl -s "http://localhost:4000/admin/usage?from=2026-07-01&to=2026-07-11" \
  -H "Authorization: Bearer YOUR_MASTER_KEY"

# Top models
curl -s "http://localhost:4000/admin/usage/top-models" \
  -H "Authorization: Bearer YOUR_MASTER_KEY"

# Cost breakdown by user
curl -s "http://localhost:4000/admin/stats/costs?group_by=user" \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

### YAML keys aren't showing up

- Check the key format: must be `sk-pico-*` (prefix matters for identification)
- Check the log for "seeded N user keys" on startup
- YAML keys use `INSERT OR IGNORE` — if the key hash already exists, it's silently skipped
- YAML keys are not assigned to users (no `user_id`). Use the admin API to assign them

### How do I reload config without restarting?

```bash
curl -s -X POST http://localhost:4000/admin/config/reload \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

This drains in-flight requests (up to 120s), then restarts the process with the new config. Zero downtime for new requests.

### Can I use the master key as a proxy key?

Yes. The master key works for both admin and proxy endpoints. But it has **no rate limits** and **no budget checks** — it bypasses all restrictions. Don't give it to untrusted clients.

### What's the difference between DELETE on a key vs deactivating a team?

- `DELETE /admin/keys/{prefix}` — revokes one key. Soft-delete (sets `is_active=0`). Other keys and users are unaffected.
- `DELETE /admin/teams/{id}` — deactivates the team AND all its users AND all their keys. Cascading. This is the nuclear option.

### My budget check isn't blocking

Budget is checked using estimated cost before the request. If the upstream provider doesn't return token counts, the estimated cost might be zero. Check that your provider returns `usage` in the response.

### How do I set up YAML users with the admin API?

Two approaches:

1. **YAML only** — define keys in `users.yaml`, start the proxy. They're imported at startup. Manage via admin API afterward.
2. **Admin API only** — skip `users.yaml`, create everything via `POST /admin/keys`. More flexible for dynamic environments.
3. **Both** — YAML for initial keys, admin API for runtime changes. YAML keys with the same hash are skipped on restart.

---

## Quick Reference: All Admin Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/admin/keys` | List all keys |
| `POST` | `/admin/keys` | Create key |
| `DELETE` | `/admin/keys/{prefix}` | Revoke key |
| `PUT` | `/admin/keys/{prefix}/models` | Update key model allowlist |
| `PUT` | `/admin/keys/{prefix}/limits` | Update key rate limits |
| `PUT` | `/admin/keys/{prefix}/user` | Assign key to user |
| `POST` | `/admin/teams` | Create team |
| `GET` | `/admin/teams` | List teams |
| `GET` | `/admin/teams/{team_id}` | Get team + month spend |
| `PUT` | `/admin/teams/{team_id}/limits` | Team rate limits |
| `PUT` | `/admin/teams/{team_id}/models` | Team model allowlist |
| `DELETE` | `/admin/teams/{team_id}` | Deactivate team (cascading) |
| `GET` | `/admin/teams/{team_id}/usage` | Team usage stats |
| `POST` | `/admin/teams/{team_id}/users` | Create user |
| `GET` | `/admin/teams/{team_id}/users` | List team users |
| `GET` | `/admin/users/{user_id}` | Get user + month spend |
| `PUT` | `/admin/users/{user_id}/limits` | User rate limits |
| `PUT` | `/admin/users/{user_id}/budget` | Set monthly budget |
| `PUT` | `/admin/users/{user_id}/models` | User model allowlist |
| `GET` | `/admin/users/{user_id}/usage` | User usage stats |
| `GET` | `/admin/budgets` | Budget overview (all users) |
| `GET` | `/admin/usage` | Global usage stats |
| `GET` | `/admin/usage/top-models` | Top models by tokens |
| `GET` | `/admin/stats/costs` | Cost breakdown |
| `GET` | `/admin/log` | Admin audit log |
| `GET` | `/admin/logs` | Live log dashboard (HTML) |
| `GET` | `/admin/logs/stream` | SSE live log stream |
| `POST` | `/admin/config/reload` | Graceful config reload |
