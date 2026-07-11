from __future__ import annotations

import asyncio
import logging
import time
from collections import defaultdict
from typing import Any, Literal

from core import db as db_module

_log = logging.getLogger("llm-pico.ratelimit")

_WINDOW_TYPES = ("rpm", "rpd", "tpm", "tpd", "ash", "asd")

_InMemCounter = dict[str, Any]


class RateLimiter:
    def __init__(self) -> None:
        self._mem: dict[tuple[str, str, str, str], _InMemCounter] = {}
        self._locks: dict[int, asyncio.Lock] = defaultdict(asyncio.Lock)
        self._dirty: set[tuple[str, str, str, str]] = set()
        self._flush_task: asyncio.Task | None = None
        self._running = False

    def _shard_lock(self, key_hash: str, model_name: str) -> asyncio.Lock:
        shard = hash(key_hash + "::" + model_name) % 64
        return self._locks[shard]

    def _window_start(self, window_type: str) -> str:
        now = time.time()
        if window_type in ("rpm", "tpm", "ash"):
            return time.strftime("%Y-%m-%dT%H:%M:00", time.gmtime(now))
        return time.strftime("%Y-%m-%d", time.gmtime(now))

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
        entry: _InMemCounter = {"window_start": ws, "count": count, "dirty": False}
        mk = self._mem_key(key_hash, model_name, level, window_type)
        self._mem[mk] = entry
        return entry

    async def check_and_reserve(
        self,
        key_hash: str,
        model_name: str,
        limits: dict[str, int | None],
        reservation: int = 0,
    ) -> dict[str, Any] | None:
        for window_type in ("rpm", "tpm", "ash", "rpd", "tpd", "asd"):
            limit = limits.get(window_type)
            if limit is None:
                continue

            level = limits.get("_level", "user")
            async with self._shard_lock(key_hash, model_name):
                if window_type in ("rpm", "tpm", "ash"):
                    entry = self._mem.get(self._mem_key(key_hash, model_name, level, window_type))
                    if entry is None or entry["window_start"] != self._window_start(window_type):
                        entry = {"window_start": self._window_start(window_type), "count": 0, "dirty": False}
                        self._mem[self._mem_key(key_hash, model_name, level, window_type)] = entry

                    check_count = entry["count"] + reservation
                    if check_count > limit:
                        retry = 60 if window_type == "rpm" else 3600 if window_type == "ash" else 60
                        return {
                            "exceeded": window_type,
                            "limit": limit,
                            "count": entry["count"],
                            "retry_after": retry,
                        }
                    entry["count"] += reservation
                    entry["dirty"] = True
                    self._dirty.add(self._mem_key(key_hash, model_name, level, window_type))

                else:
                    ws = self._window_start(window_type)
                    async with db_module.get_db() as db:
                        cursor = await db.execute(
                            "SELECT count FROM rate_counters WHERE key_hash=? AND model_name=? AND level=? AND window_type=? AND window_start=?",
                            (key_hash, model_name, level, window_type, ws),
                        )
                        row = await cursor.fetchone()
                        current_count = row["count"] if row else 0

                        check_count = current_count + reservation
                        if check_count > limit:
                            return {
                                "exceeded": window_type,
                                "limit": limit,
                                "count": current_count,
                                "retry_after": 86400,
                            }

                        await db.execute(
                            """INSERT INTO rate_counters (key_hash, model_name, level, window_type, window_start, count)
                               VALUES (?, ?, ?, ?, ?, ?)
                               ON CONFLICT(key_hash, model_name, level, window_type, window_start)
                               DO UPDATE SET count = count + ?""",
                            (key_hash, model_name, level, window_type, ws, reservation, reservation),
                        )
                        await db.commit()

        return None

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

        for window_type in ("tpm", "tpd"):
            level = limits.get("_level", "user")
            async with self._shard_lock(key_hash, model_name):
                if window_type == "tpm":
                    entry = self._mem.get(self._mem_key(key_hash, model_name, level, "tpm"))
                    if entry and entry["window_start"] == self._window_start("tpm"):
                        entry["count"] += delta
                        entry["dirty"] = True
                        self._dirty.add(self._mem_key(key_hash, model_name, level, "tpm"))
                else:
                    ws = self._window_start("tpd")
                    async with db_module.get_db() as db:
                        await db.execute(
                            """INSERT INTO rate_counters (key_hash, model_name, level, window_type, window_start, count)
                               VALUES (?, ?, ?, ?, ?, ?)
                               ON CONFLICT(key_hash, model_name, level, window_type, window_start)
                               DO UPDATE SET count = count + ?""",
                            (key_hash, model_name, level, "tpd", ws, delta, delta),
                        )
                        await db.commit()

    async def _flush_dirty(self) -> None:
        if not self._dirty:
            return

        to_flush = list(self._dirty)
        self._dirty.clear()

        async with db_module.get_db() as db:
            for mk in to_flush:
                entry = self._mem.get(mk)
                if entry is None or not entry["dirty"]:
                    continue
                key_hash, model_name, level, window_type = mk
                # Only persist RPD/TPD/ASD to SQLite; RPM/TPM/ASH are in-memory only
                if window_type not in ("rpd", "tpd", "asd"):
                    entry["dirty"] = False
                    continue
                await db.execute(
                    """INSERT INTO rate_counters (key_hash, model_name, level, window_type, window_start, count)
                       VALUES (?, ?, ?, ?, ?, ?)
                       ON CONFLICT(key_hash, model_name, level, window_type, window_start)
                       DO UPDATE SET count = ?""",
                    (key_hash, model_name, level, window_type, entry["window_start"], entry["count"], entry["count"]),
                )
                entry["dirty"] = False
            await db.commit()

    async def _flush_loop(self) -> None:
        while self._running:
            await asyncio.sleep(10)
            try:
                await self._flush_dirty()
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
