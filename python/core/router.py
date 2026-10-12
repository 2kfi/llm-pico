from __future__ import annotations

import logging
import random
import statistics
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
    next_key_index: int = 0
    health_score: float = 1.0
    total_requests: int = 0
    error_count: int = 0
    cost_per_1k: float = 0.0
    latency_p50: float = 0.0
    latency_p95: float = 0.0
    latency_samples: list[float] = field(default_factory=list)

    def update_health(self) -> None:
        # ponytail: simple composite score, swap factors for better signals as needed
        total = max(1, self.total_requests)
        availability = 1.0 - (self.error_count / total)
        error_factor = 1.0 / max(0.01, self.error_count / total + 0.01)
        self.health_score = min(1.0, availability * min(1.0, error_factor))

    def record_latency(self, ms: float) -> None:
        self.latency_samples.append(ms)
        # ponytail: keep last 100, no need for a ring buffer
        if len(self.latency_samples) > 100:
            self.latency_samples = self.latency_samples[-100:]
        s = sorted(self.latency_samples)
        self.latency_p50 = statistics.median(s)
        self.latency_p95 = s[int(len(s) * 0.95)] if len(s) >= 2 else s[0]


class Router:
    def __init__(self, config: Config) -> None:
        self._model_map: dict[str, list[ProviderGroup]] = {}
        self._model_entries: dict[str, ModelEntry] = {}
        self._settings = config.router_settings
        self._model_failures: dict[str, int] = {}  # model_name -> consecutive failures
        self._model_cooldown: dict[str, float] = {}  # model_name -> cooldown expiry timestamp
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

            # Handle both str and list[str] api_key (KEYS/ returns list)
            api_key = entry.model_params.api_key
            if isinstance(api_key, list):
                for k in api_key:
                    matching_group.keys.append(KeyState(api_key=k))
            else:
                matching_group.keys.append(KeyState(api_key=api_key or ""))

        _log.info("router indexed %d model names", len(self._model_map))

    def _extract_provider(self, model_string: str) -> str:
        if "/" in model_string:
            return model_string.split("/")[0]
        return "openai"

    def get_model_names(self) -> list[str]:
        return list(self._model_map.keys())

    def _record_model_failure(self, model_name: str) -> None:
        """Track consecutive failures per model. After 2 failures, cooldown for 60s."""
        self._model_failures[model_name] = self._model_failures.get(model_name, 0) + 1
        if self._model_failures[model_name] >= 2:
            self._model_cooldown[model_name] = time.time() + 60
            _log.warning("model %s cooled down for 60s after %d failures", model_name, self._model_failures[model_name])

    def _is_model_cooling_down(self, model_name: str) -> bool:
        """Check if a model is in cooldown period."""
        expiry = self._model_cooldown.get(model_name, 0)
        if expiry and time.time() < expiry:
            return True
        if expiry and time.time() >= expiry:
            # Cooldown expired — reset failure count
            self._model_failures.pop(model_name, None)
            self._model_cooldown.pop(model_name, None)
        return False

    def resolve(self, model_name: str, max_cost: float | None = None) -> tuple[ProviderGroup, KeyState, ModelEntry] | None:
        groups = self._model_map.get(model_name)
        if not groups:
            return None

        # Skip models in cooldown (failed 2x, pushed out for 60s)
        if self._is_model_cooling_down(model_name):
            return None

        now = time.monotonic()
        earliest_recovery = float("inf")

        # ponytail: true round-robin per group, pick best group by health
        group_picks: list[tuple[ProviderGroup, KeyState]] = []
        for group in groups:
            if not group.circuit_breaker.is_request_allowed():
                continue
            if max_cost is not None and group.cost_per_1k > max_cost:
                continue
            eligible = [k for k in group.keys if k.cooldown_until < now]
            if not eligible:
                # Track earliest recovery for 429 response
                for k in group.keys:
                    earliest_recovery = min(earliest_recovery, k.cooldown_until)
                continue
            # Round-robin within this group's keys
            pick = eligible[group.next_key_index % len(eligible)]
            group.next_key_index = (group.next_key_index + 1) % len(eligible)
            group_picks.append((group, pick))

        if group_picks:
            # Weighted random pick proportional to health_score
            weights = [max(0.01, g.health_score) for g, _ in group_picks]
            chosen_group, chosen_key = random.choices(group_picks, weights=weights, k=1)[0]
            entry = self._model_entries.get(model_name)
            if entry is None:
                return None
            return chosen_group, chosen_key, entry

        # All keys exhausted — raise 429 with Retry-After
        if groups and earliest_recovery < float("inf"):
            retry_after = max(1, int(earliest_recovery - now))
            from fastapi import HTTPException
            raise HTTPException(
                status_code=429,
                detail={
                    "error": {
                        "message": f"All keys for model '{model_name}' are rate-limited. Retry after {retry_after}s.",
                        "type": "rate_limit_exceeded",
                        "code": 429,
                    }
                },
                headers={"Retry-After": str(retry_after)},
            )

        return None

    def record_failure(self, provider_group: ProviderGroup, key_state: KeyState, status_code: int) -> None:
        now = time.monotonic()
        provider_group.total_requests += 1
        provider_group.error_count += 1

        if status_code == 429:
            # Progressive cooldown: 10s for first 3 fails, then 30s
            cooldown = 30 if key_state.fails >= 3 else 10
            key_state.cooldown_until = now + cooldown
            key_state.fails += 1
            _log.debug("key cooled down for %ds (429, fail #%d)", cooldown, key_state.fails)

        elif status_code in (401, 403, 402):
            # 401/403 = invalid key, 402 = insufficient funds — mark invalid for 1 year
            key_state.cooldown_until = now + 86400 * 365
            _log.warning("key marked as invalid (status=%d)", status_code)

        elif 500 <= status_code < 600:
            provider_group.circuit_breaker.record_failure()

        provider_group.update_health()

    async def resolve_with_fallbacks(self, model_name: str) -> tuple[ProviderGroup, KeyState, ModelEntry, str] | None:
        """Try resolve, then fallbacks, then failover_model, then alias resolution."""
        result = self.resolve(model_name)
        if result:
            return (*result, model_name)

        entry = self._model_entries.get(model_name)

        # Try configured fallback chain
        if entry:
            fallbacks = entry.fallbacks or []
            # Also include legacy failover_model as lowest-priority fallback
            if entry.failover_model:
                fallbacks = fallbacks + [{"model": entry.failover_model, "priority": 99}]
            for fb in sorted(fallbacks, key=lambda f: f.get("priority", 0)):
                fb_model = fb.get("model", "")
                result = self.resolve(fb_model)
                if result:
                    _log.info("fallback %s → %s", model_name, fb_model)
                    return (*result, fb_model)

        # Try alias resolution
        from core.aliases import resolve_alias
        resolved = await resolve_alias(model_name, self.get_model_names())
        if resolved and resolved != model_name:
            result = self.resolve(resolved)
            if result:
                _log.info("alias %s → %s", model_name, resolved)
                return (*result, resolved)

        return None

    def resolve_chain(self, model_chain: list[str]) -> tuple[ProviderGroup, KeyState, ModelEntry, str, int, list[str]] | None:
        """Walk an ordered model chain, return first success.

        Returns: (group, key, entry, resolved_model_name, hops, models_tried) or None
        """
        tried: list[str] = []
        for model_name in model_chain:
            tried.append(model_name)
            result = self.resolve(model_name)
            if result is not None:
                group, key, entry = result
                return group, key, entry, model_name, len(tried) - 1, tried
        return None

    def record_success(self, provider_group: ProviderGroup) -> None:
        provider_group.total_requests += 1
        provider_group.update_health()
        provider_group.circuit_breaker.record_success()
        for key in provider_group.keys:
            key.fails = 0
