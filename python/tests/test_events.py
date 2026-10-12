from __future__ import annotations

import asyncio
import json

from core.events import emit, subscribe, unsubscribe


def test_emit_received():
    q = subscribe()
    emit({"key": "value"})
    result = q.get_nowait()
    assert json.loads(result) == {"key": "value"}
    unsubscribe(q)


def test_multiple_subscribers():
    q1 = subscribe()
    q2 = subscribe()
    emit({"n": 42})
    r1 = json.loads(q1.get_nowait())
    r2 = json.loads(q2.get_nowait())
    assert r1 == {"n": 42}
    assert r2 == {"n": 42}
    unsubscribe(q1)
    unsubscribe(q2)


def test_slow_consumer_drops_oldest():
    q = subscribe()
    maxsize = q.maxsize

    for i in range(maxsize + 10):
        emit({"i": i})

    count = 0
    while True:
        try:
            q.get_nowait()
            count += 1
        except asyncio.QueueEmpty:
            break

    assert count == maxsize
    unsubscribe(q)
