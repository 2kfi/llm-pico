from __future__ import annotations

import os
import secrets
import shutil
import sys
from pathlib import Path

import click
import httpx
import questionary
import yaml

from core.config import load_config

# ── Built-in provider registry ──────────────────────────────────────────────
# NO hardcoded model lists. Everything is fetched live from the provider.

BUILTIN_PROVIDERS: dict[str, dict] = {
    "openai": {
        "label": "OpenAI",
        "env_key": "OPENAI_API_KEY",
        "base_url": "https://api.openai.com/v1",
        "models_url": "https://api.openai.com/v1/models",
    },
    "gemini": {
        "label": "Gemini",
        "env_key": "GEMINI_API_KEY",
        "base_url": "https://generativelanguage.googleapis.com/v1beta/openai",
        "models_url": "https://generativelanguage.googleapis.com/v1beta/openai/models",
    },
    "anthropic": {
        "label": "Anthropic",
        "env_key": "ANTHROPIC_API_KEY",
        "base_url": "https://api.anthropic.com/v1",
        "models_url": None,
    },
    "openrouter": {
        "label": "OpenRouter",
        "env_key": "OPENROUTER_API_KEY",
        "base_url": "https://openrouter.ai/api/v1",
        "models_url": "https://openrouter.ai/api/v1/models",
    },
    "opencode_zen": {
        "label": "OpenCode Zen",
        "env_key": "OPENCODE_ZEN_API_KEY",
        "base_url": "https://opencode.ai/zen/v1",
        "models_url": "https://opencode.ai/zen/v1/models",
    },
    "groq": {
        "label": "Groq",
        "env_key": "GROQ_API_KEY",
        "base_url": "https://api.groq.com/openai/v1",
        "models_url": "https://api.groq.com/openai/v1/models",
    },
    "nvidia_nim": {
        "label": "NVIDIA NIM",
        "env_key": "NVIDIA_NIM_API_KEY",
        "base_url": "https://integrate.api.nvidia.com/v1",
        "models_url": "https://integrate.api.nvidia.com/v1/models",
    },
    "cloudflare": {
        "label": "Cloudflare Workers AI",
        "env_key": "CLOUDFLARE_API_TOKEN",
        "env_account_id": "CLOUDFLARE_ACCOUNT_ID",
        "base_url": None,  # Built dynamically: https://api.cloudflare.com/client/v4/accounts/{ACCOUNT_ID}/ai/v1
        "models_url": None,  # Uses Cloudflare-specific API
    },
    "cohere": {
        "label": "Cohere",
        "env_key": "COHERE_API_KEY",
        "base_url": "https://api.cohere.ai/compatibility/v1",
        "models_url": "https://api.cohere.ai/compatibility/v1/models",
    },
    "zai": {
        "label": "ZAI (GLM)",
        "env_key": "ZAI_API_KEY",
        "base_url": "https://open.bigmodel.cn/api/paas/v4",
        "models_url": "https://open.bigmodel.cn/api/paas/v4/models",
    },
}

CUSTOM_PROVIDER_TYPES = [
    ("custom_openai", "Custom (OpenAI-compatible)"),
    ("custom_anthropic", "Custom (Anthropic-compatible)"),
    ("custom_gemini", "Custom (Gemini-compatible)"),
]

FETCH_TIMEOUT = 8.0


# ── Model fetching ──────────────────────────────────────────────────────────

