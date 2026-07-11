from __future__ import annotations

import asyncio
import calendar
import logging
import time
from collections import defaultdict
from typing import Any

from core import db as db_module

_log = logging.getLogger("llm-pico.ratelimit")

_WINDOW_TYPES = ("rpm", "rpd", "tpm", "tpd", "ash", "asd")

_InMemCounter = dict[str, Any]


class RateLimiter:
    def __init__(self, shard_count: int = 4, flush_interval: int = 60) -> None:
        self._mem: dict[tuple[str, str, str, str], _InMemCounter] = {}
        self._locks: dict[int, asyncio.Lock] = {i: asyncio.Lock() for i in range(shard_count)}
        self._shard_count = shard_count
        self._dirty: set[tuple[str, str, str, str]] = set()
        self._flush_task: asyncio.Task | None = None
        self._running = False
        self._flush_interval = flush_interval

    def _shard_lock(self, key_hash: str, model_name: str) -> asyncio.Lock:
        shard = hash(key_hash + "::" + model_name) % self._shard_count
        return self._locks[shard]

    def _window_start(self, window_type: str, now: float | None = None) -> str:
        ts = now if now is not None else time.time()
        if window_type in ("rpm", "tpm", "ash"):
            return time.strftime("%Y-%m-%dT%H:%M:00", time.gmtime(ts))
        return time.strftime("%Y-%m-%d", time.gmtime(ts))

    def _window_end(self, window_type: str, now: float | None = None) -> int:
        """Return Unix timestamp when the current window ends."""
        ts = now if now is not None else time.time()
        if window_type in ("rpm", "tpm"):
            # next minute boundary
            return int(ts) + 60 - int(ts) % 60
        if window_type == "ash":
            # next hour boundary
            return int(ts) + 3600 - int(ts) % 3600
        # daily windows: next midnight UTC
        utc = time.gmtime(ts)
        tomorrow = time.struct_time((utc.tm_year, utc.tm_mon, utc.tm_mday + 1, 0, 0, 0, 0, 0, 0))
        return int(calendar.timegm(tomorrow))

    def _mem_key(self, key_hash: str, model_name: str, level: str, window_type: str) -> tuple[str, str, str, str]:
        return (key_hash, model_name, level, window_type)

    async def _load_from_db(self, key_hash: str, model_name: str, level: str, window_type: str) -> _InMemCounter:
        ws = self._window_start(window_type)
        async with db_module.get_db() as db:
            cursor = await db.execute(
                "SELECT count FROM rate_counters WHERE key_hash=? AND model_name=? AND level=? AND window_type=? AND window_start=?",
                (key_hash, model_name, level, window_type, ws),
            )
            row = await cursor.fetchone()
            count = row["count"] if row else 0
        return {"window_start": ws, "count": count, "dirty": False}

    async def check_and_reserve(
        self,
        key_hash: str,
        model_name: str,
        limits: dict[str, int | None],
        reservation: int = 0,
    ) -> dict[str, Any] | None:
        now = time.time()
        for window_type in ("rpm", "tpm", "ash", "rpd", "tpd", "asd"):
            limit = limits.get(window_type)
            if limit is None:
                continue

            # Count-based windows (RPM/RPD/ASH/ASD) reserve 1 per request.
            # Token-based windows (TPM/TPD) reserve prompt+max_tokens.
            reserve_amount = 1 if window_type in ("rpm", "rpd", "ash", "asd") else reservation

            level = limits.get("_level", "user")
            ws = self._window_start(window_type, now)
            async with self._shard_lock(key_hash, model_name):
                mk = self._mem_key(key_hash, model_name, level, window_type)
                entry = self._mem.get(mk)
                if entry is None or entry["window_start"] != ws:
                    if entry is None and window_type in ("rpd", "tpd", "asd"):
                        entry = await self._load_from_db(key_hash, model_name, level, window_type)
                    else:
                        entry = {"window_start": ws, "count": 0, "dirty": False}
                    self._mem[mk] = entry

                check_count = entry["count"] + reserve_amount
                if check_count > limit:
                    if window_type in ("rpm", "ash"):
                        retry = 60 if window_type == "rpm" else 3600
                    else:
                        retry = 86400
                    return {
                        "exceeded": window_type,
                        "limit": limit,
                        "count": entry["count"],
                        "retry_after": retry,
                    }
                entry["count"] += reserve_amount
                entry["dirty"] = True
                self._dirty.add(self._mem_key(key_hash, model_name, level, window_type))

        return None

    async def get_usage(
        self,
        key_hash: str,
        model_name: str,
        level: str,
        window_type: str,
    ) -> int | None:
        """Return current count for the active window, or None if no entry exists."""
        ws = self._window_start(window_type)
        mk = self._mem_key(key_hash, model_name, level, window_type)
        entry = self._mem.get(mk)
        if entry is None or entry["window_start"] != ws:
            return 0
        return entry["count"]

    async def reconcile(
        self,
        key_hash: str,
        model_name: str,
        limits: dict[str, int | None],
        actual_tokens: int,
        reserved_tokens: int,
    ) -> None:
        delta = actual_tokens - reserved_tokens
        if delta == 0:
            return

        now = time.time()
        for window_type in ("tpm", "tpd"):
            level = limits.get("_level", "user")
            ws = self._window_start(window_type, now)
            async with self._shard_lock(key_hash, model_name):
                entry = self._mem.get(self._mem_key(key_hash, model_name, level, window_type))
                if entry is None or entry["window_start"] != ws:
                    entry = {"window_start": ws, "count": 0, "dirty": False}
                    self._mem[self._mem_key(key_hash, model_name, level, window_type)] = entry
                entry["count"] += delta
                entry["dirty"] = True
                self._dirty.add(self._mem_key(key_hash, model_name, level, window_type))

    async def _flush_dirty(self) -> None:
        if not self._dirty:
            return

        to_flush = list(self._dirty)
        self._dirty.clear()

        async with db_module.get_db() as db:
            await db.execute("BEGIN")
            for mk in to_flush:
                # Only persist daily windows (rpd/tpd/asd); minutely/hourly stay in-memory
                if mk[3] not in ("rpd", "tpd", "asd"):
                    continue
                entry = self._mem.get(mk)
                if entry is None or not entry["dirty"]:
                    continue
                key_hash, model_name, level, window_type = mk
                await db.execute(
                    """INSERT INTO rate_counters (key_hash, model_name, level, window_type, window_start, count)
                       VALUES (?, ?, ?, ?, ?, ?)
                       ON CONFLICT(key_hash, model_name, level, window_type, window_start)
                       DO UPDATE SET count = ?""",
                    (key_hash, model_name, level, window_type, entry["window_start"], entry["count"], entry["count"]),
                )
                entry["dirty"] = False
            await db.commit()

    async def _purge_stale_entries(self) -> None:
        now = time.time()
        stale_keys = []
        for mk, entry in self._mem.items():
            _, _, _, window_type = mk
            current_ws = self._window_start(window_type, now)
            if entry["window_start"] != current_ws:
                stale_keys.append(mk)
        for mk in stale_keys:
            if mk in self._dirty:
                continue
            del self._mem[mk]

    async def _flush_loop(self) -> None:
        flush_count = 0
        while self._running:
            await asyncio.sleep(self._flush_interval)
            try:
                await self._flush_dirty()
                flush_count += 1
                if flush_count % 10 == 0:
                    await self._purge_stale_entries()
            except Exception:
                _log.exception("flush error")

    def start(self) -> None:
        self._running = True
        self._flush_task = asyncio.create_task(self._flush_loop())

    async def stop(self) -> None:
        self._running = False
        if self._flush_task:
            self._flush_task.cancel()
            try:
                await self._flush_task
            except asyncio.CancelledError:
                pass
        await self._flush_dirty()


_global_limiter: RateLimiter | None = None


def get_limiter() -> RateLimiter:
    global _global_limiter
    if _global_limiter is None:
        _global_limiter = RateLimiter()
    return _global_limiter
