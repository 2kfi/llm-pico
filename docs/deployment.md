# Deployment Guide

llm-pico is a single Python process with no external service dependencies (just SQLite on local disk). This guide covers running it in production.

---

## Requirements

- Python 3.11+
- SQLite (included in Python stdlib)
- A reverse proxy for TLS termination (nginx, Caddy, etc.)
- ~200MB RAM at 50 concurrent clients

---

## Quick Start (Direct)

```bash
pip install llm-pico

# Set secrets via environment
export LLM_PICO_MASTER_KEY=$(openssl rand -hex 32)
export OPENAI_API_KEY="sk-..."

# Run
llm-pico --config config.yaml --users users.yaml --verbose
```

The server starts on `http://0.0.0.0:4000`. All endpoints are plain HTTP — use a reverse proxy for TLS.

---

## Docker

### Build from source

```bash
docker build -t llm-pico .
```

### Run

```bash
docker run -d \
  --name llm-pico \
  -p 4000:4000 \
  -v /path/to/config.yaml:/app/config.yaml:ro \
  -v /path/to/users.yaml:/app/users.yaml:ro \
  -v llm-pico-data:/app/data \
  -e LLM_PICO_MASTER_KEY=$(openssl rand -hex 32) \
  -e OPENAI_API_KEY="sk-..." \
  llm-pico
```

The container runs as a non-root `appuser`. The SQLite database persists in the `/app/data` volume.

### Docker Compose

```yaml
services:
  llm-pico:
    build: .
    ports:
      - "4000:4000"
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./users.yaml:/app/users.yaml:ro
      - llm-pico-data:/app/data
    environment:
      - LLM_PICO_MASTER_KEY=${LLM_PICO_MASTER_KEY}
      - OPENAI_API_KEY=${OPENAI_API_KEY}
      - GEMINI_API_KEY=${GEMINI_API_KEY}
    restart: unless-stopped

volumes:
  llm-pico-data:
```

---

## Systemd Service

Create `/etc/systemd/system/llm-pico.service`:

```ini
[Unit]
Description=llm-pico LLM proxy
After=network.target

[Service]
Type=simple
User=llm-pico
Group=llm-pico
WorkingDirectory=/etc/llm-pico
ExecStart=/usr/local/bin/llm-pico --config config.yaml --users users.yaml --db /var/lib/llm-pico/llm-pico.db
Restart=on-failure
RestartSec=5
EnvironmentFile=-/etc/llm-pico/env

# Security hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/llm-pico
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

Create `/etc/llm-pico/env`:
```
LLM_PICO_MASTER_KEY=<your-master-key>
OPENAI_API_KEY=<your-openai-key>
GEMINI_API_KEY=<your-gemini-key>
```

```bash
sudo useradd -r -s /bin/false llm-pico
sudo mkdir -p /etc/llm-pico /var/lib/llm-pico
sudo chown llm-pico:llm-pico /var/lib/llm-pico
sudo systemctl daemon-reload
sudo systemctl enable --now llm-pico
```

---

## Reverse Proxy (nginx)

llm-pico serves plain HTTP. Put nginx in front for TLS termination and rate limiting:

```nginx
server {
    listen 443 ssl http2;
    server_name llm.example.com;

    ssl_certificate /etc/letsencrypt/live/llm.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/llm.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:4000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # For streaming responses
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 300s;
    }
}
```

### Caddy (alternative)

```
llm.example.com {
    reverse_proxy localhost:4000
}
```

Caddy auto-configures TLS via Let's Encrypt.

---

## Configuration

See [config.md](config.md) for the full reference. Minimal `config.yaml`:

```yaml
general_settings:
  master_key: "${LLM_PICO_MASTER_KEY}"

router_settings:
  num_retries: 2
  cooldown_time: 45
  circuit_breaker:
    enabled: true
    failure_threshold: 3
    recovery_timeout: 30

model_list:
  - model_name: gpt-5.4-mini
    model_params:
      model: openai/gpt-5.4-mini
      api_key: "${OPENAI_API_KEY}"
    rpm: 100
    rpd: 10000
```

Set all secrets via environment variables (never hardcode in YAML).

---

## Health Checks

```
GET /health → {"status": "ok"}
```

Use this for load balancer health checks. Returns 200 when the server is running and not draining.

---

## Config Reload (Zero-Downtime)

```bash
curl -X POST http://localhost:4000/admin/config/reload \
  -H "Authorization: Bearer YOUR_MASTER_KEY"
```

This triggers a graceful drain:
1. New requests get 503 (`draining`)
2. In-flight requests are allowed to complete (up to 120s)
3. The process restarts via `os.execve` with the new config

If drain times out (120s), the process restarts anyway with remaining requests dropped.

---

## Graceful Shutdown

When receiving SIGTERM/SIGINT, the process:
1. Sets `_is_draining = True` — new requests get 503
2. Waits up to 120s for in-flight requests to complete
3. Closes SQLite connections cleanly
4. Exits

---

## Monitoring

### Key Metrics

| Metric | Source |
|--------|--------|
| Request count / latency | `GET /admin/usage` |
| Rate limit remaining | `X-RateLimit-Remaining-*` headers |
| Circuit breaker status | Check logs for `circuit breaker tripped` |
| Active keys | `GET /admin/keys` |
| Monthly spend per user | `GET /admin/budgets` |
| Live requests | `GET /admin/logs/stream` (SSE) |

### Log Levels

- Default: INFO
- `--verbose`: DEBUG (silences httpx/uvicorn access logs)

Set specific loggers to WARNING in production to reduce noise:
```
LLM_PICO_LOG_LEVEL=WARNING
```

---

## Troubleshooting

### Server won't start

- Check `master_key` is non-empty in config
- Check config file path matches `--config` flag
- Check database directory is writable

### 502 errors from upstream

- Check `api_base` URLs are correct for the provider
- Check API keys are valid (test with `curl` directly)
- Check circuit breaker isn't tripped (look for logs)

### High memory usage

- Check `max_concurrent` isn't set too high in httpx client config
- Reduce `cache_max_entries` if caching is enabled
- Check for connection leaks (httpx client pool exhaustion)

### Database locked

llm-pico uses WAL mode with `busy_timeout=2000ms`. If you see SQLite lock errors:
- Ensure only one llm-pico process uses the database file
- Check disk I/O isn't saturated
- Reduce `rate_counters` flush frequency if needed
