from __future__ import annotations

import time

import pytest

from core.router import Router


class TestResolve:
    def test_returns_group_key_and_entry(self, single_model_config):
        router = Router(single_model_config)
        result = router.resolve("test-model")
        assert result is not None
        group, key, entry = result
        assert group.provider_slug == "openai"
        assert key.api_key == "sk-test-key-1"
        assert entry.model_name == "test-model"

    def test_returns_none_for_unknown_model(self, single_model_config):
        router = Router(single_model_config)
        assert router.resolve("nonexistent-model") is None

    def test_returns_model_names(self, single_model_config):
        router = Router(single_model_config)
        assert router.get_model_names() == ["test-model"]

    def test_picks_next_key_on_cooldown(self, multi_key_config):
        router = Router(multi_key_config)
        result1 = router.resolve("test-model")
        assert result1 is not None
        group, key1, _ = result1

        # Cool down the returned key
        router.record_failure(group, key1, 429)
        assert key1.cooldown_until > 0

        # Second resolve should return the other key
        result2 = router.resolve("test-model")
        assert result2 is not None
        _, key2, _ = result2
        assert key2.api_key != key1.api_key

        # The cooled key should NOT be returned again
        group_keys = [k.api_key for k in group.keys]
        assert key1.api_key in group_keys
        assert key2.api_key in group_keys

    def test_returns_none_when_all_keys_cooled(self, multi_key_config):
        router = Router(multi_key_config)

        result1 = router.resolve("test-model")
        router.record_failure(result1[0], result1[1], 429)

        result2 = router.resolve("test-model")
        router.record_failure(result2[0], result2[1], 429)

        result3 = router.resolve("test-model")
        assert result3 is None

    def test_picks_other_group_when_circuit_open(self, dual_group_config):
        router = Router(dual_group_config)

        # Get the first group, fail it 3 times to open circuit
        for _ in range(3):
            result = router.resolve("test-model")
            assert result is not None
            group, _, _ = result
            router.record_failure(group, result[1], 502)

        result = router.resolve("test-model")
        assert result is not None
        group, key, _ = result
        assert group.provider_slug == "groq"
        assert key.api_key == "gsk-gr-key"

    def test_circuit_breaker_recovers_after_timeout(self, dual_group_config):
        router = Router(dual_group_config)
        cb = None

        for _ in range(3):
            result = router.resolve("test-model")
            assert result is not None
            group, key, _ = result
            cb = group.circuit_breaker
            if group.provider_slug == "openai":
                router.record_failure(group, key, 502)

        assert cb.state == "OPEN"

        # Simulate recovery timeout
        cb.opened_at = time.monotonic() - 31
        assert cb.is_request_allowed() is True
        assert cb.state == "HALF_OPEN"

        # Success should close it
        cb.record_success()
        assert cb.state == "CLOSED"
        assert cb.error_count == 0