def _fetch_models(models_url: str, api_key: str | None, provider_label: str) -> list[str] | None:
    """Fetch model list from provider's /v1/models endpoint. Returns None on any failure."""
    headers = {}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"

    try:
        resp = httpx.get(models_url, headers=headers, timeout=FETCH_TIMEOUT)
    except httpx.ConnectError:
        click.echo(f"    ✗ {provider_label}: could not connect to {models_url}")
        return None
    except httpx.TimeoutException:
        click.echo(f"    ✗ {provider_label}: request timed out")
        return None
    except httpx.RequestError as e:
        click.echo(f"    ✗ {provider_label}: network error — {e}")
        return None

    if resp.status_code in (401, 403):
        click.echo(f"    ✗ {provider_label}: authentication failed (HTTP {resp.status_code})")
        return None
    if resp.status_code == 404:
        click.echo(f"    ✗ {provider_label}: models endpoint not found (HTTP 404)")
        return None
    if resp.status_code >= 400:
        click.echo(f"    ✗ {provider_label}: unexpected error (HTTP {resp.status_code})")
        return None

    try:
        data = resp.json()
    except Exception:
        click.echo(f"    ✗ {provider_label}: could not parse response as JSON")
        return None

    models_raw = data.get("data") or data.get("models") or []
    if not models_raw:
        click.echo(f"    ✗ {provider_label}: no models returned")
        return None

    model_ids = []
    for m in models_raw:
        mid = m.get("id") if isinstance(m, dict) else None
        if mid:
            model_ids.append(mid)

    if not model_ids:
        click.echo(f"    ✗ {provider_label}: model list was empty")
        return None

    click.echo(f"    ✓ {provider_label}: fetched {len(model_ids)} models")
    return sorted(model_ids)


def _fetch_with_retry(slug: str, api_key: str, base_url_override: str | None = None) -> list[str] | None:
    """Fetch models. If it fails, offer one retry. If retry fails, return None (skip)."""
    info = BUILTIN_PROVIDERS[slug]

    if not info["models_url"]:
        return None

    # Use override base_url to build models URL
    if base_url_override:
        models_url = base_url_override.rstrip("/") + "/models"
    else:
        models_url = info["models_url"]

    click.echo(f"  Fetching models from {info['label']}...")
    models = _fetch_models(models_url, api_key, info["label"])

    if models:
        return models

    click.echo(f"  Could not fetch models for {info['label']}.")
    retry = questionary.confirm(f"  Retry with a different API key?", default=True).ask()
    if retry is None:
        sys.exit(0)

    if retry:
        new_key = questionary.text(f"  {info['label']} — new API key:").ask()
        if new_key is None:
            sys.exit(0)
        if new_key.strip():
            api_key = new_key.strip()

        click.echo(f"  Retrying...")
        models = _fetch_models(models_url, api_key, info["label"])
        if models:
            return models

    click.echo(f"  Skipping {info['label']} — could not fetch models.")
    return None


def _fetch_custom_with_retry(name: str, base_url: str, api_key: str) -> list[str] | None:
    """Fetch models for a custom provider with one retry."""
    bu = base_url.rstrip("/")

    for suffix in ["/models", "/v1/models"]:
        url = f"{bu}{suffix}"
        click.echo(f"  Fetching models from {name}...")
        models = _fetch_models(url, api_key or None, name)
        if models:
            return models

    click.echo(f"  Could not fetch models for {name}.")
    retry = questionary.confirm(f"  Retry with a different base URL?", default=True).ask()
    if retry is None:
        sys.exit(0)

    if retry:
        new_url = questionary.text(f"  {name} — new base URL:").ask()
        if new_url is None:
            sys.exit(0)
        if new_url.strip():
            bu = new_url.strip().rstrip("/")
            for suffix in ["/models", "/v1/models"]:
                url = f"{bu}{suffix}"
                click.echo(f"  Retrying...")
                models = _fetch_models(url, api_key or None, name)
                if models:
                    return models

    click.echo(f"  Skipping {name} — could not fetch models.")
    return None


# ── Model filtering + selection ─────────────────────────────────────────────

def _filter_models(models: list[str], query: str) -> list[str]:
    """Filter model list by search query (case-insensitive substring match)."""
    if not query.strip():
        return models
    q = query.strip().lower()
    return [m for m in models if q in m.lower()]


def _select_models_with_filter(
    models: list[str],
    provider_label: str,
) -> list[str]:
    """Show filter prompt, then checkbox. Returns selected models."""
    click.echo(f"  {provider_label}: {len(models)} models available")

    filter_raw = questionary.text(
        f"  Type to filter models (e.g. 'gpt', 'claude', 'whisper'), or empty for all:",
        default="",
    ).ask()
    if filter_raw is None:
        sys.exit(0)

    filtered = _filter_models(models, filter_raw or "")

    if not filtered:
        click.echo(f"  No models match '{filter_raw}'. Showing all.")
        filtered = models

    if len(filtered) > 50:
        click.echo(f"  Showing {len(filtered)} models. Use filter to narrow down.")

    choices = [
        questionary.Choice(m, value=m, checked=False)
        for m in filtered
    ]

    picked = questionary.checkbox(
        f"{provider_label} — select models:",
        choices=choices,
    ).ask()

    if picked is None:
        sys.exit(0)

    return picked


