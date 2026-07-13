# Deployment

## Docker

### Quick Start

```bash
docker run -d -p 4000:4000 \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/users.yaml:/app/users.yaml \
  -v $PWD/keys.yaml:/app/keys.yaml \
  llm-pico:latest
```

### Docker Compose

The init wizard generates a `docker-compose.yml`:

```yaml
version: "3.8"
services:
  llm-pico:
    build: .
    ports:
      - "4000:4000"
    volumes:
      - ./config.yaml:/app/config.yaml
      - ./users.yaml:/app/users.yaml
      - ./keys.yaml:/app/keys.yaml
      - ./data:/app/data
    env_file:
      - .env
    restart: unless-stopped
```

```bash
docker compose up -d
```

### Environment Variables

For `ENV/XXX` key references or Cloudflare account ID:

```bash
docker run -d -p 4000:4000 \
  -e OPENAI_API_KEY="sk-..." \
  -e CLOUDFLARE_ACCOUNT_ID="..." \
  -v $PWD/config.yaml:/app/config.yaml \
  -v $PWD/keys.yaml:/app/keys.yaml \
  llm-pico:latest
```

## Production Setup

### Reverse Proxy

Run behind Caddy or nginx for TLS termination:

```bash
# llm-pico (bind to localhost only)
llm-pico --host 127.0.0.1 --port 4000 --config /etc/llm-pico/config.yaml
```

Caddy example:

```
api.example.com {
    reverse_proxy 127.0.0.1:4000
}
```

### Systemd Service

```ini
[Unit]
Description=llm-pico LLM proxy
After=network.target

[Service]
Type=simple
User=llm-pico
ExecStart=/usr/local/bin/llm-pico --config /etc/llm-pico/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

### File Locations

```
/etc/llm-pico/
├── config.yaml          # Main config
├── users.yaml           # User keys
├── keys.yaml            # Provider keys
└── data/
    └── llm-pico.db      # SQLite database
```

## Graceful Shutdown

When the proxy receives SIGTERM or SIGINT:

1. Sets `_is_draining` flag
2. New requests return 503 with `Retry-After: 30`
3. Waits for in-flight requests to complete (up to 120 seconds)
4. Flushes rate limit counters to SQLite
5. Closes HTTP client connections
6. Closes database connections
7. Process exits

## Config Hot-Reload

Reload config without downtime:

```bash
curl -X POST http://localhost:4000/admin/config/reload \
  -H "Authorization: Bearer sk-pico-master-..."
```

**Behavior:**

1. Drains in-flight requests (same as graceful shutdown)
2. Restarts process with `os.execve` (same arguments)
3. New process loads updated config
4. Zero-downtime update

## Database

### SQLite

- **File:** `llm-pico.db` (next to config by default)
- **Mode:** WAL (Write-Ahead Logging) for concurrent reads
- **Pool:** 2 connections
- **PRAGMAs:** busy_timeout=2s, synchronous=NORMAL, mmap_size=64MB

### Tables

| Table | Purpose |
|-------|---------|
| `teams` | Team definitions |
| `users` | User definitions |
| `user_keys` | API key hashes and metadata |
| `usage_log` | Request usage tracking |
| `rate_counters` | Daily rate limit counters |
| `admin_log` | Admin audit log |

### Log Pruning

Background task runs hourly:

- Deletes `usage_log` entries older than `usage_log_retention_days` (default 30)
- Deletes `admin_log` entries older than `admin_log_retention_days` (default 90)

## Resource Usage

Measured on Intel Atom D410 (1 core / 2 threads @ 1.66 GHz), 2 GB DDR2:

| Concurrency | RAM |
|-------------|-----|
| Idle | ~54MB |
| 10 concurrent | ~91MB |
| 50 concurrent | ~127MB |

Startup time: ~1.9s

## Monitoring

### Health Check

```bash
curl http://localhost:4000/health
# {"status":"ok","version":"0.1.0"}
```

### Live Logs

```bash
# SSE stream
curl -N "http://localhost:4000/admin/logs/stream?token=sk-pico-master-..."

# HTML dashboard
open http://localhost:4000/admin/logs
```

### Usage Stats

```bash
curl http://localhost:4000/admin/usage \
  -H "Authorization: Bearer sk-pico-master-..."

curl http://localhost:4000/admin/usage/top-models \
  -H "Authorization: Bearer sk-pico-master-..."

curl http://localhost:4000/admin/stats/costs \
  -H "Authorization: Bearer sk-pico-master-..."
```

## Security Checklist

- [ ] Set a strong `master_key` (not the default)
- [ ] Bind to localhost behind reverse proxy
- [ ] Enable TLS via reverse proxy
- [ ] Restrict CORS in production (modify `CORSMiddleware`)
- [ ] Use `KEYS/XXX` for key rotation (not literal keys)
- [ ] Set per-user budgets for cost control
- [ ] Monitor usage via admin API
- [ ] Regular database backups
