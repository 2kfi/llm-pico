# Quick Start

## Prerequisites

- Python 3.11 or later
- At least one API key from a supported provider

## 1. Install

```bash
pip install llm-pico
```

## 2. Run the Init Wizard

```bash
llm-pico init
```

The wizard walks you through:

1. **Select providers** — checkbox from 10 built-in providers
2. **Enter API keys** — one per selected provider
3. **Fetch models** — live from each provider's API (with retry on failure)
4. **Select models** — fuzzy search across all providers
5. **Test models** (optional) — send a test request to verify keys work
6. **Assign capabilities** — embeddings, images, STT, TTS (auto-detected from model names)
7. **Create first user** — with rate limits and model allowlist
8. **Docker option** — generate `docker-compose.yml`
9. **Generate files** — writes `config.yaml`, `users.yaml`, `keys.yaml`, `.env`

## 3. Add Backup Keys (Optional)

Edit `keys.yaml` to add backup keys for rotation:

```yaml
OPENAI_API_KEY:
  - "sk-primary"
  - "sk-backup1"
  - "sk-backup2"
```

When a key gets rate-limited, the proxy automatically rotates to the next one.

## 4. Start the Proxy

```bash
llm-pico
```

The server starts on `http://0.0.0.0:4000`.

## 5. Make Your First Request

Get your user key from `users.yaml`:

```bash
USER_KEY=$(python -c "import yaml; print(yaml.safe_load(open('users.yaml'))['users'][0]['key'])")
```

Then:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer $USER_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Say hello in one sentence."}]
  }'
```

## 6. Verify

Check the proxy is healthy:

```bash
curl http://localhost:4000/health
# {"status":"ok","version":"0.1.0"}

curl http://localhost:4000/v1/models \
  -H "Authorization: Bearer $USER_KEY"
# Lists all available models
```

## What Gets Generated

After `llm-pico init`, you'll have:

```
.
├── config.yaml          # Model routing, rate limits, master key
├── users.yaml           # User API keys and permissions
├── keys.yaml            # Provider API keys (with rotation support)
├── .env                 # Environment variables (for ENV/ references)
└── docker-compose.yml   # (optional) Docker deployment
```

## Next Steps

- [Configuration](CONFIG.md) — understand every config option
- [Key Management](KEYS.md) — add backup keys, understand rotation
- [API Endpoints](ENDPOINTS.md) — full endpoint reference
- [Deployment](DEPLOYMENT.md) — Docker and production setup
