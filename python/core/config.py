from __future__ import annotations

import json
import logging
import time
from dataclasses import dataclass, field
from typing import Any

from core.db import get_db

_log = logging.getLogger("llm-pico.config")


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
    hmac_enabled: bool = False
    hmac_secret: str = ""


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
    fallbacks: list[dict[str, Any]] | None = None  # ponytail: ordered fallback chain
    can_cache: bool = False
    cost_per_1m_input: float | None = None
    cost_per_1m_output: float | None = None
    db_model_id: int | None = None


@dataclass
class Config:
    general_settings: GeneralSettings = field(default_factory=GeneralSettings)
    router_settings: RouterSettings = field(default_factory=RouterSettings)
    model_list: list[ModelEntry] = field(default_factory=list)
    degradation_mode: str = "normal"


async def _get_settings() -> dict[str, str | None]:
    async with get_db() as db:
        cursor = await db.execute("SELECT key, value FROM settings")
        rows = await cursor.fetchall()
    return {row["key"]: row["value"] for row in rows}


async def _set_setting(key: str, value: str) -> None:
    async with get_db() as db:
        await db.execute(
            "INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)",
            (key, value),
        )
        await db.commit()


async def load_config_from_db() -> Config:
    s = await _get_settings()

    def _int(key: str, default: int) -> int:
        v = s.get(key)
        return int(v) if v is not None else default

    def _bool(key: str, default: bool) -> bool:
        v = s.get(key)
        if v is None:
            return default
        return v not in ("0", "false", "")

    cfg = Config()
    cfg.general_settings = GeneralSettings(
        master_key=s.get("master_key") or "",
        db_path=s.get("db_path"),
        usage_log_retention_days=_int("usage_log_retention_days", 30),
        admin_log_retention_days=_int("admin_log_retention_days", 90),
        hmac_enabled=_bool("hmac_enabled", False),
        hmac_secret=s.get("hmac_secret") or "",
    )
    cfg.router_settings = RouterSettings(
        num_retries=_int("num_retries", 2),
        cooldown_time=_int("cooldown_time", 45),
        circuit_breaker=CircuitBreakerSettings(
            enabled=_bool("circuit_breaker_enabled", True),
            failure_threshold=_int("circuit_breaker_failure_threshold", 3),
            recovery_timeout=_int("circuit_breaker_recovery_timeout", 30),
        ),
    )
    cfg.degradation_mode = s.get("degradation_mode") or "normal"

    async with get_db() as db:
        cursor = await db.execute(
            """SELECT m.id, m.model_name, m.model, m.api_base,
                      m.images, m.embeddings, m.stt, m.tts,
                      m.failover_model, m.can_cache,
                      m.cost_per_1m_input, m.cost_per_1m_output,
                      m.rpm, m.rpd, m.tpm, m.tpd, m.ash, m.asd,
                      GROUP_CONCAT(pk.api_key) as api_keys
               FROM models m
               LEFT JOIN provider_keys pk ON pk.model_id = m.id AND pk.is_active = 1
               WHERE m.is_active = 1
               GROUP BY m.id
               ORDER BY m.id"""
        )
        rows = await cursor.fetchall()

    for row in rows:
        raw_keys = row["api_keys"]
        if raw_keys:
            keys = raw_keys.split(",")
            api_key = keys if len(keys) > 1 else keys[0]
        else:
            api_key = None

        cfg.model_list.append(ModelEntry(
            model_name=row["model_name"],
            model_params=ModelParams(
                model=row["model"],
                api_key=api_key,
                api_base=row["api_base"],
            ),
            rpm=row["rpm"],
            rpd=row["rpd"],
            tpm=row["tpm"],
            tpd=row["tpd"],
            ash=row["ash"],
            asd=row["asd"],
            images=bool(row["images"]),
            embeddings=bool(row["embeddings"]),
            stt=bool(row["stt"]),
            tts=bool(row["tts"]),
            failover_model=row["failover_model"],
            can_cache=bool(row["can_cache"]),
            cost_per_1m_input=row["cost_per_1m_input"],
            cost_per_1m_output=row["cost_per_1m_output"],
            db_model_id=row["id"],
        ))

    if not cfg.model_list:
        _log.warning("no models configured in database")

    return cfg