# ── Capability auto-detection ───────────────────────────────────────────────

_EMBEDDING_KEYWORDS = {"embedding", "embed", "e5-", "bge-", "nomic", "gte-"}
_STT_KEYWORDS = {"whisper", "stt", "speech-to-text", "speech_recognition", "paraformer"}
_TTS_KEYWORDS = {"tts", "text-to-speech", "speech", "bark", "orpheus", "piper", "coqui"}
_IMAGE_KEYWORDS = {"vision", "vl-", "qwenvl", "llava", "internvl", "cogvlm", "gemini"}


def _detect_capabilities(model_id: str) -> dict[str, bool]:
    """Detect capabilities from model name. Returns pre-checked state."""
    lower = model_id.lower()
    return {
        "embeddings": any(kw in lower for kw in _EMBEDDING_KEYWORDS),
        "stt": any(kw in lower for kw in _STT_KEYWORDS),
        "tts": any(kw in lower for kw in _TTS_KEYWORDS),
        "images": any(kw in lower for kw in _IMAGE_KEYWORDS),
    }


# ── Helpers ─────────────────────────────────────────────────────────────────

def _parse_limits(raw: str) -> dict[str, int]:
    """Parse 'tpm=20K,rpm=100,rpd=10K' into {'tpm': 20000, 'rpm': 100, ...}."""
    suffixes = {"K": 10**3, "M": 10**6, "B": 10**9, "T": 10**12}
    limits: dict[str, int] = {}
    for part in raw.split(","):
        part = part.strip()
        if "=" not in part:
            continue
        key, val = part.split("=", 1)
        key = key.strip().lower()
        val = val.strip().upper()
        if not val:
            continue
        multiplier = 1
        if val and val[-1] in suffixes:
            multiplier = suffixes[val[-1]]
            val = val[:-1]
        try:
            limits[key] = int(float(val) * multiplier)
        except ValueError:
            continue
    return limits


def _generate_master_key() -> str:
    return f"sk-pico-master-{secrets.token_hex(24)}"


def _generate_user_key() -> str:
    return f"sk-pico-demo-{secrets.token_hex(24)}"


# ── Interactive wizard steps ────────────────────────────────────────────────

def _step_select_providers() -> list[str]:
    """Step 1: Provider selection checkboxes (no defaults)."""
    choices = [
        questionary.Choice(p["label"], value=k, checked=False)
        for k, p in BUILTIN_PROVIDERS.items()
    ]
    for slug, label in CUSTOM_PROVIDER_TYPES:
        choices.append(questionary.Choice(label, value=slug, checked=False))

    selected = questionary.checkbox(
        "Which providers do you have API keys for?",
        choices=choices,
    ).ask()

    if selected is None:
        sys.exit(0)
    return selected


def _step_api_keys(providers: list[str]) -> tuple[dict[str, str], dict[str, str], dict[str, str]]:
    """Step 2: Enter API key per provider. Returns (api_keys, base_urls_override, account_ids)."""
    api_keys: dict[str, str] = {}
    base_urls: dict[str, str] = {}
    account_ids: dict[str, str] = {}

    for slug in providers:
        if slug.startswith("custom_"):
            continue

        info = BUILTIN_PROVIDERS[slug]
        click.echo()

        # Cloudflare needs account ID first
        if slug == "cloudflare":
            account_id = questionary.text("Cloudflare — account ID:").ask()
            if account_id is None:
                sys.exit(0)
            account_id = account_id.strip()
            if not account_id:
                click.echo("  Account ID required for Cloudflare. Skipping.")
                continue
            account_ids[slug] = account_id
            base_urls[slug] = f"https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1"

        # Get API key
        prompt = f"{info['label']} — paste your API key:"
        if slug == "opencode_zen":
            prompt = f"{info['label']} — API key (empty for free tier):"
        key = questionary.text(prompt).ask()
        if key is None:
            sys.exit(0)

        if key:
            api_keys[slug] = key

    return api_keys, base_urls, account_ids


