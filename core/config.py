from __future__ import annotations

import logging
import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Literal

import yaml

_ENV_VAR_RE = re.compile(r"\$\{([^}:]+)(?::(-?[^}]*))?\}")
_KEY_REF_RE = re.compile(r"^(KEYS|ENV)/(.+)$")

_log = logging.getLogger("llm-pico.config")


def _resolve_env_vars(raw: dict) -> dict:
    """Walk a parsed-YAML dict and resolve ${VAR_NAME} or ${VAR_NAME:-default} strings.

    Recursively traverses nested dicts and lists. Any string value matching
    the ${VAR} or ${VAR:-default} pattern is replaced with the environment
    variable value. Raises ValueError if a variable is not set and has no default.
    """

    def _walk(value):
        if isinstance(value, str):
            match = _ENV_VAR_RE.fullmatch(value)
            if not match:
                return value
            var_name = match.group(1)
            default = match.group(2)
            env_val = os.environ.get(var_name)
            if env_val is not None:
                return env_val
            if default is not None:
                return default
            raise ValueError(
                f"Environment variable ${var_name} is not set "
                f"and no default value was provided"
            )
        elif isinstance(value, dict):
            return {k: _walk(v) for k, v in value.items()}
        elif isinstance(value, list):
            return [_walk(item) for item in value]
        return value

    return _walk(raw)


def _resolve_api_keys(raw: dict, keys_yaml_path: str = "keys.yaml") -> dict:
    """Resolve KEYS/XXX and ENV/XXX references in api_key fields.

    KEYS/XXX → loads keys.yaml, returns list[str] (multiple backup keys)
    ENV/XXX  → os.environ.get(XXX), returns str (single key)
    """
    keys_data: dict[str, list[str]] | None = None

    def _load_keys_yaml():
        nonlocal keys_data
        if keys_data is not None:
            return
        keys_path = Path(keys_yaml_path)
        if not keys_path.exists():
            keys_data = {}
            return
        with open(keys_path) as f:
            loaded = yaml.safe_load(f)
        if not isinstance(loaded, dict):
            keys_data = {}
            return
        # Normalize: ensure all values are lists of strings
        keys_data = {}
        for k, v in loaded.items():
            if isinstance(v, list):
                keys_data[k] = [str(item) for item in v]
            elif isinstance(v, str):
                keys_data[k] = [v]
            else:
                keys_data[k] = []

    def _walk(value):
        if isinstance(value, str):
            match = _KEY_REF_RE.fullmatch(value)
            if not match:
                return value
            ref_type = match.group(1)
            ref_name = match.group(2)

            if ref_type == "KEYS":
                _load_keys_yaml()
                key_list = (keys_data or {}).get(ref_name)
                if key_list:
                    return key_list  # Returns list[str] for rotation
                raise ValueError(
                    f"Key '{ref_name}' not found in {keys_yaml_path}. "
                    f"Add it with: {ref_name}:\n  - \"sk-your-key\""
                )
            elif ref_type == "ENV":
                env_val = os.environ.get(ref_name)
                if env_val is not None:
                    return env_val  # Returns str (single key)
                raise ValueError(
                    f"Environment variable {ref_name} is not set"
                )
        elif isinstance(value, dict):
            return {k: _walk(v) for k, v in value.items()}
        elif isinstance(value, list):
            return [_walk(item) for item in value]
        return value

    return _walk(raw)


@dataclass
class CircuitBreakerSettings:
    enabled: bool = True
    failure_threshold: int = 3
    recovery_timeout: int = 30


@dataclass
class RouterSettings:
    num_retries: int = 2
    cooldown_time: int = 45
    circuit_breaker: CircuitBreakerSettings = field(default_factory=CircuitBreakerSettings)


@dataclass
class GeneralSettings:
    master_key: str = ""
    db_path: str | None = None
    usage_log_retention_days: int = 30
    admin_log_retention_days: int = 90


@dataclass
class ModelParams:
    model: str = ""
    api_key: str | list[str] | None = None
    api_base: str | None = None


@dataclass
class ModelEntry:
    model_name: str
    model_params: ModelParams
    rpm: int | None = None
    rpd: int | None = None
    tpm: int | None = None
    tpd: int | None = None
    ash: int | None = None
    asd: int | None = None
    images: bool = False
    embeddings: bool = False
    stt: bool = False
    tts: bool = False
    failover_model: str | None = None
    can_cache: bool = False
    cost_per_1m_input: float | None = None
    cost_per_1m_output: float | None = None


@dataclass
class UserKey:
    key: str
    label: str | None = None
    models: list[str] | None = None
    rpm: int | None = None
    rpd: int | None = None
    tpm: int | None = None
    tpd: int | None = None


