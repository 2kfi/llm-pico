# llm-pico

![llm-pico](./assets/banner.jpg)

**One config. Every provider. Round-robin key rotation.**

Hyper-lightweight LLM proxy with an OpenAI-compatible API. Route requests to OpenAI, Anthropic, Gemini, Cloudflare, Cohere, ZAI, Groq, NVIDIA NIM, OpenRouter — or any OpenAI-compatible endpoint.

```bash
pip install llm-pico
llm-pico init        # interactive wizard
# edit keys.yaml     # add your API keys (with backups for rotation)
llm-pico             # start proxying
```

## Quick Start

```bash
# 1. Install
pip install llm-pico

# 2. Run the init wizard (selects providers, fetches models, generates config)
llm-pico init

# 3. Add backup keys to keys.yaml for rotation (optional but recommended)
# OPENAI_API_KEY:
#   - "sk-primary"
#   - "sk-backup"

# 4. Start the proxy
llm-pico
```

Then use it like any OpenAI endpoint:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer $(python -c "import yaml; print(yaml.safe_load(open('users.yaml'))['users'][0]['key'])")" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4", "messages": [{"role": "user", "content": "hello"}]}'
```

## Documentation

| Topic | Description |
|-------|-------------|
| [Quick Start](docs/QUICK_START.md) | Detailed setup walkthrough |
| [Configuration](docs/CONFIG.md) | `config.yaml` reference — every field explained |
| [Key Management](docs/KEYS.md) | `keys.yaml`, rotation, `KEYS/` vs `ENV/` |
| [Users](docs/USERS.md) | `users.yaml` format and user management |
| [API Endpoints](docs/ENDPOINTS.md) | Every endpoint with examples |
| [Admin API](docs/ADMIN.md) | Key/team/user/budget management |
| [Providers](docs/PROVIDERS.md) | Built-in providers, custom adapters |
| [Authentication](docs/AUTH.md) | Master key, API keys, model allowlists |
| [Rate Limiting](docs/RATE_LIMITING.md) | 6-window limits, budgets, headers |
| [Routing](docs/ROUTING.md) | Key rotation, circuit breaker, failover |
| [Deployment](docs/DEPLOYMENT.md) | Docker, production setup, graceful shutdown |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | Common errors and fixes |

## Contributing

```bash
git clone https://github.com/your-org/llm-pico
cd llm-pico
python -m venv .venv && source .venv/bin/activate
pip install -e ".[dev]"
pytest
```

## Feedback

Found a bug? Have a feature request? Open an issue at [github.com/your-org/llm-pico/issues](https://github.com/your-org/llm-pico/issues).

## License

MIT