def _step_fetch_all_models(
    providers: list[str],
    api_keys: dict[str, str],
    base_urls: dict[str, str],
) -> dict[str, list[str]]:
    """Step 3: Fetch models from all providers. Returns {slug: [model_id, ...]}."""
    all_models: dict[str, list[str]] = {}

    for slug in providers:
        if slug.startswith("custom_"):
            continue

        info = BUILTIN_PROVIDERS[slug]
        key = api_keys.get(slug)

        # Fetch models (with retry) — use override base_url if set
        models = _fetch_with_retry(slug, key or None, base_urls.get(slug))

        if models:
            all_models[slug] = models

    return all_models


def _step_custom_provider_details(selected_custom: list[str]) -> tuple[list[dict], dict[str, list[str]], dict[str, str]]:
    """Step 4: Detailed setup for each selected custom provider type.
    Returns (custom_providers, custom_models, custom_base_urls).
    """
    custom_providers = []
    custom_models: dict[str, list[str]] = {}
    custom_base_urls: dict[str, str] = {}
    type_labels = {slug: label for slug, label in CUSTOM_PROVIDER_TYPES}

    for custom_type in selected_custom:
        label = type_labels.get(custom_type, custom_type)
        count_raw = questionary.text(
            f"How many {label} providers?"
        ).ask()
        if count_raw is None:
            sys.exit(0)

        try:
            count = int(count_raw.strip())
        except (ValueError, AttributeError):
            count = 0

        for i in range(1, count + 1):
            name = questionary.text(f"  Custom provider #{i} — name:").ask()
            if name is None:
                sys.exit(0)
            if not name.strip():
                break

            base_url = questionary.text(f"  {name} — base URL:").ask()
            if base_url is None:
                sys.exit(0)

            api_key = questionary.text(f"  {name} — API key (empty to skip):").ask()
            if api_key is None:
                sys.exit(0)

            # Fetch models (with retry)
            models: list[str] = []
            if base_url.strip():
                models = _fetch_custom_with_retry(
                    name, base_url.strip(), api_key.strip()
                ) or []

            # If no models endpoint, ask user to type names
            if not models:
                raw = questionary.text(
                    f"  {name} — type model names (comma-separated, or empty to skip):",
                    default="",
                ).ask()
                if raw is None:
                    sys.exit(0)
                models = [m.strip() for m in raw.split(",") if m.strip()]

            safe_name = name.strip()
            custom_providers.append({
                "name": safe_name,
                "base_url": base_url.strip(),
                "api_key": api_key.strip(),
            })
            if models:
                custom_models[safe_name] = models
                custom_base_urls[safe_name] = base_url.strip()

    return custom_providers, custom_models, custom_base_urls

    return custom_providers


def _step_assign_capabilities(
    selected_models: dict[str, list[str]],
    custom_providers: list[dict],
) -> dict[str, dict[str, bool]]:
    """Step 5: Per-capability checkboxes with auto-detection from model names."""
    all_models: list[tuple[str, str]] = []

    for slug, models in selected_models.items():
        for m in models:
            all_models.append((m, slug))

    if not all_models:
        return {}

    capabilities = {
        "embeddings": "Which models support embeddings?",
        "images": "Which models support images?",
        "stt": "Which models support STT (speech-to-text)?",
        "tts": "Which models support TTS (text-to-speech)?",
    }

    # Pre-detect capabilities from model names
    model_caps: dict[str, dict[str, bool]] = {}
    for display, _slug in all_models:
        detected = _detect_capabilities(display)
        model_caps[display] = detected

    for cap_key, cap_label in capabilities.items():
        choices = [
            questionary.Choice(
                display,
                value=display,
                checked=model_caps[display][cap_key],
            )
            for display, _ in all_models
        ]

        picked = questionary.checkbox(cap_label, choices=choices).ask()
        if picked is None:
            sys.exit(0)

        # Reset all to False, then set picked ones to True
        for display in all_models:
            model_caps[display[0]][cap_key] = False
        for display in picked:
            if display in model_caps:
                model_caps[display][cap_key] = True

    # Warn about STT/TTS on unsupported providers
    unsupported_stt_tts = {"anthropic", "gemini"}
    warnings = []
    for display, slug in all_models:
        if slug in unsupported_stt_tts:
            if model_caps[display].get("stt"):
                warnings.append(f"  - {display} (STT not supported by {slug})")
            if model_caps[display].get("tts"):
                warnings.append(f"  - {display} (TTS not supported by {slug})")
    if warnings:
        click.echo()
        click.echo("  WARNING: The following models have STT/TTS enabled but their provider doesn't support it:")
        for w in warnings:
            click.echo(w)
        click.echo("  These will fail at runtime. Uncheck them in the capability step if needed.")
        click.echo()

    return model_caps