async def save_settings(settings: dict[str, Any]) -> None:
    for key, value in settings.items():
        if value is None:
            continue
        await _set_setting(key, str(value))


async def save_model(model_id: int | None, data: dict[str, Any]) -> int:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        if model_id is not None:
            await db.execute(
                """UPDATE models SET
                   model_name=?, model=?, api_base=?, images=?, embeddings=?,
                   stt=?, tts=?, failover_model=?, can_cache=?,
                   cost_per_1m_input=?, cost_per_1m_output=?,
                   rpm=?, rpd=?, tpm=?, tpd=?, ash=?, asd=?, is_active=1
                   WHERE id=?""",
                (
                    data["model_name"], data["model"], data.get("api_base"),
                    int(data.get("images", False)), int(data.get("embeddings", False)),
                    int(data.get("stt", False)), int(data.get("tts", False)),
                    data.get("failover_model"), int(data.get("can_cache", False)),
                    data.get("cost_per_1m_input"), data.get("cost_per_1m_output"),
                    data.get("rpm"), data.get("rpd"), data.get("tpm"), data.get("tpd"),
                    data.get("ash"), data.get("asd"),
                    model_id,
                ),
            )
            await db.commit()
            return model_id
        else:
            cursor = await db.execute(
                """INSERT INTO models
                   (model_name, model, api_base, images, embeddings, stt, tts,
                    failover_model, can_cache, cost_per_1m_input, cost_per_1m_output,
                    rpm, rpd, tpm, tpd, ash, asd, is_active, created_at)
                   VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)""",
                (
                    data["model_name"], data["model"], data.get("api_base"),
                    int(data.get("images", False)), int(data.get("embeddings", False)),
                    int(data.get("stt", False)), int(data.get("tts", False)),
                    data.get("failover_model"), int(data.get("can_cache", False)),
                    data.get("cost_per_1m_input"), data.get("cost_per_1m_output"),
                    data.get("rpm"), data.get("rpd"), data.get("tpm"), data.get("tpd"),
                    data.get("ash"), data.get("asd"),
                    now,
                ),
            )
            await db.commit()
            return cursor.lastrowid


async def delete_model(model_id: int) -> bool:
    async with get_db() as db:
        cursor = await db.execute("UPDATE models SET is_active = 0 WHERE id = ?", (model_id,))
        await db.commit()
        return cursor.rowcount > 0


async def save_provider_key(model_id: int, api_key: str, priority: int = 0) -> int:
    async with get_db() as db:
        cursor = await db.execute(
            "INSERT INTO provider_keys (model_id, api_key, priority, is_active) VALUES (?,?,?,1)",
            (model_id, api_key, priority),
        )
        await db.commit()
        return cursor.lastrowid


async def delete_provider_key(key_id: int) -> bool:
    async with get_db() as db:
        cursor = await db.execute("UPDATE provider_keys SET is_active = 0 WHERE id = ?", (key_id,))
        await db.commit()
        return cursor.rowcount > 0


async def get_provider_keys(model_id: int) -> list[dict[str, Any]]:
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT id, api_key, priority, is_active FROM provider_keys WHERE model_id = ? ORDER BY priority, id",
            (model_id,),
        )
        rows = await cursor.fetchall()
    return [dict(r) for r in rows]


async def reload_config(app_state) -> bool:
    """Hot-reload config from DB and swap router. Returns True on success."""
    try:
        new_config = await load_config_from_db()
        from core.router import Router
        new_router = Router(new_config)
        app_state.router = new_router
        app_state.config = new_config
        _log.info("hot-reloaded config: %d models", len(new_config.model_list))
        return True
    except Exception:
        _log.exception("hot-reload failed, falling back to os.execve")
        return False



