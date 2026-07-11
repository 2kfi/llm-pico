# LLM Pico Admin Dashboard — User Guide

## 1. Introduction

The admin dashboard is a web interface for managing your LLM Pico proxy. You can create and revoke API keys, manage teams and users, track budgets, view usage statistics and costs, and watch live request logs — all in one place.

It is a single-page application (SPA) with a dark theme, sidebar navigation, and hash-based routing.

---

## 2. Getting There

Open your browser and go to:

```
http://localhost:4000/admin/dashboard
```

Replace `localhost` with your server's hostname or IP if you're accessing it remotely. The default port is **4000**.

If you're running behind a reverse proxy (nginx, Caddy, etc.), you may need to proxy `/admin` to the LLM Pico backend. Make sure WebSocket or SSE traffic for logs is not blocked.

The dashboard lives under the `/admin` path prefix, which is protected by master-key authentication.

---

## 3. Logging In

When you first load the page, you'll see a full-screen login modal:

```
┌──────────────────────────────────────┐
│          LLM Pico                    │
│                                      │
│    Enter your master key to          │
│    continue                          │
│                                      │
│    ┌──────────────────────────┐      │
│    │ Master Key               │      │
│    └──────────────────────────┘      │
│                                      │
│    ┌──────────────────────────┐      │
│    │        Connect            │      │
│    └──────────────────────────┘      │
│                                      │
│         (Invalid key)                │
└──────────────────────────────────────┘
```

**What to do:**

1. Type your master key into the password field.
2. Press **Enter** or click **Connect**.
3. The dashboard validates the key by calling `GET /health` with an `Authorization: Bearer <key>` header.
4. If the key is wrong, you'll see "Invalid key" in red and stay on the login screen.

**Session persistence:** Once you log in successfully, the key is saved in your browser's `localStorage` under the key `pico_master_key`. This means you won't need to re-enter it when you close and reopen the page — as long as you're using the same browser and haven't cleared your site data.

**If the session expires or the server restarts:** any API call that returns a 401 or 403 status will automatically log you out and show the login screen again.

> **Troubleshooting:** If you're stuck on the login screen even with the correct key, open your browser's developer tools (F12) → Application → Local Storage, and remove the `pico_master_key` entry. Then reload the page and try again.

---

## 4. Dashboard Overview (#dashboard)

The Dashboard is the landing page. It shows summary statistics and a health check at a glance.

```
┌─────────────────────────────────────────────────────────────┐
│ Dashboard                                        [Refresh] │
├─────────────────────────────────────────────────────────────┤
│ ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐          │
│ │    3    │ │    2    │ │  1,247  │ │ $12.34  │          │
│ │API Keys │ │  Teams  │ │Requests │ │Total Cost│          │
│ └─────────┘ └─────────┘ └─────────┘ └─────────┘          │
│                                                             │
│ ┌─────────────────────────────────────────────────────────┐ │
│ │ ● Healthy — Server responding                           │ │
│ └─────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

**What you see:**

| Card | Description |
|------|-------------|
| API Keys | Total number of API keys (active + revoked) |
| Teams | Total number of teams |
| Total Requests | Lifetime request count across all keys |
| Total Cost | Lifetime cost in USD |

**Health indicator:**

- **Green "Healthy"** badge — the server is reachable and responding.
- **Red "Unreachable"** badge — the server is down or not responding. Check if the LLM Pico process is running.

Click **Refresh** at the top right to reload the data.

Data is pulled from four endpoints: `/admin/keys`, `/admin/teams`, `/admin/usage`, and `/health`.

---

## 5. Key Management (#keys)

The Keys page lets you create, view, and revoke API keys.

```
┌─────────────────────────────────────────────────────────────┐
│ API Keys                                     [+ Create Key]│
├─────────────────────────────────────────────────────────────┤
│ PREFIX      LABEL       ACTIVE     USER      CREATED       │
│ ───────     ─────       ──────     ────      ───────       │
│ sk-pico-a1  prod-serv   ● Active   user@..   2h ago [Revoke]│
│ sk-pico-b2  dev-box     ● Active   -         5d ago [Revoke]│
│ sk-pico-c3  old-test    ○ Revoked  -        12d ago       │
└─────────────────────────────────────────────────────────────┘
```

**Columns:**

| Column | Description |
|--------|-------------|
| Prefix | The first part of the API key (e.g. `sk-pico-a1bc...`). Click to select the full key prefix. |
| Label | A human-readable name you gave the key. |
| Active | Green "Active" badge or red "Revoked" badge. |
| User | The user the key is assigned to (if any). |
| Created | Relative time (e.g. "2h ago", "5d ago"). |

### Creating a Key

Click **"+ Create Key"** to open a modal:

```
┌──────────────────────────────────────┐
│       Create API Key                 │
│                                      │
│  Label                               │
│  ┌──────────────────────────────┐    │
│  │ e.g. prod-server             │    │
│  └──────────────────────────────┘    │
│                                      │
│  Models (comma-separated, empty=all) │
│  ┌──────────────────────────────┐    │
│  │ gpt-4, claude-3              │    │
│  └──────────────────────────────┘    │
│                                      │
│           [Cancel]  [Create]         │
└──────────────────────────────────────┘
```

- **Label** — Required. Give it a descriptive name like "prod-server" or "dev-box".
- **Models** — Optional. Comma-separated list of model names this key is allowed to use. Leave empty to allow all models.

After clicking **Create**, the key is generated and shown **once**:

```
┌──────────────────────────────────────┐
│       Key Created                    │
│                                      │
│  ⚠ Copy this key now. It won't be   │
│    shown again.                      │
│                                      │
│  ┌──────────────────────────────┐    │
│  │ sk-pico-a1b2c3d4e5f6...     │    │
│  └──────────────────────────────┘    │
│                                      │
│              [Close]                 │
└──────────────────────────────────────┘
```

**IMPORTANT:** Copy the key immediately and store it somewhere safe (password manager, vault, etc.). The raw key is displayed only this one time — if you close the modal without copying, you will never see it again and will need to create a new key.

### Revoking a Key

Click the **Revoke** button next to any active key. A confirmation dialog will appear. Revoking soft-deletes the key by setting `is_active = 0` in the database. Revoked keys cannot be used to make API calls, but they remain in the table with a "Revoked" badge.

---

## 6. Teams & Users (#teams)

The Teams & Users page shows teams as cards in a grid. Each card lists the team's users underneath.

```
┌─────────────────────────────────────────────────────────────┐
│ Teams & Users                               [+ Create Team]│
├─────────────────────────────────────────────────────────────┤
│ ┌────────────────────────┐ ┌──────────────────────────────┐ │
│ │ Engineering        [×] │ │ Marketing              [×]  │ │
│ │                        │ │                              │ │
│ │ Core engineering team  │ │ Marketing department team    │ │
│ │                        │ │                              │ │
│ │ alice@co...  Alice     │ │ charlie@co...  Charlie       │ │
│ │ bob@comp...  Bob       │ │ diana@co...   Diana          │ │
│ │            [+ User]    │ │              [+ User]        │ │
│ └────────────────────────┘ └──────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