def _step_first_user(all_model_names: list[str]) -> tuple[str, dict[str, int], list[str] | None]:
    """Step 6: First user name, limits, and model allowlist."""
    name = questionary.text("First user name:", default="demo-user").ask()
    if name is None:
        sys.exit(0)
    name = name.strip() or "demo-user"

    limits_raw = questionary.text(
        "Rate limits? (comma-separated, e.g. tpm=20K,rpm=100,rpd=10K)",
        default="",
    ).ask()
    if limits_raw is None:
        sys.exit(0)

    limits = _parse_limits(limits_raw or "")

    # Model allowlist
    allowlist: list[str] | None = None
    if all_model_names:
        select_all = questionary.confirm(
            f"Allow demo-user to use ALL {len(all_model_names)} models? (recommended)",
            default=True,
        ).ask()
        if select_all is None:
            sys.exit(0)

        if not select_all:
            choices = [
                questionary.Choice(m, value=m, checked=True)
                for m in all_model_names
            ]
            picked = questionary.checkbox(
                "Which models can demo-user access?",
                choices=choices,
            ).ask()
            if picked is None:
                sys.exit(0)
            allowlist = picked if picked else None

    return name, limits, allowlist


def _step_docker() -> bool:
    """Step 7: Docker-compose option."""
    return questionary.confirm("Generate docker-compose.yml?", default=False).ask() or False


# ── Config generation ──────────────────────────────────────────────────────

def _generate_config(
    providers: list[str],
    api_keys: dict[str, str],
    custom_providers: list[dict],
    selected_models: dict[str, list[str]],
    model_caps: dict[str, dict[str, bool]],
    master_key: str,
    base_urls_override: dict[str, str] | None = None,
) -> dict:
    """Build the config dict ready for YAML serialization."""
    model_list = []

    for slug in providers:
        if slug.startswith("custom_"):
            continue

        info = BUILTIN_PROVIDERS[slug]
        env_key_name = info["env_key"]
        base_url = (base_urls_override or {}).get(slug) or info["base_url"]

        for model_id in selected_models.get(slug, []):
            entry: dict = {
                "model_name": model_id,
                "model_params": {
                    "model": f"{slug}/{model_id}",
                    "api_key": f"KEYS/{env_key_name}",
                },
            }

            if base_url:
                entry["model_params"]["api_base"] = base_url

            caps = model_caps.get(model_id, {})
            if caps.get("images"):
                entry["images"] = True
            if caps.get("embeddings"):
                entry["embeddings"] = True
            if caps.get("stt"):
                entry["stt"] = True
            if caps.get("tts"):
                entry["tts"] = True

            model_list.append(entry)

    for cp in custom_providers:
        cp_name = cp["name"]
        for model_id in selected_models.get(cp_name, []):
            entry = {
                "model_name": model_id,
                "model_params": {
                    "model": model_id,
                    "api_base": cp["base_url"],
                },
            }
            if cp["api_key"]:
                entry["model_params"]["api_key"] = cp["api_key"]
            else:
                safe_name = cp_name.upper().replace("-", "_").replace(" ", "_")
                entry["model_params"]["api_key"] = f"KEYS/{safe_name}_API_KEY"

            caps = model_caps.get(model_id, {})
            if caps.get("images"):
                entry["images"] = True
            if caps.get("embeddings"):
                entry["embeddings"] = True
            if caps.get("stt"):
                entry["stt"] = True
            if caps.get("tts"):
                entry["tts"] = True

            model_list.append(entry)

    return {
        "router_settings": {
            "num_retries": 2,
            "cooldown_time": 45,
        },
        "general_settings": {
            "master_key": master_key,
            "usage_log_retention_days": 30,
            "admin_log_retention_days": 90,
        },
        "model_list": model_list,
    }


