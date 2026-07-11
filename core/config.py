from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Literal

import yaml

_ENV_VAR_RE = re.compile(r"\$\{([^}:]+)(?::(-?[^}]*))?\}")


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


@dataclass
class CircuitBreakerSettings:
    enabled: bool = True
    failure_threshold: int = 3
    recovery_timeout: int = 30


@dataclass
class RouterSettings:
    routing_strategy: str = "simple-shuffle"
    num_retries: int = 2
    cooldown_time: int = 45
    allowed_fails: int = 1
    circuit_breaker: CircuitBreakerSettings = field(default_factory=CircuitBreakerSettings)


@dataclass
class GeneralSettings:
    master_key: str = ""
    db_path: str | None = None
    usage_log_retention_days: int = 30
    admin_log_retention_days: int = 90


@dataclass
class LitellmParams:
    model: str = ""
    api_key: str | None = None
    api_base: str | None = None


@dataclass
class ModelEntry:
    model_name: str
    litellm_params: LitellmParams
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
        routing_strategy=rs.get("routing_strategy", "simple-shuffle"),
        num_retries=rs.get("num_retries", 2),
        cooldown_time=rs.get("cooldown_time", 45),
        allowed_fails=rs.get("allowed_fails", 1),
        circuit_breaker=CircuitBreakerSettings(
            enabled=cb.get("enabled", True),
            failure_threshold=cb.get("failure_threshold", 3),
            recovery_timeout=cb.get("recovery_timeout", 30),
        ),
    )

    # model_list
    for entry in raw.get("model_list") or []:
        lp = entry.get("litellm_params") or {}
        model_entry = ModelEntry(
            model_name=entry["model_name"],
            litellm_params=LitellmParams(
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
        users.append(UserKey(
            key=entry.get("key", ""),
            label=entry.get("label"),
            models=entry.get("models"),
            rpm=entry.get("rpm"),
            rpd=entry.get("rpd"),
            tpm=entry.get("tpm"),
            tpd=entry.get("tpd"),
        ))
    return users
