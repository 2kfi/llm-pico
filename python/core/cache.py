from __future__ import annotations

import time
from collections import OrderedDict
from hashlib import sha256


class MemoryCache:
    def __init__(self, max_size: int = 256, default_ttl: int = 3600):
        self._max_size = max_size
        self._default_ttl = default_ttl
        self._store: OrderedDict[str, tuple[float, bytes, str]] = OrderedDict()

    def _make_key(self, body: bytes) -> str:
        return sha256(body).hexdigest()

    async def get(self, body: bytes) -> tuple[bytes, str] | None:
        key = self._make_key(body)
        entry = self._store.get(key)
        if entry is None:
            return None
        expires_at, data, content_type = entry
        if time.time() > expires_at:
            del self._store[key]
            return None
        self._store.move_to_end(key)
        return data, content_type

    async def set(self, body: bytes, response: bytes, content_type: str = "application/json", ttl: int | None = None) -> None:
        key = self._make_key(body)
        expires_at = time.time() + (ttl if ttl is not None else self._default_ttl)
        self._store[key] = (expires_at, response, content_type)
        self._store.move_to_end(key)
        if len(self._store) > self._max_size:
            self._store.popitem(last=False)

    async def clear(self) -> None:
        self._store.clear()

    async def clear_expired(self) -> int:
        now = time.time()
        expired = [k for k, (exp, _, _) in self._store.items() if now > exp]
        for k in expired:
            del self._store[k]
        return len(expired)


_cache = MemoryCache()


async def get_cached(body: bytes) -> tuple[bytes, str] | None:
    return await _cache.get(body)


async def set_cached(body: bytes, response: bytes, content_type: str = "application/json", ttl: int | None = None) -> None:
    await _cache.set(body, response, content_type, ttl)


async def clear_cache() -> None:
    await _cache.clear()


async def clear_expired() -> int:
    return await _cache.clear_expired()