def _generate_users(
    user_name: str,
    limits: dict[str, int],
    allowlist: list[str] | None,
) -> tuple[dict, str]:
    """Build the users dict."""
    user_key = _generate_user_key()
    user_entry: dict = {
        "key": user_key,
        "label": user_name,
    }
    if allowlist:
        user_entry["models"] = allowlist
    if limits:
        user_entry.update(limits)

    return {"users": [user_entry]}, user_key


def _generate_docker_compose() -> dict:
    """Build docker-compose.yml dict."""
    return {
        "version": "3.8",
        "services": {
            "llm-pico": {
                "build": ".",
                "ports": ["4000:4000"],
                "volumes": [
                    "./config.yaml:/app/config.yaml",
                    "./users.yaml:/app/users.yaml",
                    "./keys.yaml:/app/keys.yaml",
                    "./data:/app/data",
                ],
                "env_file": [".env"],
                "restart": "unless-stopped",
            }
        },
    }


def _generate_env_file(providers: list[str], api_keys: dict[str, str], account_ids: dict[str, str] | None = None) -> str:
    """Build .env content."""
    lines = []
    for slug in providers:
        if slug.startswith("custom_"):
            continue
        info = BUILTIN_PROVIDERS[slug]
        key_val = api_keys.get(slug, "")
        lines.append(f'{info["env_key"]}="{key_val}"')
        # Cloudflare also needs account ID
        if slug == "cloudflare" and account_ids and account_ids.get(slug):
            lines.append(f'CLOUDFLARE_ACCOUNT_ID="{account_ids[slug]}"')
    return "\n".join(lines) + "\n"


def _generate_keys_file(providers: list[str], api_keys: dict[str, str], custom_providers: list[dict] | None = None) -> dict:
    """Build keys.yaml dict. Each key is a list for rotation support."""
    keys = {}
    for slug in providers:
        if slug.startswith("custom_"):
            continue
        info = BUILTIN_PROVIDERS[slug]
        key_val = api_keys.get(slug, "")
        if key_val:
            keys[info["env_key"]] = [key_val]
    # Custom providers
    for cp in (custom_providers or []):
        if cp.get("api_key"):
            safe_name = cp["name"].upper().replace("-", "_").replace(" ", "_")
            keys[f"{safe_name}_API_KEY"] = [cp["api_key"]]
    return keys


# ── Model selection (InquirerPy fuzzy) ──────────────────────────────────────

def _step_select_models(
    all_models: dict[str, list[str]],
    api_keys: dict[str, str],
    base_urls: dict[str, str],
) -> dict[str, list[str]]:
    """Fuzzy multi-select across all providers. Returns {slug: [model_id, ...]}."""
    from InquirerPy import inquirer

    # Build choices: one entry per model, grouped by provider
    choices = []
    for slug, models in all_models.items():
        for model_id in models:
            choices.append({"name": f"{slug}/{model_id}", "value": (slug, model_id)})

    if not choices:
        return {}

    picked = inquirer.fuzzy(
        message="Select models:",
        choices=choices,
        multiselect=True,
        max_height="70%",
    ).execute()

    if not picked:
        return {}

    result: dict[str, list[str]] = {}
    for slug, model_id in picked:
        if slug not in result:
            result[slug] = []
        result[slug].append(model_id)
    return result


def _step_test_models(
    selected_models: dict[str, list[str]],
    api_keys: dict[str, str],
    base_urls: dict[str, str],
) -> None:
    """Optional: test a model before continuing."""
    from InquirerPy import inquirer

    # Flatten selected models for the test picker
    flat = []
    for slug, models in selected_models.items():
        for m in models:
            flat.append({"name": f"{slug}/{m}", "value": (slug, m)})

    if not flat:
        return

    want_test = inquirer.confirm(
        "Test a model before continuing?", default=False
    ).execute()

    if not want_test:
        return

    while True:
        pick = inquirer.select(
            message="Which model to test?",
            choices=flat + [{"name": "Done testing", "value": None}],
        ).execute()

        if pick is None:
            break

        slug, model_id = pick
        click.echo(f"  Testing {slug}/{model_id}...")

        import asyncio
        try:
            result = asyncio.run(_test_model_request(
                base_urls.get(slug, ""),
                api_keys.get(slug, ""),
                model_id,
                slug,
            ))
            click.echo(f"  Response: {result}")
        except Exception as e:
            click.echo(f"  Error: {e}")
        click.echo()


