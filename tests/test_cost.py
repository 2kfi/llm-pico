from __future__ import annotations

from core.usage import compute_cost


def test_cost_both_rates():
    cost = compute_cost(100, 200, 10.0, 20.0)
    expected = (100 / 1_000_000 * 10.0) + (200 / 1_000_000 * 20.0)
    assert cost == expected


def test_cost_blended_output_only():
    cost = compute_cost(100, 200, None, 15.0)
    expected = 200 / 1_000_000 * 15.0
    assert cost == expected


def test_cost_blended_input_only():
    cost = compute_cost(100, 200, 15.0, None)
    expected = 100 / 1_000_000 * 15.0
    assert cost == expected


def test_cost_no_rates():
    cost = compute_cost(100, 200, None, None)
    assert cost is None


def test_cost_zero_tokens():
    cost = compute_cost(0, 0, 10.0, 20.0)
    assert cost == 0.0
