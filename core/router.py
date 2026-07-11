from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field
from typing import Any, Literal

from core.config import Config, ModelEntry, RouterSettings

_log = logging.getLogger("llm-pico.router")


@dataclass
class CircuitBreaker:
    state: Literal["CLOSED", "OPEN", "HALF_OPEN"] = "CLOSED"
    error_count: int = 0
    opened_at: float = 0.0
    failure_threshold: int = 3
    recovery_timeout: float = 30.0

    def record_failure(self) -> None:
        self.error_count += 1
        if self.error_count >= self.failure_threshold:
            self.state = "OPEN"
            self.opened_at = time.monotonic()
            _log.warning("circuit breaker OPEN for provider (threshold=%d)", self.failure_threshold)

    def record_success(self) -> None:
        if self.state == "HALF_OPEN":
            _log.info("circuit breaker CLOSED after successful probe")
        self.state = "CLOSED"
        self.error_count = 0
        self.opened_at = 0.0

    def is_request_allowed(self) -> bool:
        if self.state == "CLOSED":
            return True
        if self.state == "OPEN":
            if time.monotonic() - self.opened_at >= self.recovery_timeout:
                self.state = "HALF_OPEN"
                _log.info("circuit breaker HALF_OPEN, probing")
                return True
            return False
        return True


@dataclass
class KeyState:
    api_key: str
    cooldown_until: float = 0.0
    fails: int = 0


@dataclass
class ProviderGroup:
    provider_slug: str
    keys: list[KeyState] = field(default_factory=list)
    circuit_breaker: CircuitBreaker = field(default_factory=CircuitBreaker)
    api_base: str | None = None


class Router:
    def __init__(self, config: Config) -> None:
        self._model_map: dict[str, list[ProviderGroup]] = {}
        self._model_entries: dict[str, ModelEntry] = {}
        self._settings = config.router_settings
        self._build_index(config)

    def _build_index(self, config: Config) -> None:
        for entry in config.model_list:
            model_name = entry.model_name
            self._model_entries[model_name] = entry

            provider_slug = self._extract_provider(entry.model_params.model)
            if model_name not in self._model_map:
                self._model_map[model_name] = []

            groups = self._model_map[model_name]
            matching_group = None
            for g in groups:
                if g.provider_slug == provider_slug and g.api_base == entry.model_params.api_base:
                    matching_group = g
                    break

            if matching_group is None:
                matching_group = ProviderGroup(
                    provider_slug=provider_slug,
                    api_base=entry.model_params.api_base,
                    circuit_breaker=CircuitBreaker(
                        failure_threshold=self._settings.circuit_breaker.failure_threshold,
                        recovery_timeout=self._settings.circuit_breaker.recovery_timeout,
                    ),
                )
                groups.append(matching_group)

            matching_group.keys.append(KeyState(api_key=entry.model_params.api_key or ""))

        _log.info("router indexed %d model names", len(self._model_map))

    def _extract_provider(self, model_string: str) -> str:
        if "/" in model_string:
            return model_string.split("/")[0]
        return "openai"

    def get_model_names(self) -> list[str]:
        return list(self._model_map.keys())

    def resolve(self, model_name: str) -> tuple[ProviderGroup, KeyState, ModelEntry] | None:
        groups = self._model_map.get(model_name)
        if not groups:
            return None

        strategy = self._settings.routing_strategy
        now = time.monotonic()

        for group in groups:
            if not group.circuit_breaker.is_request_allowed():
                continue

            active_keys = [k for k in group.keys if k.cooldown_until < now]
            if not active_keys:
                continue

            if strategy == "simple-shuffle":
                key = active_keys[hash(str(now) + group.provider_slug) % len(active_keys)]
            else:
                key = active_keys[0]

            entry = self._model_entries.get(model_name)
            if entry is None:
                return None

            return group, key, entry

        return None

    def record_failure(self, provider_group: ProviderGroup, key_state: KeyState, status_code: int) -> None:
        now = time.monotonic()

        if status_code == 429:
            key_state.cooldown_until = now + self._settings.cooldown_time
            key_state.fails += 1
            _log.debug("key cooled down for %.0fs (429)", self._settings.cooldown_time)

        elif status_code in (401, 403):
            key_state.cooldown_until = now + 86400 * 365
            _log.warning("key marked as invalid (status=%d)", status_code)

        elif 500 <= status_code < 600:
            provider_group.circuit_breaker.record_failure()

    def record_success(self, provider_group: ProviderGroup) -> None:
        provider_group.circuit_breaker.record_success()
        for key in provider_group.keys:
            key.fails = 0