async def _test_model_request(
    base_url: str, api_key: str, model_id: str, provider_slug: str
) -> str:
    """Send a 128-token test request to a model."""
    import httpx as _httpx

    async with _httpx.AsyncClient(timeout=30) as client:
        if provider_slug == "anthropic":
            resp = await client.post(
                f"{base_url}/messages",
                json={
                    "model": model_id,
                    "messages": [{"role": "user", "content": "Say hello in one sentence."}],
                    "max_tokens": 128,
                },
                headers={"x-api-key": api_key, "anthropic-version": "2023-06-01"},
            )
            data = resp.json()
            if "content" in data and data["content"]:
                return data["content"][0].get("text", "")
            return f"Error: {data.get('error', {}).get('message', str(data)[:100])}"

        elif provider_slug == "gemini":
            clean = model_id.split("/", 1)[1] if "/" in model_id else model_id
            resp = await client.post(
                f"{base_url}/models/{clean}:generateContent?key={api_key}",
                json={
                    "contents": [{"role": "user", "parts": [{"text": "Say hello in one sentence."}]}],
                    "generationConfig": {"maxOutputTokens": 128},
                },
            )
            data = resp.json()
            if "candidates" in data and data["candidates"]:
                parts = data["candidates"][0].get("content", {}).get("parts", [])
                return "".join(p.get("text", "") for p in parts)
            return f"Error: {data.get('error', {}).get('message', str(data)[:100])}"

        else:
            resp = await client.post(
                f"{base_url}/chat/completions",
                json={
                    "model": model_id,
                    "messages": [{"role": "user", "content": "Say hello in one sentence."}],
                    "max_tokens": 128,
                },
                headers={"Authorization": f"Bearer {api_key}"},
            )
            data = resp.json()
            if "choices" in data and data["choices"]:
                return data["choices"][0]["message"]["content"]
            return f"Error: {data.get('error', {}).get('message', str(data)[:100])}"


# ── Main command ────────────────────────────────────────────────────────────