### Creating a Team

Click **"+ Create Team"** → fill in Name and optional Description.

Teams group users together. Deleting a team will cascade-delete all its users and their keys. There is no undo.

### Adding a User

Click **"+ User"** on any team card → enter the user's email and display name.

### Deleting a Team

Click the **"×"** button on a team card. You'll be asked to confirm. After deletion:
- The team is gone.
- All users in that team are deleted.
- Any API keys assigned to those users are also deleted.

> **Warning:** Deleting a team is permanent and cascading. Make sure you want to remove everything.

---

## 7. Budget Tracking (#budgets)

The Budgets page shows each user's monthly budget and current spend with color-coded progress bars.

```
┌─────────────────────────────────────────────────────────────┐
│ Budgets                                        [Refresh]   │
├─────────────────────────────────────────────────────────────┤
│ ┌─────────────────────────────────────────────────────────┐ │
│ │ alice@example.com                Engineering            │ │
│ │ Budget: $100.00   Spent: $45.23   45.2%                │ │
│ │ ▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░ │ │
│ └─────────────────────────────────────────────────────────┘ │
│ ┌─────────────────────────────────────────────────────────┐ │
│ │ bob@company.com                   Engineering            │ │
│ │ Budget: $200.00   Spent: $175.50  87.8%                │ │
│ │ ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░░░░░░░ │ │
│ └─────────────────────────────────────────────────────────┘ │
│ ┌─────────────────────────────────────────────────────────┐ │
│ │ charlie@example.com               Marketing              │ │
│ │ Budget: $50.00    Spent: $48.90   97.8%                 │ │
│ │ ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░│ │
│ └─────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

**Color coding:**

| Percentage | Color | Meaning |
|-----------|-------|---------|
| 0% – 69%  | Green | Under budget |
| 70% – 90%  | Orange | Approaching the limit |
| > 90%     | Red | Near or over budget |

Users without a budget set show "None" for the budget field and no progress bar.

Data comes from `GET /admin/budgets`, which joins user records with current-month usage.

---

## 8. Usage & Costs (#usage)

The Usage & Costs page provides a high-level overview of usage statistics, a top-models table, and a configurable cost breakdown chart.

```
┌─────────────────────────────────────────────────────────────┐
│ Usage & Costs                             [By User ▾]      │
├─────────────────────────────────────────────────────────────┤
│ ┌─────────┐ ┌───────────┐ ┌──────────┐ ┌─────────┐       │
│ │  1,247  │ │ 5,280,931 │ │ $12.34   │ │   3     │       │
│ │Requests │ │  Tokens   │ │Total Cost│ │Act. Keys│       │
│ └─────────┘ └───────────┘ └──────────┘ └─────────┘       │
│                                                             │
│ ┌─────────────────────┐ ┌──────────────────────────────┐   │
│ │ Top Models          │ │ Cost Breakdown               │   │
│ │───────  ──────  ───┐│ │                              │   │
│ │Model    Reqs  Tokens││ │ ██                           │   │
│ │gpt-4    520   2.1M  ││ │ ██ ██                        │   │
│ │claude-3 380   1.5M  ││ │ ██ ██ ██                     │   │
│ │gpt-3.5  200   900k  ││ │ ██ ██ ██ ██                  │   │
│ │llama-2  147   780k  ││ │ A  B  C  D  ...             │   │
│ └─────────────────────┘ └──────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**Stat cards:** Total Requests, Total Tokens, Total Cost, and Active Keys.

