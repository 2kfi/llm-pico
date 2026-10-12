from __future__ import annotations

import time

import pytest

from core.router import Router, ProviderGroup, KeyState


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

        with pytest.raises(Exception, match="429"):
            router.resolve("test-model")

    def test_picks_other_group_when_circuit_open(self, dual_group_config):
        router = Router(dual_group_config)
        # Force openai circuit open directly (weighted random may pick either group)
        oa_group = [g for g in router._model_map["test-model"] if g.provider_slug == "openai"][0]
        for _ in range(3):
            oa_group.circuit_breaker.record_failure()
        assert oa_group.circuit_breaker.state == "OPEN"

        result = router.resolve("test-model")
        assert result is not None
        group, key, _ = result
        assert group.provider_slug == "groq"
        assert key.api_key == "gsk-gr-key"

    def test_circuit_breaker_recovers_after_timeout(self, dual_group_config):
        router = Router(dual_group_config)
        oa_group = [g for g in router._model_map["test-model"] if g.provider_slug == "openai"][0]
        cb = oa_group.circuit_breaker

        for _ in range(3):
            cb.record_failure()

        assert cb.state == "OPEN"

        # Simulate recovery timeout
        cb.opened_at = time.monotonic() - 31
        assert cb.is_request_allowed() is True
        assert cb.state == "HALF_OPEN"

        # Success should close it
        cb.record_success()
        assert cb.state == "CLOSED"
        assert cb.error_count == 0


class TestLatencyTracking:
    def test_record_latency_computes_percentiles(self):
        group = ProviderGroup(provider_slug="openai")
        for ms in range(10, 110):  # 10ms to 109ms
            group.record_latency(float(ms))
        assert group.latency_p50 == pytest.approx(59.5)
        assert group.latency_p95 == pytest.approx(105.0)

    def test_latency_samples_capped_at_100(self):
        group = ProviderGroup(provider_slug="openai")
        for i in range(150):
            group.record_latency(float(i))
        assert len(group.latency_samples) == 100
        assert group.latency_samples[0] == 50.0  # oldest kept is index 50

    def test_single_sample(self):
        group = ProviderGroup(provider_slug="openai")
        group.record_latency(42.0)
        assert group.latency_p50 == 42.0
        assert group.latency_p95 == 42.0


class TestCostAwareRouting:
    def test_filters_by_max_cost(self, dual_group_config):
        router = Router(dual_group_config)
        # Set different costs on the two groups
        for g in router._model_map["test-model"]:
            if g.provider_slug == "openai":
                g.cost_per_1k = 30.0
            else:
                g.cost_per_1k = 0.5

        # max_cost=1.0 should only allow the cheap group
        result = router.resolve("test-model", max_cost=1.0)
        assert result is not None
        assert result[0].provider_slug == "groq"

    def test_no_max_cost_returns_any_group(self, dual_group_config):
        router = Router(dual_group_config)
        for g in router._model_map["test-model"]:
            g.cost_per_1k = 999.0
        result = router.resolve("test-model", max_cost=None)
        assert result is not None

    def test_all_groups_over_cost_returns_none(self, dual_group_config):
        router = Router(dual_group_config)
        for g in router._model_map["test-model"]:
            g.cost_per_1k = 50.0
        result = router.resolve("test-model", max_cost=1.0)
        assert result is None


class TestWeightedRoundRobin:
    def test_fewer_failures_gets_higher_weight(self):
        group = ProviderGroup(provider_slug="openai")
        k1 = KeyState(api_key="key-1", fails=0)
        k2 = KeyState(api_key="key-2", fails=10)
        group.keys = [k1, k2]

        # With weight formula 1/(1+fails*0.1): k1=1.0, k2=0.5
        # k1 should be chosen ~2x more often
        counts = {"key-1": 0, "key-2": 0}
        for _ in range(1000):
            w1 = 1.0 / (1.0 + k1.fails * 0.1)
            w2 = 1.0 / (1.0 + k2.fails * 0.1)
            import random as _r
            chosen = _r.choices(["key-1", "key-2"], weights=[w1, w2], k=1)[0]
            counts[chosen] += 1
        assert counts["key-1"] > counts["key-2"]

    def test_resolve_returns_key_with_fewer_fails_more_often(self, multi_key_config):
        router = Router(multi_key_config)
        result = router.resolve("test-model")
        group, first_key, _ = result
        # Give the first key many failures
        for _ in range(20):
            router.record_failure(group, first_key, 429)

        counts = {first_key.api_key: 0}
        other_key = [k for k in group.keys if k is not first_key][0]
        counts[other_key.api_key] = 0

        # Resolve many times — the low-fail key should dominate
        for _ in range(200):
            r = router.resolve("test-model")
            if r is not None:
                counts[r[1].api_key] += 1

        assert counts[other_key.api_key] > counts[first_key.api_key]