@click.command()
@click.option("--force", is_flag=True, help="Overwrite existing config.yaml and users.yaml")
def init_command(force: bool):
    """Initialize llm-pico configuration."""
    config_path = Path("config.yaml")
    users_path = Path("users.yaml")

    if config_path.exists() and not force:
        click.echo("config.yaml already exists. Use --force to overwrite.")
        raise SystemExit(1)

    click.echo()
    click.echo("  llm-pico v0.1.0 — setup wizard")
    click.echo()

    # Step 1: Select providers
    providers = _step_select_providers()
    if not providers:
        click.echo("  No providers selected. Exiting.")
        raise SystemExit(0)

    # Step 2: API keys
    api_keys, base_urls, account_ids = _step_api_keys(providers)

    # Step 3: Fetch models from all providers
    click.echo()
    click.echo("  Fetching models...")
    all_models = _step_fetch_all_models(providers, api_keys, base_urls)

    # Step 4: Custom provider details
    selected_custom = [p for p in providers if p.startswith("custom_")]
    custom_providers, custom_models, custom_base_urls = (
        _step_custom_provider_details(selected_custom) if selected_custom else ([], {}, {})
    )

    # Merge custom models and base_urls into the unified pool
    for slug, models in custom_models.items():
        all_models[slug] = models
    base_urls.update(custom_base_urls)

    # Collect all API keys for testing (custom providers too)
    for cp in custom_providers:
        if cp["api_key"]:
            api_keys[cp["name"]] = cp["api_key"]

    # Step 5: Model selection (fuzzy multi-select)
    click.echo()
    selected_models = _step_select_models(all_models, api_keys, base_urls)

    if not selected_models:
        click.echo("  No models selected. Exiting.")
        raise SystemExit(0)

    # Step 5b: Optional test
    _step_test_models(selected_models, api_keys, base_urls)

    # Step 6: Capability assignment
    model_caps = _step_assign_capabilities(selected_models, custom_providers)

    # Collect all model names for user allowlist step
    all_model_names: list[str] = []
    for models in selected_models.values():
        all_model_names.extend(models)
    for cp in custom_providers:
        all_model_names.extend(cp.get("models", []))

    # Step 7: Master key
    master_key = _generate_master_key()

    # Step 8: First user (with model allowlist)
    user_name, limits, allowlist = _step_first_user(all_model_names)

    # Step 9: Docker option
    want_docker = _step_docker()

    # ── Generate files ──────────────────────────────────────────────────────

    click.echo()

    config_dict = _generate_config(providers, api_keys, custom_providers, selected_models, model_caps, master_key, base_urls)
    users_dict, demo_user_key = _generate_users(user_name, limits, allowlist)
    keys_dict = _generate_keys_file(providers, api_keys, custom_providers)

    # Write config.yaml
    if config_path.exists():
        backup = config_path.with_suffix(".yaml.bak")
        shutil.copy2(config_path, backup)
        click.echo(f"  Backed up existing config.yaml to {backup}")

    with open(config_path, "w") as f:
        yaml.dump(config_dict, f, default_flow_style=False, sort_keys=False, allow_unicode=True)
    click.echo("  Generated config.yaml")

    # Write users.yaml
    with open(users_path, "w") as f:
        yaml.dump(users_dict, f, default_flow_style=False, sort_keys=False, allow_unicode=True)
    click.echo("  Generated users.yaml")

    # Write keys.yaml (for KEYS/XXX resolution)
    keys_path = Path("keys.yaml")
    if not keys_path.exists():
        with open(keys_path, "w") as f:
            yaml.dump(keys_dict, f, default_flow_style=False, sort_keys=False, allow_unicode=True)
        click.echo("  Generated keys.yaml")
    else:
        click.echo("  keys.yaml already exists, skipping")

    # Write .env (for ENV/XXX and Cloudflare ACCOUNT_ID)
    env_content = _generate_env_file(providers, api_keys, account_ids)
    env_path = Path(".env")
    if not env_path.exists():
        with open(env_path, "w") as f:
            f.write(env_content)
        click.echo("  Generated .env")
    else:
        click.echo("  .env already exists, skipping")

    # Load .env into os.environ for ENV/XXX resolution
    for line in env_content.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" in line:
            k, v = line.split("=", 1)
            os.environ[k.strip()] = v.strip().strip('"').strip("'")

    # Validate
    try:
        load_config(str(config_path))
        click.echo("  config.yaml validated")
    except Exception as e:
        click.echo(f"  ERROR: config validation failed: {e}")
        config_path.unlink(missing_ok=True)
        users_path.unlink(missing_ok=True)
        raise SystemExit(1)

    # Docker
    if want_docker:
        docker_path = Path("docker-compose.yml")
        docker_dict = _generate_docker_compose()
        with open(docker_path, "w") as f:
            yaml.dump(docker_dict, f, default_flow_style=False, sort_keys=False)
        click.echo("  Generated docker-compose.yml")

    # Summary
    model_count = sum(len(m) for m in selected_models.values())
    for cp in custom_providers:
        model_count += len(cp.get("models", []))
    click.echo()
    click.echo(f"  Done! ({len(providers)} providers, {model_count} models, 1 user)")
    click.echo()

    # Print keys info
    click.echo("  API keys stored in keys.yaml (add backup keys for rotation):")
    for slug in providers:
        if slug.startswith("custom_"):
            continue
        info = BUILTIN_PROVIDERS[slug]
        click.echo(f"    {info['env_key']}:")
        click.echo(f"      - \"{api_keys.get(slug, 'YOUR_KEY')}\"")
    click.echo()

    # Print env vars
    env_lines = []
    for slug in providers:
        if slug.startswith("custom_"):
            continue
        info = BUILTIN_PROVIDERS[slug]
        env_lines.append(f'  export {info["env_key"]}="..."')
    if env_lines:
        click.echo("  Or set env vars for single-key mode (ENV/XXX):")
        for line in env_lines:
            click.echo(line)
        click.echo()

    click.echo("  Start the server:")
    click.echo("    llm-pico")
    click.echo()

    if want_docker:
        click.echo("  Or with Docker:")
        click.echo("    docker compose up -d")
        click.echo()
