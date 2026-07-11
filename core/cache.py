from __future__ import annotations

import hashlib
import logging
import time

from core.db import get_db

_log = logging.getLogger("llm-pico.cache")

CACHE_TTL = 3600


def make_cache_key(body_bytes: bytes) -> str:
    return hashlib.sha256(body_bytes).hexdigest()


async def get_cached(cache_key: str) -> tuple[bytes, str] | None:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        cursor = await db.execute(
            "SELECT response_body, content_type FROM request_cache WHERE cache_key = ? AND expires_at > ?",
            (cache_key, now),
        )
        row = await cursor.fetchone()
    if row is None:
        return None
    return row["response_body"], row["content_type"]


async def set_cached(cache_key: str, response_body: bytes, content_type: str = "application/json", ttl: int = CACHE_TTL) -> None:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    expires_at = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(time.time() + ttl))
    async with get_db() as db:
        await db.execute(
            """INSERT OR REPLACE INTO request_cache
               (cache_key, response_body, content_type, created_at, expires_at)
               VALUES (?, ?, ?, ?, ?)""",
            (cache_key, response_body, content_type, now, expires_at),
        )
        await db.commit()
    _log.debug("cached response %s (ttl=%ds)", cache_key[:12], ttl)


async def clear_expired() -> int:
    now = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    async with get_db() as db:
        cursor = await db.execute("DELETE FROM request_cache WHERE expires_at <= ?", (now,))
        await db.commit()
        return cursor.rowcount


async def clear_cache() -> int:
    async with get_db() as db:
        cursor = await db.execute("DELETE FROM request_cache")
        await db.commit()
        return cursor.rowcount