@dataclass
class Config:
    general_settings: GeneralSettings = field(default_factory=GeneralSettings)
    router_settings: RouterSettings = field(default_factory=RouterSettings)
    model_list: list[ModelEntry] = field(default_factory=list)


def load_config(path: str) -> Config:
    path_obj = Path(path)
    if not path_obj.exists():
        raise FileNotFoundError(f"config file not found: {path}")

    with open(path_obj) as f:
        raw = yaml.safe_load(f)

    raw = _resolve_api_keys(raw)
    raw = _resolve_env_vars(raw)

    if not isinstance(raw, dict):
        raise ValueError("config must be a YAML dictionary")

    cfg = Config()

    # general_settings
    gs = raw.get("general_settings") or {}
    cfg.general_settings = GeneralSettings(
        master_key=gs.get("master_key", ""),
        db_path=gs.get("db_path"),
        usage_log_retention_days=gs.get("usage_log_retention_days", 30),
        admin_log_retention_days=gs.get("admin_log_retention_days", 90),
    )

    # router_settings
    rs = raw.get("router_settings") or {}
    cb = rs.get("circuit_breaker") or {}
    cfg.router_settings = RouterSettings(
        num_retries=rs.get("num_retries", 2),
        cooldown_time=rs.get("cooldown_time", 45),
        circuit_breaker=CircuitBreakerSettings(
            enabled=cb.get("enabled", True),
            failure_threshold=cb.get("failure_threshold", 3),
            recovery_timeout=cb.get("recovery_timeout", 30),
        ),
    )

    # model_list
    for entry in raw.get("model_list") or []:
        lp = entry.get("model_params") or {}
        model_entry = ModelEntry(
            model_name=entry["model_name"],
            model_params=ModelParams(
                model=lp.get("model", ""),
                api_key=lp.get("api_key"),
                api_base=lp.get("api_base"),
            ),
            rpm=entry.get("rpm"),
            rpd=entry.get("rpd"),
            tpm=entry.get("tpm"),
            tpd=entry.get("tpd"),
            ash=entry.get("ash"),
            asd=entry.get("asd"),
            images=entry.get("images", False),
            embeddings=entry.get("embeddings", False),
            stt=entry.get("stt", False),
            tts=entry.get("tts", False),
            failover_model=entry.get("failover_model"),
            can_cache=entry.get("can_cache", False),
            cost_per_1m_input=entry.get("cost_per_1m_input"),
            cost_per_1m_output=entry.get("cost_per_1m_output"),
        )
        cfg.model_list.append(model_entry)

    if not cfg.model_list:
        raise ValueError("config must have at least one model in model_list")

    if not cfg.general_settings.master_key:
        raise ValueError("general_settings.master_key is required")

    for entry in cfg.model_list:
        base = entry.model_params.api_base or ""
        if "UNSET" in base:
            raise ValueError(
                f"model '{entry.model_name}' has UNSET placeholder in api_base: {base}. "
                f"Set api_base to the actual Cloudflare Workers AI URL or set CLOUDFLARE_ACCOUNT_ID."
            )

        # Warn about STT/TTS on providers that don't support them
        provider_slug = entry.model_params.model.split("/", 1)[0] if "/" in entry.model_params.model else ""
        unsupported_stt_tts = {"anthropic", "gemini"}
        if provider_slug in unsupported_stt_tts:
            if entry.stt:
                _log.warning(
                    "model '%s' has stt=true under provider '%s', which does not support STT. "
                    "This will fail at runtime.",
                    entry.model_name, provider_slug,
                )
            if entry.tts:
                _log.warning(
                    "model '%s' has tts=true under provider '%s', which does not support TTS. "
                    "This will fail at runtime.",
                    entry.model_name, provider_slug,
                )

    return cfg


def load_users(path: str) -> list[UserKey]:
    path_obj = Path(path)
    if not path_obj.exists():
        return []

    with open(path_obj) as f:
        raw = yaml.safe_load(f)

    raw = _resolve_env_vars(raw)

    if not isinstance(raw, dict):
        return []

    users = []
    for entry in raw.get("users") or []:
        raw_key = entry.get("key", "")
        if not raw_key:
            _log.warning("skipping user entry with empty key")
            continue
        users.append(UserKey(
            key=raw_key,
            label=entry.get("label"),
            models=entry.get("models"),
            rpm=entry.get("rpm"),
            rpd=entry.get("rpd"),
            tpm=entry.get("tpm"),
            tpd=entry.get("tpd"),
        ))
    return users