**Top Models table:** Shows which models are being used the most, with request count, token count, and cost.

**Cost Breakdown chart:** A bar chart that groups cost by:

| Dropdown option | Groups by |
|----------------|-----------|
| By User | Cost per user |
| By Model | Cost per model |
| By Day | Cost per day |

The chart shows the top 12 items. The raw values are listed below the bars.

---

## 9. Live Logs (#logs)

The Live Logs page streams request logs in real time using Server-Sent Events (SSE) from `/admin/logs/stream`.

```
┌─────────────────────────────────────────────────────────────┐
│ Live Logs                              [Clear] [● Connected]│
├─────────────────────────────────────────────────────────────┤
│ 2026-07-11T10:15:23 gpt-4 [200] tokens:452 cost:$0.0090   │
│ 2026-07-11T10:15:24 claude-3 [200] tokens:128 cost:$0.0038 │
│ 2026-07-11T10:15:25 gpt-4 [500] tokens:0   cost:$0.0000   │
│ 2026-07-11T10:15:26 gpt-3.5 [200] tokens:84  cost:$0.0004 │
│ 2026-07-11T10:15:27 llama-2 [429] tokens:0   cost:$0.0000 │
│ ...                                                         │
└─────────────────────────────────────────────────────────────┘
```

**Features:**

- **Color-coded lines:**
  - Red: Error responses (status codes 4xx, 5xx)
  - Green: Successful responses (status 200)
- **Auto-scroll:** New lines appear at the bottom and the viewport scrolls down automatically.
- **500-line cap:** Once the container reaches 500 log lines, the oldest lines are removed.
- **Clear button:** Empties the log display.
- **Connection status:** Green "Connected" badge when streaming, turns red "Disconnected" when the connection drops.
- **Each line** shows: timestamp, model name, status code in brackets, token count, and cost.

**Troubleshooting:** If the status shows "Disconnected", the SSE connection dropped. Navigate away and back to the Live Logs tab, or refresh the page to reconnect.

### Standalone Log Page

A separate standalone log viewer is available at:

```
http://localhost:4000/admin/logs
```

This is not part of the SPA — it is a plain HTML page with its own SSE connection. It parses structured JSON events (with timestamp, model, status, tokens, and cost fields) and uses a 1000-line cap. Useful if you want to keep logs open in a separate browser tab or window.

```
┌─────────────────────────────────────────────────────────────┐
│ llm-pico  live log stream                                  │
├─────────────────────────────────────────────────────────────┤
│ 2026-07-11T10:15:23 gpt-4 [200] tokens:452 cost:$0.009000  │
│ 2026-07-11T10:15:24 claude-3 [200] tokens:128 cost:$0.003800│
│ 2026-07-11T10:15:27 llama-2 [429] tokens:0   cost:$0.000000│
│ ...                                                         │
└─────────────────────────────────────────────────────────────┘
```

---

## 10. FAQ / Common Issues

### Q: I lost the master key. What do I do?

You cannot recover it from the dashboard. The master key is set in your `llm-pico` configuration file (`config.yaml` or environment variable). Check the server configuration to find or reset it, then restart the server.

### Q: I created a key but forgot to copy it. Can I see it again?

No. The raw key is shown once and never stored in plaintext (only a hash is stored). You must create a new key.

### Q: The dashboard keeps logging me out.

This means API calls are returning 401 or 403. Possible causes:

- The master key was changed or the server was restarted with a new config.
- Your `localStorage` data got corrupted. Clear it (F12 → Application → Local Storage → delete `pico_master_key`) and log in again.
- A reverse proxy or firewall is stripping the `Authorization` header.

### Q: The health check shows "Unreachable" but the server is running.

Make sure the dashboard URL points to the LLM Pico server directly — not to a different service. If using a reverse proxy, ensure `/admin` routes are forwarded correctly.

### Q: Budget percentages look wrong.

Budgets are calculated from the start of the current month. Spend data comes from the `usage_log` table. If usage logs were purged or the month just started, the numbers may appear lower than expected.

### Q: Live logs say "Disconnected" and won't reconnect.

The SSE stream uses a 30-second keepalive. If no event is received for 30 seconds, the connection is considered stale. Navigate away from the Live Logs tab and back, or refresh the page.

### Q: Can I delete a user without deleting the team?

Not from the current UI. You'd need to use the API directly: `DELETE /admin/teams/{team_id}/users/{user_id}`.

### Q: The "Top Models" table is empty.

The endpoint returns data only after requests have been logged. Make at least one successful API call through the proxy first.
